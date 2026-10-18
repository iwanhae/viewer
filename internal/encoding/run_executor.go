package encoding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"io"
	"os"
	"os/exec"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/runs"
)

const localEncodeTimeout = 5 * time.Minute
const localCompletionTimeout = 2*LeaseTTL + time.Minute
const maxSourcePixels = 100_000_000

// EncodingExecutor performs exact-target local work, not polling. It deliberately
// is not registered at startup. Service.Start must run with the service lifetime
// before use, so accepted output survives run cancellation and process restart.
// cwebp_unavailable means install libwebp's cwebp executable on the server PATH;
// local execution never needs WORKER_TOKEN or presigned URLs.
type EncodingExecutor struct {
	service           *Service
	runCommand        func(context.Context, string, []string, io.Writer) error
	completionTimeout time.Duration
}

func NewEncodingExecutor(service *Service) *EncodingExecutor {
	return &EncodingExecutor{service: service, runCommand: runEncoderCommand, completionTimeout: localCompletionTimeout}
}

var _ runs.Executor = (*EncodingExecutor)(nil)

func runEncoderCommand(ctx context.Context, name string, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = output
	// Do not capture cwebp diagnostics: they can contain private paths/metadata,
	// and retaining arbitrary stderr would also bypass our memory bound.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func (e *EncodingExecutor) Execute(ctx context.Context, run catalog.JobRun, item catalog.JobRunItem) (outcome runs.Outcome, err error) {
	failed := runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	if e == nil || e.service == nil || run.Stage != "encoding" || !validJobIdentity(item.Hash, item.ClaimToken) {
		return failed, nil
	}
	s := e.service
	if s.cat == nil || s.store == nil || e.runCommand == nil {
		return failed, nil
	}
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return failed, nil
	}
	job, status, claimErr := s.cat.ClaimLocalEncoding(ctx, item.Hash, item.ClaimToken, time.Now().Add(LeaseTTL))
	if claimErr != nil {
		if ctx.Err() != nil {
			return runs.Outcome{Status: "cancelled"}, nil
		}
		return failed, nil
	}
	switch status {
	case catalog.LocalEncodingBusy:
		return runs.Outcome{Status: "busy", ErrorCode: "target_busy"}, nil
	case catalog.LocalEncodingMissing:
		return runs.Outcome{Status: "skipped", ErrorCode: "target_missing"}, nil
	}
	defer func() {
		if recover() != nil {
			outcome = runs.Outcome{Status: "failed", ErrorCode: "executor_panic"}
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// This is a no-op after handoff, terminal completion, or ownership loss.
		if releaseErr := s.cat.ReleaseLocalEncoding(cleanup, job.Hash, job.Token); releaseErr != nil && outcome.Status != "succeeded" {
			outcome = failed
		}
	}()
	work, cancel := context.WithTimeout(ctx, localEncodeTimeout)
	defer cancel()
	terminal := func(status, contentType, code string, size int64) runs.Outcome {
		if ctx.Err() != nil {
			return runs.Outcome{Status: "cancelled"}
		}
		ok, err := s.cat.SkipEncoding(work, job.Hash, job.Token, status, contentType, code, size)
		if err != nil {
			if ctx.Err() != nil {
				return runs.Outcome{Status: "cancelled"}
			}
			return failed
		}
		if !ok {
			return runs.Outcome{Status: "busy", ErrorCode: "claim_lost"}
		}
		if status == "failed" {
			return runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}
		}
		return runs.Outcome{Status: "skipped"}
	}
	if job.SizeBytes <= 0 || job.SizeBytes > MaxOutputBytes {
		return terminal("failed", job.Type, "invalid source blob for encoding", job.SizeBytes), nil
	}
	source, createErr := os.CreateTemp(s.spoolDir, "encoding-local-source-*.part")
	if createErr != nil {
		return failed, nil
	}
	defer os.Remove(source.Name())
	defer source.Close()
	reader, _, loadErr := s.store.GetObject(work, pipeline.BlobKey(job.Hash))
	if loadErr != nil {
		if ctx.Err() != nil {
			return runs.Outcome{Status: "cancelled"}, nil
		}
		return failed, nil // storage outages are retryable, not invalid input
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(source, hash), io.LimitReader(&contextReader{work, reader}, job.SizeBytes+1))
	_ = reader.Close()
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	if copyErr != nil {
		return failed, nil
	}
	if n != job.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != job.Hash {
		return terminal("failed", job.Type, "invalid source blob for encoding", job.SizeBytes), nil
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return failed, nil
	}
	var header [12]byte
	_, _ = io.ReadFull(source, header[:])
	if isWebP(header[:]) {
		return terminal("skipped", "image/webp", "", n), nil
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return failed, nil
	}
	config, _, decodeErr := image.DecodeConfig(source)
	if decodeErr != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width) > maxSourcePixels/int64(config.Height) {
		return terminal("failed", job.Type, "invalid source blob for encoding", n), nil
	}
	encoded, createErr := os.CreateTemp(s.spoolDir, "encoding-local-output-*.part")
	if createErr != nil {
		return failed, nil
	}
	defer os.Remove(encoded.Name())
	defer encoded.Close()
	// cwebp's '-' output streams directly to a strictly bounded private file.
	// A path output would let the child grow a file without an enforceable bound.
	commandCtx, stopCommand := context.WithCancel(work)
	output := &boundedEncoderOutput{file: encoded, remaining: min(n, MaxOutputBytes), cancel: stopCommand}
	commandErr := e.runCommand(commandCtx, "cwebp", []string{"-q", "85", "-alpha_q", "100", "-metadata", "all", source.Name(), "-o", "-"}, output)
	stopCommand()
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	if work.Err() != nil {
		return failed, nil // timeout: release only this local lease for retry
	}
	if output.exceeded {
		return terminal("skipped", job.Type, "", n), nil
	}
	if output.writeErr != nil {
		return failed, nil // e.g. a full spool disk: do not fail the blob stage
	}
	if commandErr != nil {
		if errors.Is(commandErr, exec.ErrNotFound) || errors.Is(commandErr, os.ErrNotExist) {
			return runs.Outcome{Status: "failed", ErrorCode: "cwebp_unavailable"}, nil
		}
		var exitErr *exec.ExitError
		if errors.As(commandErr, &exitErr) {
			return terminal("failed", job.Type, "processing_failed", n), nil
		}
		return failed, nil
	}
	info, statErr := encoded.Stat()
	if statErr != nil {
		return failed, nil
	}
	if info.Size() >= n {
		return terminal("skipped", job.Type, "", n), nil
	}
	if _, err := encoded.Seek(0, io.SeekStart); err != nil {
		return failed, nil
	}
	if err := validateWebP(encoded, info.Size(), n, config); err != nil {
		return terminal("failed", job.Type, "invalid encoded output", n), nil
	}
	if work.Err() != nil {
		if ctx.Err() != nil {
			return runs.Outcome{Status: "cancelled"}, nil
		}
		return failed, nil
	}
	receiveErr := s.ReceiveOutput(work, job.Hash, job.Token, &contextReader{work, encoded})
	// Always inspect with the completion context: a cancelled receive may have
	// committed its handoff before losing the acknowledgement. Never abandon it.
	completion, complete := context.WithTimeout(context.WithoutCancel(ctx), e.completionTimeout)
	defer complete()
	current, getErr := s.cat.GetEncodingJob(completion, job.Hash)
	if receiveErr == nil || getErr != nil || (current.Token == job.Token && (current.Status == "received" || current.Status == "committing" || current.Status == "done" || current.Status == "failed" || current.Status == "skipped")) {
		return e.waitForCompletion(completion, job), nil
	}
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	return failed, nil
}

func (e *EncodingExecutor) waitForCompletion(ctx context.Context, job catalog.EncodingJob) runs.Outcome {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := e.service.cat.GetEncodingJob(ctx, job.Hash)
		if errors.Is(err, catalog.ErrBlobNotFound) {
			return runs.Outcome{Status: "skipped", ErrorCode: "target_missing"}
		}
		if err == nil {
			if current.Token != job.Token {
				return runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}
			}
			switch current.Status {
			case "done":
				return runs.Outcome{Status: "succeeded"}
			case "skipped":
				return runs.Outcome{Status: "skipped"}
			case "failed":
				return runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}
			case "pending", "leased":
				return runs.Outcome{Status: "failed", ErrorCode: "claim_lost"}
			}
		}
		select {
		case <-ctx.Done():
			// Accepted output stays durable for the service's normal recovery.
			return runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}
		case <-ticker.C:
		}
	}
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type boundedEncoderOutput struct {
	file      *os.File
	remaining int64
	cancel    context.CancelFunc
	exceeded  bool
	writeErr  error
}

func (w *boundedEncoderOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.exceeded = true
		w.cancel()
		return 0, ErrOutputTooLarge
	}
	n, err := w.file.Write(p)
	w.writeErr = err
	w.remaining -= int64(n)
	return n, err
}
