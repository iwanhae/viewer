package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/pipeline"
)

const localEncodeTimeout = 5 * time.Minute
const localCompletionTimeout = 2*LeaseTTL + time.Minute
const maxSourcePixels = 100_000_000

// localEncodeStatus is the terminal state of one locally encoded blob. The
// vocabulary is the one the retired run items persisted; it survives as the
// internal contract between the drain and its logs because every value maps
// to a distinct operator action (retry, ignore, or wait out the shutdown).
type localEncodeStatus string

const (
	localEncodeSucceeded localEncodeStatus = "succeeded"
	localEncodeFailed    localEncodeStatus = "failed"
	localEncodeBusy      localEncodeStatus = "busy"
	localEncodeSkipped   localEncodeStatus = "skipped"
	localEncodeCancelled localEncodeStatus = "cancelled"
)

// localEncodeOutcome replaces the outcome coupling the retired run executor
// had. ErrorCode stays a public machine code (never cwebp stderr or a storage
// error) because the drain writes it into logs that operators read.
type localEncodeOutcome struct {
	Status    localEncodeStatus
	ErrorCode string
}

func runEncoderCommand(ctx context.Context, name string, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = output
	// Do not capture cwebp diagnostics: they can contain private paths/metadata,
	// and retaining arbitrary stderr would also bypass our memory bound.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// StartLocalDrain launches one background pass over every pending non-WebP
// blob, encoding it locally with cwebp. It replaces the manual encoding run:
// the target set is snapshotted once (the same eligibility the run snapshot
// used), which is what makes the drain terminate — blobs uploaded after the
// snapshot wait for the next trigger, exactly as they waited for the next
// manual run before.
//
// Single-flight: if a drain is already running it reports (false, nil) rather
// than forking a second cwebp pipeline over the same queue; the HTTP layer
// surfaces that as "already running", not an error.
//
// It fails fast — before claiming anything — when the service is not started
// or cwebp is missing from PATH, so a deployment without libwebp gets one
// clear error instead of a per-blob cwebp_unavailable failure for every
// target in the snapshot.
func (s *Service) StartLocalDrain() (started bool, err error) {
	if s == nil || s.cat == nil || s.store == nil || s.spoolDir == "" {
		return false, fmt.Errorf("encoding service is not started")
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if !s.started || s.drainCtx == nil {
		return false, fmt.Errorf("encoding service is not started")
	}
	if _, err := exec.LookPath("cwebp"); err != nil {
		return false, fmt.Errorf("cwebp is not on the server PATH; install libwebp's cwebp (Debian: apt-get install webp) to run local encoding")
	}
	if s.drainRunning {
		return false, nil
	}
	// Snapshot inside the single-flight lock: a racing caller must not take
	// its own (now stale) picture of the queue, and the goroutine below must
	// not start while a second snapshot is being read.
	hashes, err := s.cat.PendingLocalEncodingHashes(s.drainCtx)
	if err != nil {
		return false, fmt.Errorf("snapshot pending local encoding targets: %w", err)
	}
	s.drainRunning = true
	s.drainWG.Add(1)
	go func() {
		defer s.drainWG.Done()
		defer func() {
			s.startMu.Lock()
			s.drainRunning = false
			s.startMu.Unlock()
		}()
		// Bound to the drain's own lifetime (a child of the service
		// lifetime): Close cancels it between blobs, while an in-flight
		// blob's uncancellable completion section still finishes under the
		// still-running processors — never abandoning accepted output.
		s.runLocalDrain(s.drainCtx, hashes)
	}()
	return true, nil
}

// runLocalDrain walks the snapshotted hashes once. Per-blob terminal failures
// are logged and the drain continues — one corrupt image must not strand the
// rest of the queue behind it. It stops early when the service lifetime ends,
// leaving the unprocessed targets pending for the next trigger.
func (s *Service) runLocalDrain(ctx context.Context, hashes []string) {
	log.Printf("encoding: local drain started targets=%d", len(hashes))
	for i, hash := range hashes {
		if ctx.Err() != nil {
			log.Printf("encoding: local drain stopped by shutdown after %d/%d target(s)", i, len(hashes))
			return
		}
		token, err := catalog.NewEncodingToken()
		if err != nil {
			// A crypto-random failure is systemic, not per-blob: stop rather
			// than burn the whole snapshot on targets that can never claim.
			log.Printf("encoding: local drain cannot mint a claim token: %v", err)
			return
		}
		job, status, err := s.cat.ClaimLocalEncoding(ctx, hash, token, time.Now().Add(LeaseTTL))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("encoding: local drain claim for %s failed: %v", hash, err)
			continue
		}
		switch status {
		case catalog.LocalEncodingBusy:
			// An external worker or another local claim owns the row. The
			// lease fencing already decided the winner; skipping (rather than
			// retrying) is what keeps the drain finite.
			continue
		case catalog.LocalEncodingMissing:
			continue
		}
		outcome := s.encodeLocalBlob(ctx, job)
		if outcome.Status == localEncodeFailed {
			log.Printf("encoding: local drain target %s failed: %s", hash, outcome.ErrorCode)
		}
	}
	log.Printf("encoding: local drain finished targets=%d", len(hashes))
}

// encodeLocalBlob performs the exact-target local pipeline for one
// already-claimed job: webp-source repair, size/hash verification, decode
// config check, the bounded cwebp invocation, output validation, and the
// ReceiveOutput handoff with its completion wait. It deliberately requires
// Service.Start to have run with the service lifetime before use, so accepted
// output survives drain cancellation and process restart. local execution
// never needs WORKER_TOKEN or presigned URLs.
//
// The body is the battle-tested executor logic kept verbatim from the run
// executor; only the outcome type changed.
func (s *Service) encodeLocalBlob(ctx context.Context, job catalog.EncodingJob) (outcome localEncodeOutcome) {
	failed := localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "executor_failed"}
	if ctx.Err() != nil {
		return localEncodeOutcome{Status: localEncodeCancelled}
	}
	if s == nil || s.cat == nil || s.store == nil || s.runCommand == nil || !validJobIdentity(job.Hash, job.Token) {
		return failed
	}
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return failed
	}
	defer func() {
		if recover() != nil {
			outcome = localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "executor_panic"}
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// This is a no-op after handoff, terminal completion, or ownership loss.
		if releaseErr := s.cat.ReleaseLocalEncoding(cleanup, job.Hash, job.Token); releaseErr != nil && outcome.Status != localEncodeSucceeded {
			outcome = failed
		}
	}()
	work, cancel := context.WithTimeout(ctx, localEncodeTimeout)
	defer cancel()
	terminal := func(status, contentType, code string, size int64) localEncodeOutcome {
		if ctx.Err() != nil {
			return localEncodeOutcome{Status: localEncodeCancelled}
		}
		ok, err := s.cat.SkipEncoding(work, job.Hash, job.Token, status, contentType, code, size)
		if err != nil {
			if ctx.Err() != nil {
				return localEncodeOutcome{Status: localEncodeCancelled}
			}
			return failed
		}
		if !ok {
			return localEncodeOutcome{Status: localEncodeBusy, ErrorCode: "claim_lost"}
		}
		if status == "failed" {
			return localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "processing_failed"}
		}
		return localEncodeOutcome{Status: localEncodeSkipped}
	}
	reader, _, loadErr := s.store.GetObject(work, pipeline.BlobKey(job.Hash))
	if loadErr != nil {
		if ctx.Err() != nil {
			return localEncodeOutcome{Status: localEncodeCancelled}
		}
		return failed // storage outages are retryable, not invalid input
	}
	defer func() {
		if reader != nil {
			_ = reader.Close()
		}
	}()
	input := &contextReader{work, reader}
	var header [12]byte
	headerSize, headerErr := io.ReadFull(input, header[:])
	if ctx.Err() != nil {
		return localEncodeOutcome{Status: localEncodeCancelled}
	}
	if headerErr != nil && !errors.Is(headerErr, io.EOF) && !errors.Is(headerErr, io.ErrUnexpectedEOF) {
		return failed
	}
	// Restored catalogs may describe the original PNG/JPEG even though a prior
	// replacement already put WebP under its stable source hash. Repair that
	// metadata before enforcing original-source size/hash, just like workers do.
	// Count the actual bytes without trusting stale sizes or buffering the body.
	if isWebP(header[:headerSize]) {
		n, err := io.Copy(io.Discard, io.LimitReader(input, MaxOutputBytes-int64(headerSize)+1))
		if ctx.Err() != nil {
			return localEncodeOutcome{Status: localEncodeCancelled}
		}
		if err != nil {
			return failed
		}
		n += int64(headerSize)
		if n > MaxOutputBytes {
			return terminal("failed", job.Type, "invalid source blob for encoding", job.SizeBytes)
		}
		return terminal("skipped", "image/webp", "", n)
	}
	if job.SizeBytes <= 0 || job.SizeBytes > MaxOutputBytes {
		return terminal("failed", job.Type, "invalid source blob for encoding", job.SizeBytes)
	}
	source, createErr := os.CreateTemp(s.spoolDir, "encoding-local-source-*.part")
	if createErr != nil {
		return failed
	}
	defer os.Remove(source.Name())
	defer source.Close()
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(source, hash), io.LimitReader(io.MultiReader(bytes.NewReader(header[:headerSize]), input), job.SizeBytes+1))
	_ = reader.Close()
	reader = nil
	if ctx.Err() != nil {
		return localEncodeOutcome{Status: localEncodeCancelled}
	}
	if copyErr != nil {
		return failed
	}
	if n != job.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != job.Hash {
		return terminal("failed", job.Type, "invalid source blob for encoding", job.SizeBytes)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return failed
	}
	config, _, decodeErr := image.DecodeConfig(source)
	if decodeErr != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width) > maxSourcePixels/int64(config.Height) {
		return terminal("failed", job.Type, "invalid source blob for encoding", n)
	}
	encoded, createErr := os.CreateTemp(s.spoolDir, "encoding-local-output-*.part")
	if createErr != nil {
		return failed
	}
	defer os.Remove(encoded.Name())
	defer encoded.Close()
	// cwebp's '-' output streams directly to a strictly bounded private file.
	// A path output would let the child grow a file without an enforceable bound.
	commandCtx, stopCommand := context.WithCancel(work)
	output := &boundedEncoderOutput{file: encoded, remaining: min(n, MaxOutputBytes), cancel: stopCommand}
	commandErr := s.runCommand(commandCtx, "cwebp", []string{"-q", "85", "-alpha_q", "100", "-metadata", "all", source.Name(), "-o", "-"}, output)
	stopCommand()
	if ctx.Err() != nil {
		return localEncodeOutcome{Status: localEncodeCancelled}
	}
	if work.Err() != nil {
		return failed // timeout: release only this local lease for retry
	}
	if output.exceeded {
		return terminal("skipped", job.Type, "", n)
	}
	if output.writeErr != nil {
		return failed // e.g. a full spool disk: do not fail the blob stage
	}
	if commandErr != nil {
		if errors.Is(commandErr, exec.ErrNotFound) || errors.Is(commandErr, os.ErrNotExist) {
			return localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "cwebp_unavailable"}
		}
		var exitErr *exec.ExitError
		if errors.As(commandErr, &exitErr) {
			return terminal("failed", job.Type, "processing_failed", n)
		}
		return failed
	}
	info, statErr := encoded.Stat()
	if statErr != nil {
		return failed
	}
	if info.Size() >= n {
		return terminal("skipped", job.Type, "", n)
	}
	if _, err := encoded.Seek(0, io.SeekStart); err != nil {
		return failed
	}
	if err := validateWebP(encoded, info.Size(), n, config); err != nil {
		return terminal("failed", job.Type, "invalid encoded output", n)
	}
	if work.Err() != nil {
		if ctx.Err() != nil {
			return localEncodeOutcome{Status: localEncodeCancelled}
		}
		return failed
	}
	receiveErr := s.ReceiveOutput(work, job.Hash, job.Token, &contextReader{work, encoded})
	// Always inspect with the completion context: a cancelled receive may have
	// committed its handoff before losing the acknowledgement. Never abandon it.
	completion, complete := context.WithTimeout(context.WithoutCancel(ctx), s.completionTimeout)
	defer complete()
	current, getErr := s.cat.GetEncodingJob(completion, job.Hash)
	if receiveErr == nil || getErr != nil || (current.Token == job.Token && (current.Status == "received" || current.Status == "committing" || current.Status == "done" || current.Status == "failed" || current.Status == "skipped")) {
		return s.waitForCompletion(completion, job)
	}
	if ctx.Err() != nil {
		return localEncodeOutcome{Status: localEncodeCancelled}
	}
	return failed
}

func (s *Service) waitForCompletion(ctx context.Context, job catalog.EncodingJob) localEncodeOutcome {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := s.cat.GetEncodingJob(ctx, job.Hash)
		if errors.Is(err, catalog.ErrBlobNotFound) {
			return localEncodeOutcome{Status: localEncodeSkipped, ErrorCode: "target_missing"}
		}
		if err == nil {
			if current.Token != job.Token {
				return localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "processing_failed"}
			}
			switch current.Status {
			case "done":
				return localEncodeOutcome{Status: localEncodeSucceeded}
			case "skipped":
				return localEncodeOutcome{Status: localEncodeSkipped}
			case "failed":
				return localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "processing_failed"}
			case "pending", "leased":
				return localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "claim_lost"}
			}
		}
		select {
		case <-ctx.Done():
			// Accepted output stays durable for the service's normal recovery.
			return localEncodeOutcome{Status: localEncodeFailed, ErrorCode: "executor_failed"}
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
