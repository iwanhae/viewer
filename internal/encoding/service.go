// Package encoding coordinates external WebP workers. Legacy workers stage
// output in object storage; API-mode workers stream to a bounded local spool.
// Both paths validate output before replacing the original blob key.
package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
	"viewer/internal/catalog"
	"viewer/internal/config"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

const LeaseTTL = 10 * time.Minute
const DefaultClaim = 8
const MaxClaim = 64

// MaxOutputBytes caps one encoder result. The worker body is streamed to disk;
// this is a hard per-file bound, not an in-memory allocation.
const MaxOutputBytes int64 = 1 << 30

const outputProcessors = 2
const outputQueueSize = outputProcessors
const outputScanInterval = 30 * time.Second
const outputRetryInterval = 30 * time.Second

const spoolFilePattern = "encoding-*.part"

var ErrLostLease = errors.New("encoding lease is no longer current")
var ErrInvalidOutput = errors.New("invalid encoded output")
var ErrInvalidSource = errors.New("invalid source blob for encoding")
var ErrInvalidOutcome = errors.New("invalid encoding outcome")
var ErrOutputTooLarge = errors.New("encoded output exceeds the size limit")
var ErrSpoolBusy = errors.New("encoding spool is at capacity")
var ErrSpoolMissing = errors.New("accepted encoding output is missing from the spool")

type Store interface {
	GetObject(context.Context, string) (io.ReadCloser, string, error)
	PutObject(context.Context, string, io.Reader, string) error
	PresignGet(context.Context, string, time.Duration) (string, error)
	PresignPut(context.Context, string, time.Duration) (string, error)
	StatObject(context.Context, string) (storage.Object, bool, error)
	CopyObjectIfMatch(context.Context, string, string, string, string) error
	DeleteObjects(context.Context, []string) error
	ListObjects(context.Context, string) ([]storage.Object, error)
}

type Service struct {
	cat         *catalog.Store
	store       Store
	commitSlots chan struct{}
	spoolDir    string
	spoolSlots  chan struct{}
	outputQueue chan catalog.EncodingJob

	startMu  sync.Mutex
	started  bool
	queueMu  sync.Mutex
	queued   map[string]struct{}
	uploadMu sync.Mutex
	uploads  map[string]struct{}
}

// New takes a private spool directory. The app places it on the catalog's
// persistent volume so accepted output survives a container restart.
func New(cat *catalog.Store, store Store, spoolDir string) *Service {
	return &Service{
		cat:         cat,
		store:       store,
		commitSlots: make(chan struct{}, outputProcessors),
		spoolDir:    spoolDir,
		spoolSlots:  make(chan struct{}, outputQueueSize),
		outputQueue: make(chan catalog.EncodingJob, outputQueueSize),
		queued:      make(map[string]struct{}),
		uploads:     make(map[string]struct{}),
	}
}

type Claimed struct {
	Hash        string    `json:"hash"`
	SizeBytes   int64     `json:"sizeBytes"`
	ContentType string    `json:"contentType"`
	Token       string    `json:"token"`
	GetURL      string    `json:"getUrl"`
	PutURL      string    `json:"putUrl,omitempty"`
	LeaseUntil  time.Time `json:"leaseUntil"`
}

func (s *Service) Claim(ctx context.Context, limit int) ([]Claimed, error) {
	return s.claim(ctx, limit, true)
}

// ClaimForAPI is the v2 worker protocol: it gives the worker a direct API
// result-upload path, so no presigned S3 PUT is created or returned.
func (s *Service) ClaimForAPI(ctx context.Context, limit int) ([]Claimed, error) {
	return s.claim(ctx, limit, false)
}

func (s *Service) claim(ctx context.Context, limit int, presignOutput bool) ([]Claimed, error) {
	if limit <= 0 {
		limit = DefaultClaim
	} else if limit > MaxClaim {
		limit = MaxClaim
	}
	jobs, err := s.cat.ClaimEncoding(ctx, limit, LeaseTTL)
	if err != nil {
		return nil, err
	}
	claimed := make([]Claimed, 0, len(jobs))
	for _, job := range jobs {
		getURL, getErr := s.store.PresignGet(ctx, pipeline.BlobKey(job.Hash), config.PresignTTL)
		putURL := ""
		var putErr error
		if presignOutput {
			putURL, putErr = s.store.PresignPut(ctx, job.StageKey, config.PresignTTL)
		}
		if getErr != nil || putErr != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for _, release := range jobs {
				_ = s.cat.ReleaseEncoding(releaseCtx, release.Hash, release.Token)
			}
			cancel()
			return nil, fmt.Errorf("prepare encoding job: get=%v put=%v", getErr, putErr)
		}
		claimed = append(claimed, Claimed{job.Hash, job.SizeBytes, job.Type, job.Token, getURL, putURL, job.LeaseUntil})
	}
	return claimed, nil
}

func (s *Service) Renew(ctx context.Context, hash, token string) (time.Time, string, error) {
	until, ok, err := s.cat.RenewEncoding(ctx, hash, token, LeaseTTL)
	if err != nil {
		return time.Time{}, "", err
	}
	if !ok {
		return time.Time{}, "", ErrLostLease
	}
	job, err := s.cat.GetEncodingJob(ctx, hash)
	if err != nil {
		return time.Time{}, "", err
	}
	url, err := s.store.PresignPut(ctx, job.StageKey, config.PresignTTL)
	return until, url, err
}

// RenewLease refreshes an API-upload worker lease without generating a legacy
// presigned S3 PUT URL.
func (s *Service) RenewLease(ctx context.Context, hash, token string) (time.Time, error) {
	until, ok, err := s.cat.RenewEncoding(ctx, hash, token, LeaseTTL)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, ErrLostLease
	}
	return until, nil
}

// Start launches the bounded spool processors and recovers accepted outputs.
// The catalog is reconciled against the private persistent spool before the
// HTTP server begins accepting work.
func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.cat == nil || s.store == nil || s.spoolDir == "" {
		return fmt.Errorf("encoding service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return nil
	}

	if err := os.MkdirAll(s.spoolDir, 0o700); err != nil {
		return fmt.Errorf("create encoding spool %s: %w", s.spoolDir, err)
	}
	info, err := os.Lstat(s.spoolDir)
	if err != nil {
		return fmt.Errorf("inspect encoding spool %s: %w", s.spoolDir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("encoding spool %s must be a directory, not a symlink", s.spoolDir)
	}
	if err := os.Chmod(s.spoolDir, 0o700); err != nil {
		return fmt.Errorf("secure encoding spool %s: %w", s.spoolDir, err)
	}
	if err := s.removePartialSpools(); err != nil {
		return err
	}
	if err := s.removeOrphanSpools(ctx); err != nil {
		return err
	}
	if err := s.reconcileExpiredCommits(ctx); err != nil {
		return err
	}
	if err := s.Cleanup(ctx); err != nil {
		return err
	}
	// Queue recoverable spool files before the processors start. The channel and
	// slot semaphore are bounded, and the periodic scan will fill remaining
	// capacity after the workers drain the initial batch.
	if err := s.scheduleSpoolOutputs(ctx); err != nil {
		return err
	}

	s.started = true
	for i := 0; i < outputProcessors; i++ {
		go s.runOutputProcessor(ctx)
	}
	go s.scanReceivedOutputs(ctx)
	return nil
}

// ReceiveOutput persists a worker's fully uploaded WebP to a private local
// spool, transfers the lease to the server, and queues asynchronous validation.
// A nil return means the server accepted responsibility for this job; it does
// not mean validation or the S3 replacement has completed.
func (s *Service) ReceiveOutput(ctx context.Context, hash, token string, body io.Reader) error {
	if s == nil || s.cat == nil || s.store == nil {
		return fmt.Errorf("encoding service is not initialized")
	}
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return fmt.Errorf("encoding output processor is not started")
	}
	if body == nil || !validJobIdentity(hash, token) {
		return ErrInvalidOutput
	}

	job, err := s.cat.GetEncodingJob(ctx, hash)
	if err != nil {
		return err
	}
	if job.Token != token {
		return ErrLostLease
	}
	if job.Status == "received" || job.Status == "committing" || job.Status == "done" || job.Status == "skipped" || job.Status == "failed" {
		if job.Status == "received" {
			if _, err := os.Stat(s.spoolPath(hash, token)); errors.Is(err, os.ErrNotExist) {
				_ = s.cat.ResetReceivedEncoding(ctx, hash, token)
				return ErrLostLease
			} else if err != nil {
				return fmt.Errorf("inspect accepted encoding output: %w", err)
			}
		}
		// A retry after a lost HTTP response is idempotent. Drain the duplicate
		// request under the same limits rather than replying while its body is
		// still being sent.
		return drainDuplicate(body, job.SizeBytes)
	}
	if job.Status != "leased" || time.Now().After(job.LeaseUntil) {
		return ErrLostLease
	}
	if job.SizeBytes <= 1 {
		return ErrInvalidOutput
	}

	key := jobKey(hash, token)
	s.uploadMu.Lock()
	if _, exists := s.uploads[key]; exists {
		s.uploadMu.Unlock()
		return ErrSpoolBusy
	}
	s.uploads[key] = struct{}{}
	s.uploadMu.Unlock()
	defer func() {
		s.uploadMu.Lock()
		delete(s.uploads, key)
		s.uploadMu.Unlock()
	}()

	// The semaphore bounds both active spool files and queued work. Reject when
	// full rather than holding an unbounded number of HTTP handlers open while
	// they wait for disk capacity; workers can retry while their lease is live.
	select {
	case s.spoolSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrSpoolBusy
	}
	reserved := true
	defer func() {
		if reserved {
			<-s.spoolSlots
		}
	}()

	// Re-read after waiting for capacity: another request may have renewed,
	// completed, or reclaimed this lease while this handler was queued.
	job, err = s.cat.GetEncodingJob(ctx, hash)
	if err != nil {
		return err
	}
	if job.Token != token || job.Status != "leased" || time.Now().After(job.LeaseUntil) {
		return ErrLostLease
	}

	maxBytes := job.SizeBytes - 1 // the encoded result must be strictly smaller
	if maxBytes > MaxOutputBytes {
		maxBytes = MaxOutputBytes
	}
	part, err := os.CreateTemp(s.spoolDir, "encoding-*.part")
	if err != nil {
		return fmt.Errorf("create encoding spool file: %w", err)
	}
	partPath := part.Name()
	keepPart := false
	defer func() {
		if !keepPart {
			_ = os.Remove(partPath)
		}
	}()

	written, copyErr := io.Copy(part, io.LimitReader(body, maxBytes+1))
	if copyErr != nil {
		_ = part.Close()
		return fmt.Errorf("write encoding spool: %w", copyErr)
	}
	if written > maxBytes {
		_ = part.Close()
		if maxBytes == MaxOutputBytes && job.SizeBytes-1 > MaxOutputBytes {
			return ErrOutputTooLarge
		}
		return ErrInvalidOutput
	}
	if written == 0 {
		_ = part.Close()
		return ErrInvalidOutput
	}
	if err := part.Sync(); err != nil {
		_ = part.Close()
		return fmt.Errorf("sync encoding spool: %w", err)
	}
	if err := part.Close(); err != nil {
		return fmt.Errorf("close encoding spool: %w", err)
	}

	finalPath := s.spoolPath(hash, token)
	if err := os.Rename(partPath, finalPath); err != nil {
		return fmt.Errorf("finalize encoding spool: %w", err)
	}
	keepPart = true // rename moved the file; it is removed by the processor.

	accepted, err := s.cat.MarkEncodingReceived(ctx, hash, token)
	if err != nil {
		// Cancellation can race the transaction's commit acknowledgement. Never
		// delete a spool whose handoff actually committed (or is uncertain).
		inspect, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		current, getErr := s.cat.GetEncodingJob(inspect, hash)
		cancel()
		if getErr != nil {
			return fmt.Errorf("record received encoding output: %w", err)
		}
		if current.Token == token && (current.Status == "received" || current.Status == "committing" || current.Status == "done") {
			accepted = true
		} else {
			_ = os.Remove(finalPath)
			return fmt.Errorf("record received encoding output: %w", err)
		}
	}
	if !accepted {
		current, getErr := s.cat.GetEncodingJob(ctx, hash)
		_ = os.Remove(finalPath)
		if getErr == nil && current.Token == token && (current.Status == "received" || current.Status == "committing" || current.Status == "done") {
			return nil // another identical request won the handoff race
		}
		return ErrLostLease
	}

	job.Status = "received"
	job.LeaseUntil = time.UnixMilli(0)
	// The reserved slot guarantees queue capacity. Once accepted, request/run
	// cancellation must not undo the handoff; service shutdown keeps the spool.
	queued, err := s.enqueueReserved(context.WithoutCancel(ctx), job)
	if err != nil {
		return nil // accepted row and spool remain recoverable
	}
	if queued {
		reserved = false // processor releases it after the terminal transition.
	}
	return nil
}

func validJobIdentity(hash, token string) bool {
	if len(hash) != sha256.Size*2 || len(token) != 32 {
		return false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func jobKey(hash, token string) string { return hash + ":" + token }

func drainDuplicate(body io.Reader, originalSize int64) error {
	if originalSize <= 1 {
		return ErrInvalidOutput
	}
	maxBytes := originalSize - 1
	if maxBytes > MaxOutputBytes {
		maxBytes = MaxOutputBytes
	}
	n, err := io.Copy(io.Discard, io.LimitReader(body, maxBytes+1))
	if err != nil {
		return err
	}
	if n > maxBytes {
		if maxBytes == MaxOutputBytes && originalSize-1 > MaxOutputBytes {
			return ErrOutputTooLarge
		}
		return ErrInvalidOutput
	}
	if n == 0 {
		return ErrInvalidOutput
	}
	return nil
}

func (s *Service) spoolPath(hash, token string) string {
	return filepath.Join(s.spoolDir, hash+"_"+token+".webp")
}

func (s *Service) enqueueReserved(ctx context.Context, job catalog.EncodingJob) (bool, error) {
	key := jobKey(job.Hash, job.Token)
	s.queueMu.Lock()
	if _, exists := s.queued[key]; exists {
		s.queueMu.Unlock()
		return false, nil
	}
	s.queued[key] = struct{}{}
	s.queueMu.Unlock()
	select {
	case s.outputQueue <- job:
		return true, nil
	case <-ctx.Done():
		s.queueMu.Lock()
		delete(s.queued, key)
		s.queueMu.Unlock()
		return false, ctx.Err()
	}
}

func (s *Service) scheduleSpoolOutputs(ctx context.Context) error {
	// Only received rows are queued here. Committing rows are deliberately
	// left to reconcileExpiredCommits, which runs first on every scan: their
	// PUT has already returned, so a processor would only observe the
	// not-received state, hit the committing branch, and hand the row
	// straight back — burning a spool slot cycle per scan on a job whose
	// grace lease has not expired yet. Their spool files stay on disk
	// (removeOrphanSpools keeps them) until the reconcile finishes.
	jobs, err := s.cat.ReceivedEncodings(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		path := s.spoolPath(job.Hash, job.Token)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// The received row's result is gone (crash between rename and
				// the DB update, or an operator cleanup); it can never be
				// validated, so hand the blob back to the pending queue.
				if resetErr := s.cat.ResetReceivedEncoding(ctx, job.Hash, job.Token); resetErr != nil {
					return resetErr
				}
				continue
			}
			return fmt.Errorf("stat accepted encoding output: %w", err)
		}

		key := jobKey(job.Hash, job.Token)
		s.queueMu.Lock()
		if _, exists := s.queued[key]; exists {
			s.queueMu.Unlock()
			continue
		}
		select {
		case s.spoolSlots <- struct{}{}:
			s.queued[key] = struct{}{}
			s.queueMu.Unlock()
		default:
			s.queueMu.Unlock()
			return nil // bounded queue is full; the next scan will retry.
		}
		select {
		case s.outputQueue <- job:
		case <-ctx.Done():
			s.queueMu.Lock()
			delete(s.queued, key)
			s.queueMu.Unlock()
			<-s.spoolSlots
			return ctx.Err()
		}
	}
	return nil
}

func (s *Service) scanReceivedOutputs(ctx context.Context) {
	ticker := time.NewTicker(outputScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reconcileExpiredCommits(ctx); err != nil {
				log.Printf("encoding: reconcile expired commits failed: %v", err)
			}
			if err := s.scheduleSpoolOutputs(ctx); err != nil {
				log.Printf("encoding: scan spooled outputs failed: %v", err)
			}
		}
	}
}

func (s *Service) runOutputProcessor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.outputQueue:
			s.processQueuedOutput(ctx, job)
		}
	}
}

func (s *Service) processQueuedOutput(ctx context.Context, job catalog.EncodingJob) {
	key := jobKey(job.Hash, job.Token)
	defer func() {
		s.queueMu.Lock()
		delete(s.queued, key)
		s.queueMu.Unlock()
		<-s.spoolSlots
	}()

	for ctx.Err() == nil {
		err := s.processReceived(ctx, job)
		if ctx.Err() != nil {
			// Keep the received row and its spool file for startup recovery.
			return
		}
		if err == nil {
			_ = os.Remove(s.spoolPath(job.Hash, job.Token))
			return
		}

		current, getErr := s.cat.GetEncodingJob(ctx, job.Hash)
		if getErr != nil {
			if errors.Is(getErr, catalog.ErrBlobNotFound) {
				_ = os.Remove(s.spoolPath(job.Hash, job.Token))
				return
			}
			log.Printf("encoding: read state for %s failed; retrying: %v", job.Hash[:12], getErr)
			if !waitOutputRetry(ctx) {
				return
			}
			continue
		}
		if current.Token != job.Token || current.Status == "done" || current.Status == "skipped" || current.Status == "failed" || current.Status == "pending" {
			_ = os.Remove(s.spoolPath(job.Hash, job.Token))
			return
		}

		if current.Status == "committing" {
			// A PUT with an ambiguous result is fenced by the committing grace
			// lease, and reconcileExpiredCommits — which runs first on every
			// spool scan — owns that reconciliation once no processor holds the
			// row. Waiting for the lease here used to park this processor and
			// its spool slot for the whole grace window, which starved the
			// queue: while every slot sat on a commit waiting to expire, every
			// new worker upload was rejected with ErrSpoolBusy until the
			// oldest lease released — an object store whose conditional PUTs
			// keep failing turned two slots into a ten-minute pipeline stall.
			// Returning now keeps uploads flowing; the spool file simply
			// outlives its slot until the reconcile decides the outcome, so
			// spool disk usage follows how many uploads can enter the grace
			// window rather than the slot count.
			log.Printf("encoding: commit for %s is ambiguous (%v); the scanner reconciles it when its grace lease expires at %s",
				job.Hash[:12], err, current.LeaseUntil.UTC().Format(time.RFC3339))
			if _, markErr := s.cat.MarkEncodingReconciling(ctx, job.Hash, job.Token); markErr != nil {
				log.Printf("encoding: mark commit for %s reconciling failed: %v", job.Hash[:12], markErr)
			}
			return
		}

		if current.Status == "received" && (errors.Is(err, ErrInvalidOutput) || errors.Is(err, ErrSpoolMissing) || errors.Is(err, ErrLostLease)) {
			if current.Owner == "local" && errors.Is(err, ErrInvalidOutput) {
				if _, failErr := s.cat.SkipEncoding(ctx, job.Hash, job.Token, "failed", current.Type, "invalid encoded output", current.SizeBytes); failErr != nil {
					if !waitOutputRetry(ctx) {
						return
					}
					continue
				}
				_ = os.Remove(s.spoolPath(job.Hash, job.Token))
				return
			}
			if resetErr := s.cat.ResetReceivedEncoding(ctx, job.Hash, job.Token); resetErr != nil {
				log.Printf("encoding: reset received output for %s failed: %v", job.Hash[:12], resetErr)
				if !waitOutputRetry(ctx) {
					return
				}
				continue
			}
			log.Printf("encoding: dropped output for %s back to pending: %v", job.Hash[:12], err)
			_ = os.Remove(s.spoolPath(job.Hash, job.Token))
			return
		}
		if current.Status == "received" && errors.Is(err, ErrInvalidSource) {
			detail := err.Error()
			if current.Owner == "local" {
				detail = "invalid source blob for encoding"
			}
			if len(detail) > 512 {
				detail = detail[:512]
			}
			if _, failErr := s.cat.SkipEncoding(ctx, job.Hash, job.Token, "failed", current.Type, detail, current.SizeBytes); failErr != nil {
				log.Printf("encoding: mark invalid source %s failed: %v", job.Hash[:12], failErr)
				if !waitOutputRetry(ctx) {
					return
				}
				continue
			}
			log.Printf("encoding: output for %s failed validation: %v", job.Hash[:12], err)
			_ = os.Remove(s.spoolPath(job.Hash, job.Token))
			return
		}
		if current.Status != "received" {
			_ = os.Remove(s.spoolPath(job.Hash, job.Token))
			return
		}
		log.Printf("encoding: background output for %s will retry: %v", job.Hash[:12], err)
		if !waitOutputRetry(ctx) {
			return
		}
	}
}

func waitOutputRetry(ctx context.Context) bool {
	return waitUntil(ctx, time.Now().Add(outputRetryInterval))
}

func waitUntil(ctx context.Context, deadline time.Time) bool {
	delay := time.Until(deadline)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) processReceived(ctx context.Context, job catalog.EncodingJob) error {
	select {
	case s.commitSlots <- struct{}{}:
		defer func() { <-s.commitSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}

	current, err := s.cat.GetEncodingJob(ctx, job.Hash)
	if err != nil {
		return err
	}
	if current.Token != job.Token || current.Status != "received" {
		return ErrLostLease
	}

	key := pipeline.BlobKey(job.Hash)
	originalInfo, exists, err := s.store.StatObject(ctx, key)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: original blob missing: %s", ErrInvalidSource, job.Hash)
	}
	original, _, err := readObject(ctx, s.store, key)
	if err != nil {
		return err
	}
	if isWebP(original) {
		ok, err := s.cat.SkipEncoding(ctx, job.Hash, job.Token, "skipped", "image/webp", "", int64(len(original)))
		if err != nil {
			return err
		}
		if !ok {
			return ErrLostLease
		}
		log.Printf("encoding: original for %s is already WebP; skipped (%d bytes)", job.Hash[:12], len(original))
		return nil
	}
	if int64(len(original)) != originalInfo.Size {
		return fmt.Errorf("%w: original blob size changed for %s", ErrInvalidSource, job.Hash)
	}
	originalConfig, err := validateSource(original, job.Hash)
	if err != nil {
		return err
	}

	path := s.spoolPath(job.Hash, job.Token)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSpoolMissing
		}
		return fmt.Errorf("stat spooled output: %w", err)
	}
	encodedFile, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSpoolMissing
		}
		return fmt.Errorf("open spooled output: %w", err)
	}
	defer encodedFile.Close()
	if err := validateWebP(encodedFile, info.Size(), int64(len(original)), originalConfig); err != nil {
		return err
	}

	currentInfo, exists, err := s.store.StatObject(ctx, key)
	if err != nil {
		return err
	}
	if !exists || currentInfo.ETag != originalInfo.ETag || currentInfo.Size != originalInfo.Size {
		return ErrLostLease
	}
	ok, err := s.cat.BeginEncodingCommit(ctx, job.Hash, job.Token, int64(len(original)), LeaseTTL)
	if err != nil {
		return err
	}
	if !ok {
		return ErrLostLease
	}
	// Use an unconditional PUT so SDK retries can write the same validated
	// output again without failing on the ETag changed by the first attempt.
	if err := s.store.PutObject(ctx, key, encodedFile, "image/webp"); err != nil {
		// A PUT may have succeeded even if its response was lost. Finalize only
		// when the current object is byte-for-byte the validated spool file;
		// otherwise retain the committing lease for normal reconciliation.
		currentBytes, _, readErr := readObject(ctx, s.store, key)
		if readErr == nil {
			if _, seekErr := encodedFile.Seek(0, io.SeekStart); seekErr == nil {
				match, compareErr := matchesEncodedFile(encodedFile, currentBytes)
				if compareErr == nil && isWebP(currentBytes) && match {
					if _, markErr := s.cat.MarkEncodingReconciling(ctx, job.Hash, job.Token); markErr != nil {
						log.Printf("encoding: mark observed commit for %s reconciling failed: %v", job.Hash[:12], markErr)
					}
					if ok, finishErr := s.cat.FinishReconciledEncoding(ctx, job.Hash, job.Token, "done", "image/webp", "", int64(len(currentBytes))); finishErr != nil {
						return fmt.Errorf("put encoded blob: %w (finish observed replacement: %v)", err, finishErr)
					} else if !ok {
						return ErrLostLease
					}
					log.Printf("encoding: commit for %s finalized from the observed replacement after a PUT error (%d bytes; put error: %v)",
						job.Hash[:12], len(currentBytes), err)
					return nil
				}
			}
		}
		if readErr != nil {
			return fmt.Errorf("put encoded blob: %w (inspect current object: %v)", err, readErr)
		}
		return err
	}
	if ok, err := s.cat.FinishEncoding(ctx, job.Hash, job.Token, "done", "image/webp", "", info.Size()); err != nil {
		return err
	} else if !ok {
		return ErrLostLease
	}
	log.Printf("encoding: committed %s (%d -> %d bytes)", job.Hash[:12], len(original), info.Size())
	return nil
}

// matchesEncodedFile compares a spooled result with an observed object without
// allocating a second result-sized buffer on the PUT failure/recovery paths.
func matchesEncodedFile(file *os.File, data []byte) (bool, error) {
	buf := make([]byte, 32*1024)
	for len(data) > 0 {
		chunk := len(data)
		if chunk > len(buf) {
			chunk = len(buf)
		}
		_, err := io.ReadFull(file, buf[:chunk])
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, err
		}
		if !bytes.Equal(buf[:chunk], data[:chunk]) {
			return false, nil
		}
		data = data[chunk:]
	}
	var extra [1]byte
	_, err := io.ReadFull(file, extra[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return errors.Is(err, io.EOF), nil
}

func (s *Service) removePartialSpools() error {
	partials, err := filepath.Glob(filepath.Join(s.spoolDir, spoolFilePattern))
	if err != nil {
		return fmt.Errorf("list partial encoding spools: %w", err)
	}
	for _, path := range partials {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove partial encoding spool %s: %w", path, err)
		}
	}
	return nil
}

// removeOrphanSpools keeps results tied to catalog rows that can still be
// recovered and removes files left by crashes between rename/DB updates or
// after a terminal state. Committing files are retained through their grace
// period; Recover remains the authority on whether the S3 PUT landed.
func (s *Service) removeOrphanSpools(ctx context.Context) error {
	received, err := s.cat.ReceivedEncodings(ctx)
	if err != nil {
		return err
	}
	committing, err := s.cat.CommittingEncodings(ctx)
	if err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(received)+len(committing))
	for _, job := range append(received, committing...) {
		if validJobIdentity(job.Hash, job.Token) {
			keep[s.spoolPath(job.Hash, job.Token)] = struct{}{}
		}
	}
	files, err := filepath.Glob(filepath.Join(s.spoolDir, "*_*.webp"))
	if err != nil {
		return fmt.Errorf("list encoding spools: %w", err)
	}
	for _, path := range files {
		if _, ok := keep[path]; ok {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphan encoding spool %s: %w", path, err)
		}
	}
	return nil
}

func readObject(ctx context.Context, store Store, key string) ([]byte, string, error) {
	reader, contentType, err := store.GetObject(ctx, key)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	return data, contentType, err
}

func isWebP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

func validateSource(original []byte, hash string) (image.Config, error) {
	sum := sha256.Sum256(original)
	if hex.EncodeToString(sum[:]) != hash {
		return image.Config{}, fmt.Errorf("%w: original bytes no longer match source hash %s", ErrInvalidSource, hash)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(original))
	if err != nil {
		return image.Config{}, fmt.Errorf("%w: decode original: %v", ErrInvalidSource, err)
	}
	return config, nil
}

// validateWebP uses the same checks for the in-memory legacy stage and the
// spooled API upload. It rewinds the reader for the subsequent copy or PUT.
func validateWebP(encoded io.ReadSeeker, size, sourceSize int64, source image.Config) error {
	if size <= 0 || size >= sourceSize || size > MaxOutputBytes {
		return ErrInvalidOutput
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(encoded, header); err != nil || !isWebP(header) {
		return ErrInvalidOutput
	}
	if _, err := encoded.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind encoded output: %w", err)
	}
	config, _, err := image.DecodeConfig(encoded)
	if err != nil || config.Width != source.Width || config.Height != source.Height {
		return ErrInvalidOutput
	}
	if _, err := encoded.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind encoded output: %w", err)
	}
	if _, _, err := image.Decode(encoded); err != nil {
		return fmt.Errorf("%w: decode WebP output: %v", ErrInvalidOutput, err)
	}
	if _, err := encoded.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind encoded output: %w", err)
	}
	return nil
}

// Complete validates a worker result. "uploaded" is the only result that
// changes the object; the others terminate the work without storage writes.
func (s *Service) Complete(ctx context.Context, hash, token, outcome, detail string) (string, error) {
	job, err := s.cat.GetEncodingJob(ctx, hash)
	if err != nil {
		return "", err
	}
	if token == "" || job.Token != token {
		return "", ErrLostLease
	}
	if job.Status == "done" || job.Status == "skipped" || job.Status == "failed" {
		return job.Status, nil
	}
	if job.Status != "leased" || time.Now().After(job.LeaseUntil) {
		return "", ErrLostLease
	}
	key := pipeline.BlobKey(hash)
	if outcome != "uploaded" {
		if outcome != "already_webp" && outcome != "not_smaller" && outcome != "failed" {
			return "", fmt.Errorf("%w: %q", ErrInvalidOutcome, outcome)
		}
		data, _, err := readObject(ctx, s.store, key)
		if err != nil {
			return "", err
		}
		status, contentType := "skipped", job.Type
		originalIsWebP := isWebP(data)
		if originalIsWebP {
			contentType = "image/webp"
		}
		if outcome == "already_webp" && !originalIsWebP {
			return "", ErrInvalidOutput
		}
		if outcome == "failed" && !originalIsWebP {
			status = "failed"
		}
		if len(detail) > 512 {
			detail = detail[:512]
		}
		ok, err := s.cat.SkipEncoding(ctx, hash, token, status, contentType, detail, int64(len(data)))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrLostLease
		}
		return status, nil
	}
	// Verification reads and decodes two full images. Bound concurrent commit
	// memory even when many external workers finish at the same time.
	select {
	case s.commitSlots <- struct{}{}:
		defer func() { <-s.commitSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	originalInfo, exists, err := s.store.StatObject(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("original blob missing: %s", hash)
	}
	original, _, err := readObject(ctx, s.store, key)
	if err != nil {
		return "", err
	}
	if isWebP(original) {
		ok, err := s.cat.SkipEncoding(ctx, hash, token, "skipped", "image/webp", "", int64(len(original)))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrLostLease
		}
		return "skipped", nil
	}
	if int64(len(original)) != originalInfo.Size {
		return "", fmt.Errorf("original blob size changed for %s", hash)
	}
	originalConfig, err := validateSource(original, hash)
	if err != nil {
		return "", err
	}
	stagedInfo, exists, err := s.store.StatObject(ctx, job.StageKey)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("staged output missing: %s", job.StageKey)
	}
	// Reject oversized staged objects before downloading them into memory.
	if stagedInfo.Size <= 0 || stagedInfo.Size >= int64(len(original)) || stagedInfo.Size > MaxOutputBytes {
		return "", ErrInvalidOutput
	}
	encoded, _, err := readObject(ctx, s.store, job.StageKey)
	if err != nil {
		return "", err
	}
	if int64(len(encoded)) != stagedInfo.Size {
		return "", ErrInvalidOutput
	}
	if err := validateWebP(bytes.NewReader(encoded), stagedInfo.Size, int64(len(original)), originalConfig); err != nil {
		return "", err
	}
	currentInfo, exists, err := s.store.StatObject(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists || currentInfo.ETag != originalInfo.ETag || currentInfo.Size != originalInfo.Size {
		return "", ErrLostLease
	}
	ok, err := s.cat.BeginEncodingCommit(ctx, hash, token, int64(len(original)), LeaseTTL)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrLostLease
	}
	if err := s.store.CopyObjectIfMatch(ctx, job.StageKey, key, stagedInfo.ETag, "image/webp"); err != nil {
		// A failed response may arrive before an accepted object-store copy
		// becomes visible. Finalize only when the replacement is already
		// observable; otherwise keep the commit lease and let Recover inspect
		// it after the grace period instead of prematurely retrying the blob.
		current, _, readErr := readObject(ctx, s.store, key)
		if readErr == nil && isWebP(current) {
			if _, markErr := s.cat.MarkEncodingReconciling(ctx, hash, token); markErr != nil {
				log.Printf("encoding: mark observed commit for %s reconciling failed: %v", hash[:12], markErr)
			}
			if ok, finishErr := s.cat.FinishReconciledEncoding(ctx, hash, token, "done", "image/webp", "", int64(len(current))); finishErr != nil {
				return "", fmt.Errorf("copy staged output: %w (finish observed replacement: %v)", err, finishErr)
			} else if !ok {
				return "", ErrLostLease
			}
			if deleteErr := s.store.DeleteObjects(ctx, []string{job.StageKey}); deleteErr != nil {
				log.Printf("encoding: delete staged %s: %v", job.StageKey, deleteErr)
			}
			return "done", nil
		}
		if readErr != nil {
			return "", fmt.Errorf("copy staged output: %w (inspect current object: %v)", err, readErr)
		}
		return "", err
	}
	if ok, err := s.cat.FinishEncoding(ctx, hash, token, "done", "image/webp", "", int64(len(encoded))); err != nil {
		return "", err
	} else if !ok {
		return "", ErrLostLease
	}
	if err := s.store.DeleteObjects(ctx, []string{job.StageKey}); err != nil {
		log.Printf("encoding: delete staged %s: %v", job.StageKey, err)
	}
	return "done", nil
}

// Recover reconciles an interrupted copy after its commit grace lease expires.
// The object store decides the outcome; a source still in its old format retries.
func (s *Service) Recover(ctx context.Context) error {
	if err := s.reconcileExpiredCommits(ctx); err != nil {
		return err
	}
	if err := s.Cleanup(ctx); err != nil {
		return err
	}
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if started {
		return s.scheduleSpoolOutputs(ctx)
	}
	return nil
}

func (s *Service) reconcileExpiredCommits(ctx context.Context) error {
	jobs, err := s.cat.CommittingEncodings(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if time.Now().Before(job.LeaseUntil) {
			continue
		}
		// A conditional PUT in flight inside processReceived can outlive the
		// grace window; while a processor owns the row the scan must not reset
		// it underneath the live request. Once the processor returns, the
		// queued entry lapses and this scan reconciles the row on a later
		// tick — processors hand ambiguous commits back instead of parking on
		// them, so ownership here never blocks the spool slots.
		s.queueMu.Lock()
		_, active := s.queued[jobKey(job.Hash, job.Token)]
		s.queueMu.Unlock()
		if active {
			continue
		}
		if err := s.reconcileCommit(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// reconcileCommit uses the object currently stored at the stable blob key as
// the source of truth after a process interruption or ambiguous copy result.
func (s *Service) reconcileCommit(ctx context.Context, job catalog.EncodingJob) error {
	if _, err := s.cat.MarkEncodingReconciling(ctx, job.Hash, job.Token); err != nil {
		return err
	}
	data, _, err := readObject(ctx, s.store, pipeline.BlobKey(job.Hash))
	if err != nil {
		return err
	}
	if isWebP(data) {
		// API uploads still have a local spool after an ambiguous PUT. Unlike a
		// legacy copy, we can distinguish our output from another WebP written
		// to the same key while the commit was in flight.
		file, openErr := os.Open(s.spoolPath(job.Hash, job.Token))
		if openErr == nil {
			match, compareErr := matchesEncodedFile(file, data)
			_ = file.Close()
			if compareErr != nil {
				return compareErr
			}
			if !match {
				return s.resetCommit(ctx, job)
			}
		} else if errors.Is(openErr, os.ErrNotExist) && job.StageKey == "" {
			// The API spool was lost. A WebP at this key may be somebody else's;
			// retry the job rather than counting an unverified replacement.
			return s.resetCommit(ctx, job)
		} else if !errors.Is(openErr, os.ErrNotExist) {
			return openErr
		}
		if ok, err := s.cat.FinishReconciledEncoding(ctx, job.Hash, job.Token, "done", "image/webp", "", int64(len(data))); err != nil {
			return err
		} else if !ok {
			return ErrLostLease
		}
		log.Printf("encoding: reconciled commit for %s as done: the replacement landed despite the PUT error (%d bytes)",
			job.Hash[:12], len(data))
		_ = os.Remove(s.spoolPath(job.Hash, job.Token))
		return nil
	}
	return s.resetCommit(ctx, job)
}

func (s *Service) resetCommit(ctx context.Context, job catalog.EncodingJob) error {
	if err := s.cat.ResetCommittingEncoding(ctx, job.Hash, job.Token); err != nil {
		return err
	}
	// The replacement never became observable within the grace window, so the
	// blob returns to the pending queue and a worker encodes it again.
	log.Printf("encoding: reconciled commit for %s as retry: the replacement was not observable", job.Hash[:12])
	_ = os.Remove(s.spoolPath(job.Hash, job.Token))
	return nil
}

// Cleanup removes abandoned staged outputs after their signed URLs and leases
// have expired. It is safe to run periodically while workers are active.
func (s *Service) Cleanup(ctx context.Context) error {
	objects, err := s.store.ListObjects(ctx, "encoding/")
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	keys := make([]string, 0)
	for _, object := range objects {
		if strings.HasPrefix(object.Key, "encoding/") && object.LastModified.Before(cutoff) {
			keys = append(keys, object.Key)
		}
	}
	return s.store.DeleteObjects(ctx, keys)
}
