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
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/runs"
)

func localExecutorFixture(t *testing.T, source []byte) (*EncodingExecutor, catalog.JobRun, catalog.JobRunItem, *memoryStore) {
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
	run, _, err := cat.CreateJobRun(ctx, "operator", "encoding", "key", "local-cwebp/v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.StartJobRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	item, err := cat.NextJobRunItem(ctx, run.ID)
	if err != nil || item == nil {
		t.Fatalf("item=%+v %v", item, err)
	}
	return NewEncodingExecutor(svc), run, *item, store
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

func assertNoLocalTemps(t *testing.T, e *EncodingExecutor) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(e.service.spoolDir, "*.part"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("private temporary files leaked: %v %v", paths, err)
	}
}

func TestEncodingExecutorCommandAndAcceptedCommit(t *testing.T) {
	t.Setenv("WORKER_TOKEN", "")
	source := sourcePNG(t)
	e, run, item, store := localExecutorFixture(t, source)
	encoded := fixtureWebP(t)
	if err := e.service.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: "not-in-run", SizeBytes: 9999, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	called := 0
	var path string
	e.runCommand = func(ctx context.Context, name string, args []string, output io.Writer) error {
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
	outcome, err := e.Execute(context.Background(), run, item)
	if err != nil || outcome.Status != "succeeded" || called != 1 || store.putCalls != 1 {
		t.Fatalf("outcome=%+v err=%v calls=%d puts=%d", outcome, err, called, store.putCalls)
	}
	if !bytes.Equal(store.objects["blobs/"+item.Hash], encoded) {
		t.Fatal("validated output was not committed")
	}
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	other, _ := e.service.cat.GetEncodingJob(context.Background(), "not-in-run")
	if job.Status != "done" || job.Token != item.ClaimToken || job.Type != "image/webp" || other.Status != "pending" {
		t.Fatalf("catalog target=%+v other=%+v", job, other)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source file survived: %v", err)
	}
	assertNoLocalTemps(t, e)
}

func TestEncodingExecutorSkipsAndRejectsSafely(t *testing.T) {
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
			e, run, item, store := localExecutorFixture(t, test.source)
			if test.mutateSource {
				store.objects["blobs/"+item.Hash] = bytes.Repeat([]byte("x"), len(test.source))
			}
			calls := 0
			e.runCommand = func(ctx context.Context, name string, args []string, w io.Writer) error {
				calls++
				return writeEncoderOutput(test.output)(ctx, name, args, w)
			}
			outcome, err := e.Execute(context.Background(), run, item)
			job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
			if err != nil || outcome.Status != test.status || job.Status != test.stage || store.putCalls != 0 {
				t.Fatalf("outcome=%+v err=%v stage=%+v puts=%d", outcome, err, job, store.putCalls)
			}
			if (test.name == "already-webp" || test.name == "hash-mismatch" || test.name == "invalid-source") && calls != 0 {
				t.Fatal("encoder ran for an already-WebP or unverified input")
			}
			assertNoLocalTemps(t, e)
		})
	}
}

func TestEncodingExecutorPreservesDimensions(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	source := append(data.Bytes(), bytes.Repeat([]byte{0}, 200)...)
	e, run, item, store := localExecutorFixture(t, source)
	e.runCommand = writeEncoderOutput(fixtureWebP(t)) // valid WebP, wrong dimensions
	outcome, err := e.Execute(context.Background(), run, item)
	if err != nil || outcome.Status != "failed" || outcome.ErrorCode != "processing_failed" || store.putCalls != 0 {
		t.Fatalf("dimensions not preserved: %+v %v", outcome, err)
	}
}

func TestEncodingExecutorSafeRetryableErrors(t *testing.T) {
	for _, test := range []struct {
		name, code string
		err        error
	}{
		{"missing-cwebp", "cwebp_unavailable", &exec.Error{Name: "/private/path/secret", Err: exec.ErrNotFound}},
		{"transient", "executor_failed", errors.New("Authorization: Bearer secret /private/path")},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, run, item, _ := localExecutorFixture(t, sourcePNG(t))
			e.runCommand = func(context.Context, string, []string, io.Writer) error { return test.err }
			outcome, err := e.Execute(context.Background(), run, item)
			job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
			if err != nil || outcome.Status != "failed" || outcome.ErrorCode != test.code || job.Status != "pending" {
				t.Fatalf("unsafe/retry-blocking failure: %+v %v job=%+v", outcome, err, job)
			}
			if err := e.service.cat.FinishJobRunItem(context.Background(), run.ID, item, outcome.Status, outcome.ErrorCode); err != nil {
				t.Fatal(err)
			}
			page, err := e.service.cat.JobRunItems(context.Background(), run.ID, 10, "")
			if err != nil || page.Run.LastError != test.code || page.Items[0].LastError != test.code {
				t.Fatalf("Admin error=%+v %v", page, err)
			}
			assertNoLocalTemps(t, e)
		})
	}
}

func TestEncodingExecutorExternalBusyAndMissing(t *testing.T) {
	for _, missing := range []bool{false, true} {
		e, run, item, _ := localExecutorFixture(t, sourcePNG(t))
		var external catalog.EncodingJob
		if missing {
			item.Hash = hex.EncodeToString(make([]byte, 32))
		} else {
			jobs, err := e.service.cat.ClaimEncoding(context.Background(), 1, time.Hour)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("external=%+v %v", jobs, err)
			}
			external = jobs[0]
		}
		e.runCommand = func(context.Context, string, []string, io.Writer) error {
			t.Error("encoder invoked without exact local claim")
			return nil
		}
		outcome, err := e.Execute(context.Background(), run, item)
		if err != nil || (missing && outcome.Status != "skipped") || (!missing && outcome.Status != "busy") {
			t.Fatalf("outcome=%+v %v", outcome, err)
		}
		if !missing {
			job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
			if job.Status != "leased" || job.Owner != "external" || job.Token != external.Token {
				t.Fatalf("external lease mutated: %+v", job)
			}
		}
	}
}

func TestEncodingExecutorCancelBeforeAcceptance(t *testing.T) {
	e, run, item, store := localExecutorFixture(t, sourcePNG(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.runCommand = func(commandCtx context.Context, _ string, _ []string, _ io.Writer) error {
		cancel()
		<-commandCtx.Done()
		return errors.New("secret encoder diagnostics")
	}
	outcome, err := e.Execute(ctx, run, item)
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if err != nil || outcome.Status != "cancelled" || job.Status != "pending" || job.Token != "" || store.putCalls != 0 {
		t.Fatalf("cancel=%+v %v job=%+v", outcome, err, job)
	}
	assertNoLocalTemps(t, e)
}

func TestEncodingExecutorCancellationAfterAcceptanceWaitsForCommit(t *testing.T) {
	e, run, item, store := localExecutorFixture(t, sourcePNG(t))
	store.putStarted, store.putRelease = make(chan struct{}), make(chan struct{})
	e.runCommand = writeEncoderOutput(fixtureWebP(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan runs.Outcome, 1)
	go func() {
		outcome, _ := e.Execute(ctx, run, item)
		done <- outcome
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
	if err := e.service.cat.ReleaseLocalEncoding(context.Background(), item.Hash, item.ClaimToken); err != nil {
		t.Fatal(err)
	}
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if job.Status != "committing" || job.Token != item.ClaimToken {
		t.Fatalf("accepted commit released: %+v", job)
	}
	close(store.putRelease)
	select {
	case outcome := <-done:
		if outcome.Status != "succeeded" {
			t.Fatalf("completion=%+v", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not observe terminal commit")
	}
	assertNoLocalTemps(t, e)
}

func TestEncodingExecutorAmbiguousPutFinalizesObservedOutput(t *testing.T) {
	e, run, item, store := localExecutorFixture(t, sourcePNG(t))
	store.putReturnsErrorAfterWrite = true
	e.runCommand = writeEncoderOutput(fixtureWebP(t))
	outcome, err := e.Execute(context.Background(), run, item)
	if err != nil || outcome.Status != "succeeded" || store.putCalls != 1 {
		t.Fatalf("ambiguous commit=%+v %v puts=%d", outcome, err, store.putCalls)
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
			run, _, err := cat.CreateJobRun(ctx, "operator", "encoding", "key", "local-cwebp/v1")
			if err != nil {
				t.Fatal(err)
			}
			if err := cat.StartJobRun(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			item, err := cat.NextJobRunItem(ctx, run.ID)
			if err != nil || item == nil {
				t.Fatalf("item=%+v %v", item, err)
			}
			if _, _, err := cat.ClaimLocalEncoding(ctx, hash, item.ClaimToken, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			store := newMemoryStore()
			store.objects["blobs/"+hash] = source
			spool := t.TempDir()
			svc := New(cat, store, spool)
			if err := os.WriteFile(svc.spoolPath(hash, item.ClaimToken), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if ok, err := cat.MarkEncodingReceived(ctx, hash, item.ClaimToken); err != nil || !ok {
				t.Fatalf("receive=%v %v", ok, err)
			}
			if state == "committing" {
				if ok, err := cat.BeginEncodingCommit(ctx, hash, item.ClaimToken, int64(len(source)), -time.Minute); err != nil || !ok {
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
			job, _ := cat.GetEncodingJob(ctx, hash)
			interrupted, _ := cat.JobRun(ctx, run.ID)
			if job.Status != state || job.Owner != "local" || job.Token != item.ClaimToken || interrupted.Status != "interrupted" {
				t.Fatalf("restart abandoned accepted output: %+v run=%+v", job, interrupted)
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

func TestEncodingExecutorCompletionTimeoutRetainsAcceptedCommit(t *testing.T) {
	e, run, item, store := localExecutorFixture(t, sourcePNG(t))
	e.completionTimeout = 100 * time.Millisecond
	store.putStarted, store.putRelease = make(chan struct{}), make(chan struct{})
	e.runCommand = writeEncoderOutput(fixtureWebP(t))
	outcome, err := e.Execute(context.Background(), run, item)
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if err != nil || outcome.Status != "failed" || outcome.ErrorCode != "executor_failed" || job.Status != "committing" || job.Token != item.ClaimToken {
		close(store.putRelease)
		t.Fatalf("bounded completion abandoned commit: %+v %v job=%+v", outcome, err, job)
	}
	close(store.putRelease)
	waitForEncodingStatus(t, e.service.cat, item.Hash, "done")
}

func TestEncodingExecutorCancellationWhileReceivedKeepsHandoff(t *testing.T) {
	e, run, item, _ := localExecutorFixture(t, sourcePNG(t))
	e.runCommand = writeEncoderOutput(fixtureWebP(t))
	// Hold validation/commit capacity so cancellation happens while received,
	// rather than after BeginEncodingCommit.
	for i := 0; i < outputProcessors; i++ {
		e.service.commitSlots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan runs.Outcome, 1)
	go func() {
		outcome, _ := e.Execute(ctx, run, item)
		done <- outcome
	}()
	waitForEncodingStatus(t, e.service.cat, item.Hash, "received")
	cancel()
	select {
	case outcome := <-done:
		t.Fatalf("cancel abandoned received output: %+v", outcome)
	case <-time.After(50 * time.Millisecond):
	}
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if job.Status != "received" || job.Token != item.ClaimToken {
		t.Fatalf("received handoff reset: %+v", job)
	}
	for i := 0; i < outputProcessors; i++ {
		<-e.service.commitSlots
	}
	select {
	case outcome := <-done:
		if outcome.Status != "succeeded" {
			t.Fatalf("received completion=%+v", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("received output did not complete independently of cancellation")
	}
}

func TestEncodingExecutorPermanentCommandFailureHidesDiagnostics(t *testing.T) {
	e, run, item, _ := localExecutorFixture(t, sourcePNG(t))
	path := filepath.Join(t.TempDir(), "fake-cwebp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'secret /private/path?token=private' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.runCommand = func(ctx context.Context, _ string, args []string, output io.Writer) error {
		return runEncoderCommand(ctx, path, args, output)
	}
	outcome, err := e.Execute(context.Background(), run, item)
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if err != nil || outcome.Status != "failed" || outcome.ErrorCode != "processing_failed" || job.Status != "failed" {
		t.Fatalf("command failure=%+v %v job=%+v", outcome, err, job)
	}
	if err := e.service.cat.FinishJobRunItem(context.Background(), run.ID, item, outcome.Status, outcome.ErrorCode); err != nil {
		t.Fatal(err)
	}
	page, err := e.service.cat.JobRunItems(context.Background(), run.ID, 10, "")
	if err != nil || page.Run.LastError != "processing_failed" || page.Items[0].LastError != "processing_failed" {
		t.Fatalf("unsafe diagnostics: %+v %v", page, err)
	}
	assertNoLocalTemps(t, e)
}

func TestEncodingExecutorRejectsOversizedSourceBeforeFullDownload(t *testing.T) {
	source := sourcePNG(t)
	e, run, item, store := localExecutorFixture(t, source)
	probe := bytes.NewReader(source)
	e.service.store = executorReadStore{Store: store, get: func(context.Context, string) (io.ReadCloser, string, error) {
		return io.NopCloser(probe), "image/png", nil
	}}
	if err := e.service.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: item.Hash, SizeBytes: MaxOutputBytes + 1, ContentType: "image/png", SourceRestored: true}); err != nil {
		t.Fatal(err)
	}
	e.runCommand = func(context.Context, string, []string, io.Writer) error {
		t.Error("encoder invoked for oversized source")
		return nil
	}
	outcome, err := e.Execute(context.Background(), run, item)
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if err != nil || outcome.Status != "failed" || outcome.ErrorCode != "processing_failed" || job.Status != "failed" {
		t.Fatalf("oversized source=%+v %v job=%+v", outcome, err, job)
	}
	if read := len(source) - probe.Len(); read != 12 {
		t.Fatalf("oversized source read %d bytes; only the magic probe is allowed", read)
	}
	assertNoLocalTemps(t, e)
}

func TestEncodingExecutorRepairsPreviouslyReplacedWebP(t *testing.T) {
	encoded := fixtureWebP(t)
	for _, contentType := range []string{"image/png", "image/jpeg"} {
		for _, staleSize := range []int64{int64(len(sourcePNG(t))), 13, 0, MaxOutputBytes + 1} {
			t.Run(fmt.Sprintf("%s/size-%d", contentType, staleSize), func(t *testing.T) {
				e, run, item, store := localExecutorFixture(t, sourcePNG(t))
				if err := e.service.cat.UpsertBlob(context.Background(), catalog.Blob{Hash: item.Hash, SizeBytes: staleSize, ContentType: contentType, SourceRestored: true}); err != nil {
					t.Fatal(err)
				}
				// A pre-conversion catalog was restored, but the prior replacement
				// still lives under the original PNG/JPEG's content-addressed key.
				store.objects["blobs/"+item.Hash] = encoded
				e.runCommand = func(context.Context, string, []string, io.Writer) error {
					t.Error("encoder invoked for a previously replaced WebP")
					return nil
				}
				outcome, err := e.Execute(context.Background(), run, item)
				job, getErr := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
				if err != nil || getErr != nil || outcome.Status != "skipped" || job.Status != "skipped" || job.Type != "image/webp" || job.SizeBytes != int64(len(encoded)) || job.Token != item.ClaimToken || job.Owner != "local" || !job.LeaseUntil.Equal(time.UnixMilli(0)) {
					t.Fatalf("restored metadata not repaired: outcome=%+v err=%v job=%+v getErr=%v", outcome, err, job, getErr)
				}
				if store.putCalls != 0 || !bytes.Equal(store.objects["blobs/"+item.Hash], encoded) {
					t.Fatal("already-WebP repair changed stored bytes")
				}
				assertNoLocalTemps(t, e)
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

func TestEncodingExecutorAlreadyWebPReadIsBounded(t *testing.T) {
	e, run, item, store := localExecutorFixture(t, sourcePNG(t))
	tail := &executorCountingReader{}
	header := fixtureWebP(t)[:12]
	e.service.store = executorReadStore{Store: store, get: func(context.Context, string) (io.ReadCloser, string, error) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(header), tail)), "image/webp", nil
	}}
	outcome, err := e.Execute(context.Background(), run, item)
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if err != nil || outcome.Status != "failed" || job.Status != "failed" || job.Type != "image/png" || tail.bytes+int64(len(header)) != MaxOutputBytes+1 {
		t.Fatalf("unbounded/unsafe WebP repair: %+v %v job=%+v bytes=%d", outcome, err, job, tail.bytes+int64(len(header)))
	}
	assertNoLocalTemps(t, e)
}

func TestEncodingExecutorAlreadyWebPRepairIsTokenFenced(t *testing.T) {
	e, run, item, store := localExecutorFixture(t, sourcePNG(t))
	var external catalog.EncodingJob
	e.service.store = executorReadStore{Store: store, get: func(ctx context.Context, key string) (io.ReadCloser, string, error) {
		if err := e.service.cat.ReleaseLocalEncoding(ctx, item.Hash, item.ClaimToken); err != nil {
			return nil, "", err
		}
		jobs, err := e.service.cat.ClaimEncoding(ctx, 1, time.Hour)
		if err != nil || len(jobs) != 1 {
			return nil, "", fmt.Errorf("external claim: %v jobs=%v", err, jobs)
		}
		external = jobs[0]
		return io.NopCloser(bytes.NewReader(fixtureWebP(t))), "image/webp", nil
	}}
	outcome, err := e.Execute(context.Background(), run, item)
	job, _ := e.service.cat.GetEncodingJob(context.Background(), item.Hash)
	if err != nil || outcome.Status != "busy" || outcome.ErrorCode != "claim_lost" || job.Status != "leased" || job.Owner != "external" || job.Token != external.Token || job.Type != "image/png" {
		t.Fatalf("repair/release touched a newer external claim: %+v %v job=%+v", outcome, err, job)
	}
	assertNoLocalTemps(t, e)
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
