package recommend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type EmbeddingProvider interface {
	Embed(ctx context.Context, imageBytes []byte) ([]float32, string, error)
	Healthcheck(ctx context.Context) error
	Close() error
}

type PythonEmbedder struct {
	endpoint       string
	modelID        string
	device         string
	requestTimeout time.Duration

	client        *http.Client
	sequence      uint64
	requestPrefix string
}

type embedRequest struct {
	RequestID string `json:"request_id"`
	Op        string `json:"op,omitempty"`
	ImageB64  string `json:"image_b64,omitempty"`
	ModelID   string `json:"model_id"`
	Device    string `json:"device"`
}

type embedResponse struct {
	RequestID      string    `json:"request_id"`
	OK             bool      `json:"ok"`
	Error          string    `json:"error,omitempty"`
	ErrorStage     string    `json:"error_stage,omitempty"`
	Traceback      string    `json:"traceback,omitempty"`
	Backend        string    `json:"backend,omitempty"`
	Model          string    `json:"model,omitempty"`
	ModelID        string    `json:"model_id,omitempty"`
	Device         string    `json:"device,omitempty"`
	ImageSizeBytes int       `json:"image_size_bytes,omitempty"`
	Embedding      []float32 `json:"embedding,omitempty"`
}

func NewPythonEmbedder(endpoint string, modelID string, device string, requestTimeout time.Duration) *PythonEmbedder {
	if requestTimeout <= 0 {
		requestTimeout = 120 * time.Second
	}
	return &PythonEmbedder{
		endpoint:       normalizeEndpoint(endpoint),
		modelID:        modelID,
		device:         device,
		requestTimeout: requestTimeout,
		client:         &http.Client{},
		requestPrefix:  "request",
	}
}

func (e *PythonEmbedder) Healthcheck(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := contextWithTimeout(ctx, e.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, e.endpoint+"/ping", nil)
	if err != nil {
		return fmt.Errorf("build ping request: %w", err)
	}
	res, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker ping failed: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024))
	if err != nil {
		return fmt.Errorf("read ping response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("worker ping failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(body) == 0 {
		return nil
	}
	var ping embedResponse
	if err := json.Unmarshal(body, &ping); err != nil {
		return fmt.Errorf("decode ping response: %w", err)
	}
	if !ping.OK {
		return fmt.Errorf("worker ping failed: %s", formatWorkerError(ping, string(body)))
	}
	return nil
}

func (e *PythonEmbedder) Embed(ctx context.Context, imageBytes []byte) ([]float32, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := contextWithTimeout(ctx, e.requestTimeout)
	defer cancel()

	payload := embedRequest{
		RequestID: e.nextRequestID(),
		Op:        "embed",
		ImageB64:  base64.StdEncoding.EncodeToString(imageBytes),
		ModelID:   e.modelID,
		Device:    e.device,
	}

	out, body, err := e.sendRequest(requestCtx, "/embed", payload)
	if err != nil {
		return nil, "", err
	}
	if !out.OK {
		if out.Error == "" {
			return nil, out.Model, errors.New("embedding failed")
		}
		return nil, out.Model, errors.New(formatWorkerError(out, body))
	}
	if len(out.Embedding) == 0 {
		return nil, out.Model, fmt.Errorf("worker returned empty embedding")
	}
	modelID := out.Model
	if modelID == "" {
		modelID = e.modelID
	}
	return out.Embedding, modelID, nil
}

func (e *PythonEmbedder) sendRequest(ctx context.Context, path string, payload embedRequest) (embedResponse, string, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return embedResponse{}, "", fmt.Errorf("encode worker request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return embedResponse{}, string(bodyBytes), fmt.Errorf("build worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := e.client.Do(req)
	if err != nil {
		return embedResponse{}, string(bodyBytes), fmt.Errorf("send worker request: %w", err)
	}
	defer res.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(res.Body, 16*1024*1024))
	if err != nil {
		return embedResponse{}, "", fmt.Errorf("read worker response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return embedResponse{}, string(responseBody), fmt.Errorf("worker request failed: status=%d body=%s", res.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var out embedResponse
	if err := json.Unmarshal(responseBody, &out); err != nil {
		return embedResponse{}, string(responseBody), fmt.Errorf("decode worker response: %w", err)
	}
	if out.RequestID != "" && out.RequestID != payload.RequestID {
		return embedResponse{}, string(responseBody), fmt.Errorf("worker request id mismatch: got=%s want=%s", out.RequestID, payload.RequestID)
	}
	return out, string(responseBody), nil
}

func (e *PythonEmbedder) nextRequestID() string {
	next := atomic.AddUint64(&e.sequence, 1)
	return e.requestPrefix + "-" + fmt.Sprintf("%d", next)
}

func (e *PythonEmbedder) Close() error {
	return nil
}

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, d)
}

func normalizeEndpoint(endpoint string) string {
	clean := strings.TrimSpace(endpoint)
	clean = strings.TrimRight(clean, "/")
	return clean
}

func formatWorkerError(out embedResponse, body string) string {
	parts := []string{}
	if out.Error != "" {
		parts = append(parts, out.Error)
	}
	if out.ErrorStage != "" {
		parts = append(parts, "stage="+out.ErrorStage)
	}
	if out.Backend != "" {
		parts = append(parts, "backend="+out.Backend)
	}
	if out.Model != "" {
		parts = append(parts, "model="+out.Model)
	}
	if out.ModelID != "" {
		parts = append(parts, "model_id="+out.ModelID)
	}
	if out.Device != "" {
		parts = append(parts, "device="+out.Device)
	}
	if out.ImageSizeBytes > 0 {
		parts = append(parts, fmt.Sprintf("image_bytes=%d", out.ImageSizeBytes))
	}
	if out.Traceback != "" {
		parts = append(parts, "traceback="+truncateText(out.Traceback, 8<<10))
	}
	if body != "" {
		parts = append(parts, "body="+truncateText(body, 8<<10))
	}
	return strings.Join(parts, " ")
}

func truncateText(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "...(truncated)"
}
