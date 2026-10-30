package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"viewer/internal/catalog"
)

// localEncodeFixture stands up the real encoding service over a memory store
// with one seeded blob. It does NOT claim: several tests mutate the blob row
// first (an upsert resets encoding ownership), so claiming is a separate
// explicit step that mirrors the drain's claim-then-encode order.
func localEncodeFixture(t *testing.T, source []byte) (*Service, string, *memoryStore) {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	// Deliberately label WebP fixtures as PNG: stale catalog metadata must be
	// repaired via the token-fenced already-WebP skip, not another invocation.
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	store.objects["blobs/"+hash] = source
	store.types["blobs/"+hash] = "image/png"
	svc := New(cat, store, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return svc, hash, store
}

// claimLocalTarget claims one blob exactly the way the drain does: a fresh
// catalog token claimed atomically from pending.
func claimLocalTarget(t *testing.T, svc *Service, hash string) catalog.EncodingJob {
	t.Helper()
	token, err := catalog.NewEncodingToken()
	if err != nil {
		t.Fatal(err)
	}
	job, status, err := svc.cat.ClaimLocalEncoding(context.Background(), hash, token, time.Now().Add(LeaseTTL))
	if err != nil || status != catalog.LocalEncodingClaimed {
		t.Fatalf("claim=%+v %s %v", job, status, err)
	}
	return job
}

func fixtureWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeEncoderOutput(data []byte) func(context.Context, string, []string, io.Writer) error {
	return func(_ context.Context, _ string, _ []string, w io.Writer) error {
		_, err := w.Write(data)
		return err
	}
}

func assertNoLocalTemps(t *testing.T, svc *Service) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(svc.spoolDir, "*.part"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("private temporary files leaked: %v %v", paths, err)
	}
}

func TestLocalEncodeCommandAndAcceptedCommit(t *testing.T) {
	t.Setenv("WORKER_TOKEN", "")
	source := sourcePNG(t)
	svc, hash, store := localEncodeFixture(t, source)
	job := claimLocalTarget(t, svc, hash)
	encoded := fixtureWebP(t)
	if err := svc.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: "not-in-drain", SizeBytes: 9999, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	called := 0
	var path string
	svc.runCommand = func(ctx context.Context, name string, args []string, output io.Writer) error {
		called++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("encoder is not time-bounded")
		}
		if len(args) != 9 {
			t.Fatalf("args=%q", args)
		}
		path = args[6]
		want := []string{"-q", "85", "-alpha_q", "100", "-metadata", "all", path, "-o", "-"}
		if name != "cwebp" || !reflect.DeepEqual(args, want) {
			t.Fatalf("command=%s %q", name, args)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("source permissions: %v %v", info, err)
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, source) {
			t.Fatal("encoder source differs from verified blob")
		}
		return writeEncoderOutput(encoded)(ctx, name, args, output)
	}
	outcome := svc.encodeLocalBlob(context.Background(), job)
	if outcome.Status != localEncodeSucceeded || called != 1 || store.putCalls != 1 {
		t.Fatalf("outcome=%+v calls=%d puts=%d", outcome, called, store.putCalls)
	}
	if !bytes.Equal(store.objects["blobs/"+job.Hash], encoded) {
		t.Fatal("validated output was not committed")
	}
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	other, _ := svc.cat.GetEncodingJob(context.Background(), "not-in-drain")
	if saved.Status != "done" || saved.Token != job.Token || saved.Type != "image/webp" || other.Status != "pending" {
		t.Fatalf("catalog target=%+v other=%+v", saved, other)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source file survived: %v", err)
	}
	assertNoLocalTemps(t, svc)
}

func TestLocalEncodeSkipsAndRejectsSafely(t *testing.T) {
	for _, test := range []struct {
		name, status, stage string
		source, output      []byte
		mutateSource        bool
	}{
		{name: "already-webp", source: fixtureWebP(t), status: "skipped", stage: "skipped"},
		{name: "not-smaller", source: sourcePNG(t), output: bytes.Repeat([]byte("x"), len(sourcePNG(t))), status: "skipped", stage: "skipped"},
		{name: "file-bound", source: sourcePNG(t), output: bytes.Repeat([]byte("x"), len(sourcePNG(t))+1), status: "skipped", stage: "skipped"},
		{name: "invalid-output", source: sourcePNG(t), output: []byte("secret /private/path"), status: "failed", stage: "failed"},
		{name: "invalid-source", source: []byte("secret /private/path"), status: "failed", stage: "failed"},
		{name: "hash-mismatch", source: sourcePNG(t), mutateSource: true, status: "failed", stage: "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, hash, store := localEncodeFixture(t, test.source)
			if test.mutateSource {
				store.objects["blobs/"+hash] = bytes.Repeat([]byte("x"), len(test.source))
			}
			job := claimLocalTarget(t, svc, hash)
			calls := 0
			svc.runCommand = func(ctx context.Context, name string, args []string, w io.Writer) error {
				calls++
				return writeEncoderOutput(test.output)(ctx, name, args, w)
			}
			outcome := svc.encodeLocalBlob(context.Background(), job)
			saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
			if outcome.Status != localEncodeStatus(test.status) || saved.Status != test.stage || store.putCalls != 0 {
				t.Fatalf("outcome=%+v stage=%+v puts=%d", outcome, saved, store.putCalls)
			}
			if (test.name == "already-webp" || test.name == "hash-mismatch" || test.name == "invalid-source") && calls != 0 {
				t.Fatal("encoder ran for an already-WebP or unverified input")
			}
			assertNoLocalTemps(t, svc)
		})
	}
}

func TestLocalEncodePreservesDimensions(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	source := append(data.Bytes(), bytes.Repeat([]byte{0}, 200)...)
	svc, hash, store := localEncodeFixture(t, source)
	job := claimLocalTarget(t, svc, hash)
	svc.runCommand = writeEncoderOutput(fixtureWebP(t)) // valid WebP, wrong dimensions
	outcome := svc.encodeLocalBlob(context.Background(), job)
	if outcome.Status != localEncodeFailed || outcome.ErrorCode != "processing_failed" || store.putCalls != 0 {
		t.Fatalf("dimensions not preserved: %+v puts=%d", outcome, store.putCalls)
	}
}

func TestLocalEncodeSafeRetryableErrors(t *testing.T) {
	for _, test := range []struct {
		name, code string
		err        error
	}{
		{"missing-cwebp", "cwebp_unavailable", &exec.Error{Name: "/private/path/secret", Err: exec.ErrNotFound}},
		{"transient", "executor_failed", errors.New("Authorization: Bearer secret /private/path")},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, hash, _ := localEncodeFixture(t, sourcePNG(t))
			job := claimLocalTarget(t, svc, hash)
			svc.runCommand = func(context.Context, string, []string, io.Writer) error { return test.err }
			outcome := svc.encodeLocalBlob(context.Background(), job)
			saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
			if outcome.Status != localEncodeFailed || outcome.ErrorCode != test.code || saved.Status != "pending" {
				t.Fatalf("unsafe/retry-blocking failure: %+v job=%+v", outcome, saved)
			}
			assertNoLocalTemps(t, svc)
		})
	}
}

// TestLocalDrainSkipsBusyAndMissingTargets pins the drain-side claim handling
// the executor used to carry: a target owned by anyone else (here an external
// worker) or gone since the snapshot is skipped without invoking cwebp, and
// the external lease is untouched.
func TestLocalDrainSkipsBusyAndMissingTargets(t *testing.T) {
	svc, hash, _ := localEncodeFixture(t, sourcePNG(t))
	external, err := svc.cat.ClaimEncoding(context.Background(), 1, time.Hour)
	if err != nil || len(external) != 1 {
		t.Fatalf("external=%+v %v", external, err)
	}
	svc.runCommand = func(context.Context, string, []string, io.Writer) error {
		t.Error("drain invoked the encoder for a target it does not own")
		return nil
	}
	// A missing hash models a row deleted between snapshot and claim.
	svc.runLocalDrain(context.Background(), []string{hash, hex.EncodeToString(make([]byte, 32))})
	current, _ := svc.cat.GetEncodingJob(context.Background(), hash)
	if current.Status != "leased" || current.Owner != "external" || current.Token != external[0].Token {
		t.Fatalf("external lease mutated: %+v", current)
	}
}

func TestLocalEncodeCancelBeforeAcceptance(t *testing.T) {
	svc, hash, store := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.runCommand = func(commandCtx context.Context, _ string, _ []string, _ io.Writer) error {
		cancel()
		<-commandCtx.Done()
		return errors.New("secret encoder diagnostics")
	}
	outcome := svc.encodeLocalBlob(ctx, job)
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if outcome.Status != localEncodeCancelled || saved.Status != "pending" || saved.Token != "" || store.putCalls != 0 {
		t.Fatalf("cancel=%+v job=%+v", outcome, saved)
	}
	assertNoLocalTemps(t, svc)
}

func TestLocalEncodeCancellationAfterAcceptanceWaitsForCommit(t *testing.T) {
	svc, hash, store := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	store.putStarted, store.putRelease = make(chan struct{}), make(chan struct{})
	svc.runCommand = writeEncoderOutput(fixtureWebP(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan localEncodeOutcome, 1)
	go func() {
		done <- svc.encodeLocalBlob(ctx, job)
	}()
	select {
	case <-store.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("commit did not start")
	}
	cancel()
	select {
	case outcome := <-done:
		t.Fatalf("cancel abandoned accepted output: %+v", outcome)
	case <-time.After(50 * time.Millisecond):
	}
	if err := svc.cat.ReleaseLocalEncoding(context.Background(), job.Hash, job.Token); err != nil {
		t.Fatal(err)
	}
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if saved.Status != "committing" || saved.Token != job.Token {
		t.Fatalf("accepted commit released: %+v", saved)
	}
	close(store.putRelease)
	select {
	case outcome := <-done:
		if outcome.Status != localEncodeSucceeded {
			t.Fatalf("completion=%+v", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not observe terminal commit")
	}
	assertNoLocalTemps(t, svc)
}

func TestLocalEncodeAmbiguousPutFinalizesObservedOutput(t *testing.T) {
	svc, hash, store := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	store.putReturnsErrorAfterWrite = true
	svc.runCommand = writeEncoderOutput(fixtureWebP(t))
	outcome := svc.encodeLocalBlob(context.Background(), job)
	if outcome.Status != localEncodeSucceeded || store.putCalls != 1 {
		t.Fatalf("ambiguous commit=%+v puts=%d", outcome, store.putCalls)
	}
}

func TestRunEncoderCommandKillsTimedOutProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-cwebp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'secret /private/path' >&2\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := runEncoderCommand(ctx, path, nil, io.Discard); err == nil || ctx.Err() == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("process did not terminate on timeout: %v ctx=%v", err, ctx.Err())
	}
}

func TestLocalAcceptedEncodingSurvivesRestart(t *testing.T) {
	for _, state := range []string{"received", "committing"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "catalog.db")
			cat, err := catalog.Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			source, encoded := sourcePNG(t), fixtureWebP(t)
			sum := sha256.Sum256(source)
			hash := hex.EncodeToString(sum[:])
			if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
				t.Fatal(err)
			}
			token, err := catalog.NewEncodingToken()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := cat.ClaimLocalEncoding(ctx, hash, token, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			store := newMemoryStore()
			store.objects["blobs/"+hash] = source
			spool := t.TempDir()
			svc := New(cat, store, spool)
			if err := os.WriteFile(svc.spoolPath(hash, token), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if ok, err := cat.MarkEncodingReceived(ctx, hash, token); err != nil || !ok {
				t.Fatalf("receive=%v %v", ok, err)
			}
			if state == "committing" {
				if ok, err := cat.BeginEncodingCommit(ctx, hash, token, int64(len(source)), -time.Minute); err != nil || !ok {
					t.Fatalf("commit=%v %v", ok, err)
				}
				// The PUT landed but the process died before recording completion.
				store.objects["blobs/"+hash] = encoded
			}
			if err := cat.Close(); err != nil {
				t.Fatal(err)
			}
			cat, err = catalog.Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			// Startup hygiene must reset only leased-local rows (the crashed
			// drain's claims); accepted output survives the restart untouched.
			job, _ := cat.GetEncodingJob(ctx, hash)
			if job.Status != state || job.Owner != "local" || job.Token != token {
				t.Fatalf("restart abandoned accepted output: %+v", job)
			}
			svc = New(cat, store, spool)
			serviceCtx, stop := context.WithCancel(ctx)
			defer stop()
			if err := svc.Start(serviceCtx); err != nil {
				t.Fatal(err)
			}
			waitForEncodingStatus(t, cat, hash, "done")
			counts, err := cat.EncodingCounts(ctx)
			if err != nil || counts.Converted != 1 || counts.SourceBytes != int64(len(source)) || counts.OutputBytes != int64(len(encoded)) {
				t.Fatalf("recovered bookkeeping=%+v %v", counts, err)
			}
		})
	}
}

func TestLocalEncodeCompletionTimeoutRetainsAcceptedCommit(t *testing.T) {
	svc, hash, store := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	svc.completionTimeout = 100 * time.Millisecond
	store.putStarted, store.putRelease = make(chan struct{}), make(chan struct{})
	svc.runCommand = writeEncoderOutput(fixtureWebP(t))
	outcome := svc.encodeLocalBlob(context.Background(), job)
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if outcome.Status != localEncodeFailed || outcome.ErrorCode != "executor_failed" || saved.Status != "committing" || saved.Token != job.Token {
		close(store.putRelease)
		t.Fatalf("bounded completion abandoned commit: %+v job=%+v", outcome, saved)
	}
	close(store.putRelease)
	waitForEncodingStatus(t, svc.cat, job.Hash, "done")
}

func TestLocalEncodeCancellationWhileReceivedKeepsHandoff(t *testing.T) {
	svc, hash, _ := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	svc.runCommand = writeEncoderOutput(fixtureWebP(t))
	// Hold validation/commit capacity so cancellation happens while received,
	// rather than after BeginEncodingCommit.
	for i := 0; i < outputProcessors; i++ {
		svc.commitSlots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan localEncodeOutcome, 1)
	go func() {
		done <- svc.encodeLocalBlob(ctx, job)
	}()
	waitForEncodingStatus(t, svc.cat, job.Hash, "received")
	cancel()
	select {
	case outcome := <-done:
		t.Fatalf("cancel abandoned received output: %+v", outcome)
	case <-time.After(50 * time.Millisecond):
	}
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if saved.Status != "received" || saved.Token != job.Token {
		t.Fatalf("received handoff reset: %+v", saved)
	}
	for i := 0; i < outputProcessors; i++ {
		<-svc.commitSlots
	}
	select {
	case outcome := <-done:
		if outcome.Status != localEncodeSucceeded {
			t.Fatalf("received completion=%+v", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("received output did not complete independently of cancellation")
	}
}

func TestLocalEncodePermanentCommandFailureHidesDiagnostics(t *testing.T) {
	svc, hash, _ := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	path := filepath.Join(t.TempDir(), "fake-cwebp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'secret /private/path?token=private' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	svc.runCommand = func(ctx context.Context, _ string, args []string, output io.Writer) error {
		return runEncoderCommand(ctx, path, args, output)
	}
	outcome := svc.encodeLocalBlob(context.Background(), job)
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if outcome.Status != localEncodeFailed || outcome.ErrorCode != "processing_failed" || saved.Status != "failed" {
		t.Fatalf("command failure=%+v job=%+v", outcome, saved)
	}
	assertNoLocalTemps(t, svc)
}

func TestLocalEncodeRejectsOversizedSourceBeforeFullDownload(t *testing.T) {
	source := sourcePNG(t)
	svc, hash, store := localEncodeFixture(t, source)
	probe := bytes.NewReader(source)
	svc.store = executorReadStore{Store: store, get: func(context.Context, string) (io.ReadCloser, string, error) {
		return io.NopCloser(probe), "image/png", nil
	}}
	if err := svc.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: hash, SizeBytes: MaxOutputBytes + 1, ContentType: "image/png", SourceRestored: true}); err != nil {
		t.Fatal(err)
	}
	job := claimLocalTarget(t, svc, hash)
	svc.runCommand = func(context.Context, string, []string, io.Writer) error {
		t.Error("encoder invoked for oversized source")
		return nil
	}
	outcome := svc.encodeLocalBlob(context.Background(), job)
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if outcome.Status != localEncodeFailed || outcome.ErrorCode != "processing_failed" || saved.Status != "failed" {
		t.Fatalf("oversized source=%+v job=%+v", outcome, saved)
	}
	if read := len(source) - probe.Len(); read != 12 {
		t.Fatalf("oversized source read %d bytes; only the magic probe is allowed", read)
	}
	assertNoLocalTemps(t, svc)
}

func TestLocalEncodeRepairsPreviouslyReplacedWebP(t *testing.T) {
	encoded := fixtureWebP(t)
	for _, contentType := range []string{"image/png", "image/jpeg"} {
		for _, staleSize := range []int64{int64(len(sourcePNG(t))), 13, 0, MaxOutputBytes + 1} {
			t.Run(fmt.Sprintf("%s/size-%d", contentType, staleSize), func(t *testing.T) {
				svc, hash, store := localEncodeFixture(t, sourcePNG(t))
				if err := svc.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: hash, SizeBytes: staleSize, ContentType: contentType, SourceRestored: true}); err != nil {
					t.Fatal(err)
				}
				job := claimLocalTarget(t, svc, hash)
				// A pre-conversion catalog was restored, but the prior replacement
				// still lives under the original PNG/JPEG's content-addressed key.
				store.objects["blobs/"+hash] = encoded
				svc.runCommand = func(context.Context, string, []string, io.Writer) error {
					t.Error("encoder invoked for a previously replaced WebP")
					return nil
				}
				outcome := svc.encodeLocalBlob(context.Background(), job)
				saved, getErr := svc.cat.GetEncodingJob(context.Background(), job.Hash)
				if getErr != nil || outcome.Status != localEncodeSkipped || saved.Status != "skipped" || saved.Type != "image/webp" || saved.SizeBytes != int64(len(encoded)) || saved.Token != job.Token || saved.Owner != "local" || !saved.LeaseUntil.Equal(time.UnixMilli(0)) {
					t.Fatalf("restored metadata not repaired: outcome=%+v job=%+v getErr=%v", outcome, saved, getErr)
				}
				if store.putCalls != 0 || !bytes.Equal(store.objects["blobs/"+hash], encoded) {
					t.Fatal("already-WebP repair changed stored bytes")
				}
				assertNoLocalTemps(t, svc)
			})
		}
	}
}

type executorReadStore struct {
	Store
	get func(context.Context, string) (io.ReadCloser, string, error)
}

func (s executorReadStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	return s.get(ctx, key)
}

// Infinite input without allocating/writing a gigabyte-sized test fixture.
type executorCountingReader struct{ bytes int64 }

func (r *executorCountingReader) Read(p []byte) (int, error) {
	r.bytes += int64(len(p))
	return len(p), nil
}

func TestLocalEncodeAlreadyWebPReadIsBounded(t *testing.T) {
	svc, hash, store := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	tail := &executorCountingReader{}
	header := fixtureWebP(t)[:12]
	svc.store = executorReadStore{Store: store, get: func(context.Context, string) (io.ReadCloser, string, error) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(header), tail)), "image/webp", nil
	}}
	outcome := svc.encodeLocalBlob(context.Background(), job)
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if outcome.Status != localEncodeFailed || saved.Status != "failed" || saved.Type != "image/png" || tail.bytes+int64(len(header)) != MaxOutputBytes+1 {
		t.Fatalf("unbounded/unsafe WebP repair: %+v job=%+v bytes=%d", outcome, saved, tail.bytes+int64(len(header)))
	}
	assertNoLocalTemps(t, svc)
}

func TestLocalEncodeAlreadyWebPRepairIsTokenFenced(t *testing.T) {
	svc, hash, store := localEncodeFixture(t, sourcePNG(t))
	job := claimLocalTarget(t, svc, hash)
	var external catalog.EncodingJob
	svc.store = executorReadStore{Store: store, get: func(ctx context.Context, key string) (io.ReadCloser, string, error) {
		if err := svc.cat.ReleaseLocalEncoding(ctx, job.Hash, job.Token); err != nil {
			return nil, "", err
		}
		jobs, err := svc.cat.ClaimEncoding(ctx, 1, time.Hour)
		if err != nil || len(jobs) != 1 {
			return nil, "", fmt.Errorf("external claim: %v jobs=%v", err, jobs)
		}
		external = jobs[0]
		return io.NopCloser(bytes.NewReader(fixtureWebP(t))), "image/webp", nil
	}}
	outcome := svc.encodeLocalBlob(context.Background(), job)
	saved, _ := svc.cat.GetEncodingJob(context.Background(), job.Hash)
	if outcome.Status != localEncodeBusy || outcome.ErrorCode != "claim_lost" || saved.Status != "leased" || saved.Owner != "external" || saved.Token != external.Token || saved.Type != "image/png" {
		t.Fatalf("repair/release touched a newer external claim: %+v job=%+v", outcome, saved)
	}
	assertNoLocalTemps(t, svc)
}

func TestBoundedEncoderOutputDoesNotGrowFilePastLimit(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &boundedEncoderOutput{file: file, remaining: 4, cancel: cancel}
	if _, err := output.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("5")); !errors.Is(err, ErrOutputTooLarge) || ctx.Err() == nil {
		t.Fatalf("file bound did not cancel command: %v ctx=%v", err, ctx.Err())
	}
	info, _ := file.Stat()
	if info.Size() != 4 || !output.exceeded {
		t.Fatalf("file grew beyond bound: size=%d", info.Size())
	}
}

// distinctSourcePNG returns a deterministic, decodable PNG unique per seed:
// the drain snapshot is keyed by content hash, so tests that need more than
// one target must not reuse sourcePNG's identical bytes.
func distinctSourcePNG(t *testing.T, seed string) []byte {
	t.Helper()
	return append(sourcePNG(t), []byte("distinct:"+seed)...)
}

// fakeCwebpOnPATH installs a harmless executable named cwebp so
// StartLocalDrain's fail-fast PATH check passes; the tests still override
// runCommand, so the fake is never executed.
func fakeCwebpOnPATH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cwebp"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestStartLocalDrainRequiresStartedServiceAndCwebp(t *testing.T) {
	// Not started: New without Start must refuse before touching the queue.
	unstarted := New(nil, nil, t.TempDir())
	if _, err := unstarted.StartLocalDrain(); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("unstarted drain=%v", err)
	}
	// Started, but no cwebp anywhere on PATH: one clear error instead of a
	// per-target cwebp_unavailable failure for the whole snapshot.
	svc, _, _ := localEncodeFixture(t, sourcePNG(t))
	svc.runCommand = func(context.Context, string, []string, io.Writer) error {
		t.Error("encoder invoked without cwebp on PATH")
		return nil
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := svc.StartLocalDrain(); err == nil || !strings.Contains(err.Error(), "cwebp") {
		t.Fatalf("missing cwebp drain=%v", err)
	}
	// The queue was not claimed behind the failed start.
	counts, err := svc.cat.EncodingCounts(context.Background())
	if err != nil || counts.Pending != 1 {
		t.Fatalf("queue touched by failed start: %+v %v", counts, err)
	}
}

// TestStartLocalDrainIsSingleFlightAndSnapshotsOnce pins the drain's two
// structural guarantees: a second trigger while one drain runs answers
// (false, nil) instead of forking a second pipeline, and the target set is
// the snapshot taken at trigger time — blobs uploaded afterwards wait for the
// next trigger.
func TestStartLocalDrainIsSingleFlightAndSnapshotsOnce(t *testing.T) {
	fakeCwebpOnPATH(t)
	first := distinctSourcePNG(t, "first")
	svc, firstHash, store := localEncodeFixture(t, first)
	encoded := fixtureWebP(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	svc.runCommand = func(ctx context.Context, name string, args []string, w io.Writer) error {
		entered <- struct{}{}
		<-release
		return writeEncoderOutput(encoded)(ctx, name, args, w)
	}
	started, err := svc.StartLocalDrain()
	if err != nil || !started {
		t.Fatalf("start=%v err=%v", started, err)
	}
	// A blob uploaded after the snapshot must not join this drain.
	second := distinctSourcePNG(t, "second")
	secondSum := sha256.Sum256(second)
	secondHash := hex.EncodeToString(secondSum[:])
	if err := svc.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: secondHash, SizeBytes: int64(len(second)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store.objects["blobs/"+secondHash] = second
	// Single flight while the first drain is mid-blob.
	again, err := svc.StartLocalDrain()
	if err != nil || again {
		t.Fatalf("second drain=%v err=%v want (false,nil)", again, err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never invoked the encoder")
	}
	close(release)
	waitForEncodingStatus(t, svc.cat, firstHash, "done")
	otherJob, _ := svc.cat.GetEncodingJob(context.Background(), secondHash)
	if otherJob.Status != "pending" {
		t.Fatalf("post-snapshot blob joined the drain: %+v", otherJob)
	}
	// Once the drain finished, a new trigger picks the late arrival up. The
	// goroutine clears drainRunning shortly after the terminal status lands,
	// so retry the trigger until it is accepted.
	svc.runCommand = writeEncoderOutput(fixtureWebP(t))
	deadline := time.Now().Add(5 * time.Second)
	for {
		started, err = svc.StartLocalDrain()
		if err != nil {
			t.Fatal(err)
		}
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain never became idle again")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitForEncodingStatus(t, svc.cat, secondHash, "done")
}

// TestStartLocalDrainStopsBetweenBlobsOnServiceShutdown pins the shutdown
// contract: Close cancels the service lifetime the drain shares with the
// output processors, the in-flight blob's lease is handed back (not stranded
// until lease expiry), and the remaining targets stay pending for the next
// trigger.
func TestStartLocalDrainStopsBetweenBlobsOnServiceShutdown(t *testing.T) {
	fakeCwebpOnPATH(t)
	first := distinctSourcePNG(t, "shutdown-first")
	svc, firstHash, store := localEncodeFixture(t, first)
	// Seed a second target, so the drain has unprocessed work left when the
	// first blob finishes.
	second := distinctSourcePNG(t, "shutdown-second")
	secondSum := sha256.Sum256(second)
	secondHash := hex.EncodeToString(secondSum[:])
	if err := svc.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: secondHash, SizeBytes: int64(len(second)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store.objects["blobs/"+secondHash] = second
	var calls atomic.Int32
	svc.runCommand = func(ctx context.Context, _ string, _ []string, _ io.Writer) error {
		calls.Add(1)
		<-ctx.Done() // hold the blob until shutdown cancels the command
		return ctx.Err()
	}
	started, err := svc.StartLocalDrain()
	if err != nil || !started {
		t.Fatalf("start=%v err=%v", started, err)
	}
	// Wait until the drain owns one blob, then shut the service down under it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, _ := svc.cat.GetEncodingJob(context.Background(), firstHash)
		other, _ := svc.cat.GetEncodingJob(context.Background(), secondHash)
		if (held.Status == "leased" && held.Owner == "local") || (other.Status == "leased" && other.Owner == "local") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain never claimed a target: %+v %+v", held, other)
		}
		time.Sleep(5 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { svc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the drain")
	}
	for _, hash := range []string{firstHash, secondHash} {
		final, err := svc.cat.GetEncodingJob(context.Background(), hash)
		if err != nil {
			t.Fatal(err)
		}
		// The blob the drain reached had its lease handed back (pending); the
		// one it never reached stayed pending. Neither may linger in a
		// leased-local state that nothing else can reclaim.
		if final.Status != "pending" || final.Owner == "local" {
			t.Fatalf("shutdown stranded target %s: %+v", hash, final)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("drain processed %d blob(s); shutdown must stop it between blobs", calls.Load())
	}
}
