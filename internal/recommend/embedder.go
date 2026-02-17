package recommend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

type EmbeddingProvider interface {
	Embed(ctx context.Context, imageBytes []byte) ([]float32, string, error)
	Healthcheck(ctx context.Context) error
	Close() error
}

type PythonEmbedder struct {
	command        string
	modelID        string
	device         string
	requestTimeout time.Duration
	restartLimit   int

	reqMu     sync.Mutex
	stateMu   sync.Mutex
	proc      *workerProcess
	started   bool
	restarts  []time.Time
	sequence  uint64
	isClosing bool
}

type embedRequest struct {
	RequestID string `json:"request_id"`
	Op        string `json:"op,omitempty"`
	ImageB64  string `json:"image_b64,omitempty"`
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

type workerProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  *limitedBuffer
	done    chan struct{}
	waitMu  sync.Mutex
	waitErr error
}

func (p *workerProcess) setWaitErr(err error) {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	p.waitErr = err
}

func (p *workerProcess) IsDone() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

type limitedBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newLimitedBuffer(max int) *limitedBuffer {
	if max <= 0 {
		max = 16 << 10
	}
	return &limitedBuffer{max: max}
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	keep := p
	if len(keep) > l.max {
		keep = keep[len(keep)-l.max:]
	}
	combined := append(l.buf, keep...)
	if len(combined) > l.max {
		combined = combined[len(combined)-l.max:]
	}
	l.buf = combined
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}

func NewPythonEmbedder(command string, modelID string, device string, requestTimeout time.Duration, restartLimit int) *PythonEmbedder {
	if requestTimeout <= 0 {
		requestTimeout = 120 * time.Second
	}
	if restartLimit <= 0 {
		restartLimit = 10
	}
	return &PythonEmbedder{
		command:        command,
		modelID:        modelID,
		device:         device,
		requestTimeout: requestTimeout,
		restartLimit:   restartLimit,
	}
}

func (e *PythonEmbedder) Healthcheck(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	out, _, err := e.sendRequest(ctx, embedRequest{
		RequestID: e.nextRequestID(),
		Op:        "ping",
		ModelID:   e.modelID,
		Device:    e.device,
	})
	if err != nil {
		return fmt.Errorf("worker healthcheck failed: %w", err)
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = "worker healthcheck failed"
		}
		return errors.New(out.Error)
	}
	return nil
}

func (e *PythonEmbedder) Embed(ctx context.Context, imageBytes []byte) ([]float32, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx := ctx
	cancel := func() {}
	if e.requestTimeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, e.requestTimeout)
	}
	defer cancel()

	payload := embedRequest{
		RequestID: e.nextRequestID(),
		Op:        "embed",
		ImageB64:  base64.StdEncoding.EncodeToString(imageBytes),
		ModelID:   e.modelID,
		Device:    e.device,
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		out, stderr, err := e.sendRequest(requestCtx, payload)
		if err == nil {
			if !out.OK {
				if out.Error == "" {
					out.Error = "embedding failed"
				}
				return nil, out.Model, errors.New(out.Error)
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err
		}
		if stderr != "" {
			lastErr = fmt.Errorf("%w (stderr=%s)", err, stderr)
		} else {
			lastErr = err
		}
		e.stopProcess()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("embedding failed")
	}
	return nil, "", lastErr
}

func (e *PythonEmbedder) sendRequest(ctx context.Context, payload embedRequest) (embedResponse, string, error) {
	e.reqMu.Lock()
	defer e.reqMu.Unlock()

	proc, err := e.ensureProcess()
	if err != nil {
		return embedResponse{}, "", err
	}

	input, err := json.Marshal(payload)
	if err != nil {
		return embedResponse{}, proc.stderr.String(), fmt.Errorf("encode worker request: %w", err)
	}
	if _, err := proc.stdin.Write(append(input, '\n')); err != nil {
		return embedResponse{}, proc.stderr.String(), fmt.Errorf("write worker request: %w", err)
	}

	respCh := make(chan struct {
		out embedResponse
		err error
	}, 1)
	go func() {
		out, readErr := readEmbedResponse(proc.scanner)
		respCh <- struct {
			out embedResponse
			err error
		}{out: out, err: readErr}
	}()

	select {
	case <-ctx.Done():
		e.stopProcess()
		return embedResponse{}, proc.stderr.String(), ctx.Err()
	case result := <-respCh:
		if result.err != nil {
			return embedResponse{}, proc.stderr.String(), fmt.Errorf("read worker output: %w", result.err)
		}
		if result.out.RequestID != payload.RequestID {
			return embedResponse{}, proc.stderr.String(), fmt.Errorf("worker request id mismatch: got=%s want=%s", result.out.RequestID, payload.RequestID)
		}
		return result.out, proc.stderr.String(), nil
	}
}

func (e *PythonEmbedder) nextRequestID() string {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.sequence++
	return fmt.Sprintf("%d", e.sequence)
}

func (e *PythonEmbedder) ensureProcess() (*workerProcess, error) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e.isClosing {
		return nil, fmt.Errorf("embedder is closed")
	}
	if e.proc != nil && e.proc.IsDone() {
		e.proc = nil
	}
	if e.proc != nil {
		return e.proc, nil
	}

	if e.started && e.restartLimit > 0 {
		now := time.Now()
		cutoff := now.Add(-1 * time.Hour)
		kept := e.restarts[:0]
		for _, t := range e.restarts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		e.restarts = kept
		if len(e.restarts) >= e.restartLimit {
			return nil, fmt.Errorf("worker restart limit reached (%d in last hour)", e.restartLimit)
		}
		e.restarts = append(e.restarts, now)
	}

	proc, err := startWorkerProcess(e.command)
	if err != nil {
		return nil, err
	}
	e.proc = proc
	e.started = true
	return e.proc, nil
}

func (e *PythonEmbedder) stopProcess() {
	e.stateMu.Lock()
	proc := e.proc
	e.proc = nil
	e.stateMu.Unlock()

	if proc == nil {
		return
	}
	_ = proc.stdin.Close()
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
	select {
	case <-proc.done:
	case <-time.After(2 * time.Second):
	}
}

func (e *PythonEmbedder) Close() error {
	e.stateMu.Lock()
	e.isClosing = true
	proc := e.proc
	e.proc = nil
	e.stateMu.Unlock()
	if proc == nil {
		return nil
	}
	_ = proc.stdin.Close()
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
	select {
	case <-proc.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func startWorkerProcess(command string) (*workerProcess, error) {
	cmd := exec.Command("bash", "-lc", command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open worker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open worker stdout: %w", err)
	}
	stderr := newLimitedBuffer(16 << 10)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start worker command: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	proc := &workerProcess{
		cmd:     cmd,
		stdin:   stdin,
		scanner: scanner,
		stderr:  stderr,
		done:    make(chan struct{}),
	}
	go func() {
		waitErr := cmd.Wait()
		proc.setWaitErr(waitErr)
		close(proc.done)
	}()
	return proc, nil
}

func readEmbedResponse(scanner *bufio.Scanner) (embedResponse, error) {
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var out embedResponse
		if err := json.Unmarshal(line, &out); err != nil {
			continue
		}
		return out, nil
	}
	if err := scanner.Err(); err != nil {
		return embedResponse{}, err
	}
	return embedResponse{}, io.EOF
}
