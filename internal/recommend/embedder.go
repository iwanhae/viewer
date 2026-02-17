package recommend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type EmbeddingProvider interface {
	Embed(ctx context.Context, imageBytes []byte) ([]float32, string, error)
}

type PythonEmbedder struct {
	command string
	modelID string
	device  string
}

type embedRequest struct {
	RequestID string `json:"request_id"`
	ImageB64  string `json:"image_b64"`
	ModelID   string `json:"model_id"`
	Device    string `json:"device"`
}

type embedResponse struct {
	RequestID string    `json:"request_id"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	Model     string    `json:"model,omitempty"`
	Embedding []float32 `json:"embedding,omitempty"`
}

func NewPythonEmbedder(command string, modelID string, device string) *PythonEmbedder {
	return &PythonEmbedder{command: command, modelID: modelID, device: device}
}

func (e *PythonEmbedder) Embed(ctx context.Context, imageBytes []byte) ([]float32, string, error) {
	requestID := fmt.Sprintf("%d", time.Now().UnixNano())
	payload := embedRequest{
		RequestID: requestID,
		ImageB64:  base64.StdEncoding.EncodeToString(imageBytes),
		ModelID:   e.modelID,
		Device:    e.device,
	}
	input, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("encode worker request: %w", err)
	}

	cmd := exec.CommandContext(ctx, "bash", "-lc", e.command)
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("worker command failed: %w (stderr=%s)", err, stderr.String())
	}

	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var out embedResponse
		if err := json.Unmarshal(line, &out); err != nil {
			continue
		}
		if out.RequestID != requestID {
			continue
		}
		if !out.OK {
			if out.Error == "" {
				out.Error = "embedding failed"
			}
			return nil, out.Model, fmt.Errorf(out.Error)
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
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("read worker output: %w", err)
	}
	return nil, "", fmt.Errorf("worker produced no parseable response")
}
