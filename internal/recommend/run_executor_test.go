package recommend

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/images"
	"viewer/internal/qdrant"
	"viewer/internal/runs"
)

type runEmbedder struct {
	okEmbedder
	embed func(context.Context, []byte) ([]float32, error)
}

func (e runEmbedder) Embed(ctx context.Context, data []byte) ([]float32, error) {
	return e.embed(ctx, data)
}

type runImageStore struct {
	blobStoreStub
	before func(context.Context) error
}

func (s *runImageStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	if s.before != nil {
		if err := s.before(ctx); err != nil {
			return nil, "", err
		}
	}
	return s.blobStoreStub.GetObject(ctx, key)
}

type embeddingRunFixture struct {
	cat      *catalog.Store
	service  *Service
	images   *runImageStore
	vectors  *fakeVectorStore
	run      catalog.JobRun
	item     catalog.JobRunItem
	executor *EmbeddingExecutor
}

func newEmbeddingRunFixture(t *testing.T) *embeddingRunFixture {
	t.Helper()
	ctx := context.Background()
	cat := newTestCatalog(t)
	seedBlob(t, cat, "target")
	seedAlbum(t, cat, "album", catalog.Photo{Index: 0, Hash: "target", Width: 2, Height: 3})
	store := &runImageStore{blobStoreStub: blobStoreStub{objects: map[string][]byte{"blobs/target": []byte("image bytes")}}}
	vectors := newFakeVectorStore()
	svc := NewService(cat, vectors, images.NewService(cat, store), okEmbedder{}, nil)
	run, _, err := cat.CreateJobRun(ctx, "admin", "embedding", "test-run", "local-embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.StartJobRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	item, err := cat.NextJobRunItem(ctx, run.ID)
	if err != nil || item == nil || item.ClaimToken == "" {
		t.Fatalf("run item=%v err=%v", item, err)
	}
	return &embeddingRunFixture{cat, svc, store, vectors, run, *item, NewEmbeddingExecutor(svc)}
}

func (f *embeddingRunFixture) execute(t *testing.T, ctx context.Context, want runs.Outcome) {
	t.Helper()
	got, err := f.executor.Execute(ctx, f.run, f.item)
	if err != nil || got != want {
		t.Fatalf("outcome=%+v err=%v want=%+v", got, err, want)
	}
	if err := f.cat.FinishJobRunItem(context.Background(), f.run.ID, f.item, got.Status, got.ErrorCode); err != nil {
		t.Fatal(err)
	}
	page, err := f.cat.JobRunItems(context.Background(), f.run.ID, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != want.Status || page.Items[0].LastError != want.ErrorCode {
		t.Fatalf("persisted item=%+v err=%v", page, err)
	}
}

func (f *embeddingRunFixture) assertBlob(t *testing.T, hash string, want catalog.EmbeddingStatus) {
	t.Helper()
	blob, err := f.cat.GetBlob(context.Background(), hash)
	if err != nil || blob.EmbeddingStatus != want {
		t.Fatalf("blob=%+v err=%v want=%s", blob, err, want)
	}
}

func (f *embeddingRunFixture) assertNoVectors(t *testing.T) {
	t.Helper()
	if points, ops := f.vectors.snapshot(); len(points) != 0 || len(ops) != 0 {
		t.Fatalf("unexpected vector writes: points=%v ops=%v", points, ops)
	}
}

func TestEmbeddingExecutorSuccessClaimsOnlySnapshotTarget(t *testing.T) {
	f := newEmbeddingRunFixture(t)
	// New uploads are outside the finite snapshot and must never be claimed.
	seedBlob(t, f.cat, "later")
	f.service.embedder = runEmbedder{embed: func(ctx context.Context, data []byte) ([]float32, error) {
		if string(data) != "image bytes" {
			t.Fatalf("unexpected image bytes: %q", data)
		}
		// The run item's token, not a newly generated batch token, owns the blob.
		owned := catalog.LocalEmbeddingClaim{Blob: catalog.Blob{Hash: f.item.Hash}, Token: f.item.ClaimToken}
		if rows, err := f.cat.RenewLocalEmbeddingLeases(ctx, []catalog.LocalEmbeddingClaim{owned}, time.Now().Add(time.Hour)); err != nil || len(rows) != 1 {
			t.Fatalf("run item does not own blob: %v %v", rows, err)
		}
		f.assertBlob(t, "later", catalog.EmbeddingStatusPending)
		return vec768(1), nil
	}}
	f.execute(t, context.Background(), runs.Outcome{Status: "succeeded"})
	f.assertBlob(t, "target", catalog.EmbeddingStatusReady)
	f.assertBlob(t, "later", catalog.EmbeddingStatusPending)
	points, ops := f.vectors.snapshot()
	if len(points) != 1 || len(ops) != 1 {
		t.Fatalf("vector writes=%v ops=%v", points, ops)
	}
	for _, point := range points {
		if point.Hash != "target" || point.AlbumID != "album" || point.W != 2 || point.H != 3 || len(point.Vector) != catalog.EmbeddingDim {
			t.Fatalf("wrong photo vector: %+v", point)
		}
	}
}

func TestEmbeddingExecutorBusyAndMissing(t *testing.T) {
	for _, owner := range []string{"external", "local", "missing"} {
		t.Run(owner, func(t *testing.T) {
			f := newEmbeddingRunFixture(t)
			ctx := context.Background()
			want := runs.Outcome{Status: "busy", ErrorCode: "target_busy"}
			if owner == "external" {
				if rows, err := f.cat.ClaimExternalEmbeddings(ctx, 1, time.Now().Add(-time.Hour)); err != nil || len(rows) != 1 {
					t.Fatalf("external claim=%v %v", rows, err)
				}
			} else if owner == "local" {
				if _, status, err := f.cat.ClaimLocalEmbedding(ctx, "target", "other-owner", time.Now().Add(time.Hour)); err != nil || status != catalog.LocalEmbeddingClaimed {
					t.Fatalf("local claim=%s %v", status, err)
				}
			} else {
				f.item.Hash = "disappeared"
				want = runs.Outcome{Status: "skipped", ErrorCode: "target_missing"}
			}
			f.images.before = func(context.Context) error {
				t.Fatal("busy/missing target loaded image")
				return nil
			}
			got, err := f.executor.Execute(ctx, f.run, f.item)
			if err != nil || got != want {
				t.Fatalf("outcome=%+v err=%v", got, err)
			}
			f.assertNoVectors(t)
			if owner == "local" {
				claim := catalog.LocalEmbeddingClaim{Blob: catalog.Blob{Hash: "target"}, Token: "other-owner"}
				if rows, err := f.cat.RenewLocalEmbeddingLeases(ctx, []catalog.LocalEmbeddingClaim{claim}, time.Now().Add(time.Hour)); err != nil || len(rows) != 1 {
					t.Fatalf("busy executor changed owner: %v %v", rows, err)
				}
			}
		})
	}
}

func TestEmbeddingExecutorCancellationBeforeFinalization(t *testing.T) {
	for _, phase := range []string{"before-claim", "image-load", "inference"} {
		t.Run(phase, func(t *testing.T) {
			f := newEmbeddingRunFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inferred := false
			f.service.embedder = runEmbedder{embed: func(context.Context, []byte) ([]float32, error) {
				inferred = true
				cancel()
				// Even a provider ignoring cancellation cannot finalize this result.
				return vec768(1), nil
			}}
			if phase == "before-claim" {
				cancel()
				f.images.before = func(context.Context) error { t.Fatal("loaded cancelled item"); return nil }
			} else if phase == "image-load" {
				f.images.before = func(context.Context) error { cancel(); return nil }
			}
			f.execute(t, ctx, runs.Outcome{Status: "cancelled"})
			if inferred != (phase == "inference") {
				t.Fatalf("inferred=%v phase=%s", inferred, phase)
			}
			f.assertBlob(t, "target", catalog.EmbeddingStatusPending)
			f.assertNoVectors(t)
		})
	}
}

func TestEmbeddingExecutorLostTokenNeverWritesOrReleasesNewOwner(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale-result", true: "cancelled-stale-result"}[cancelRun], func(t *testing.T) {
			f := newEmbeddingRunFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.service.embedder = runEmbedder{embed: func(context.Context, []byte) ([]float32, error) {
				claim := catalog.LocalEmbeddingClaim{Blob: catalog.Blob{Hash: f.item.Hash}, Token: f.item.ClaimToken}
				if rows, err := f.cat.ReleaseLocalEmbeddingClaims(context.Background(), []catalog.LocalEmbeddingClaim{claim}); err != nil || len(rows) != 1 {
					t.Fatalf("release=%v %v", rows, err)
				}
				if _, status, err := f.cat.ClaimLocalEmbedding(context.Background(), f.item.Hash, "new-token", time.Now().Add(time.Hour)); err != nil || status != catalog.LocalEmbeddingClaimed {
					t.Fatalf("new claim=%s %v", status, err)
				}
				if cancelRun {
					cancel()
				}
				return vec768(1), nil
			}}
			want := runs.Outcome{Status: "busy", ErrorCode: "claim_lost"}
			if cancelRun {
				want = runs.Outcome{Status: "cancelled"}
			}
			f.execute(t, ctx, want)
			f.assertNoVectors(t)
			newClaim := catalog.LocalEmbeddingClaim{Blob: catalog.Blob{Hash: f.item.Hash}, Token: "new-token"}
			if rows, err := f.cat.RenewLocalEmbeddingLeases(context.Background(), []catalog.LocalEmbeddingClaim{newClaim}, time.Now().Add(time.Hour)); err != nil || len(rows) != 1 {
				t.Fatalf("executor released newer token: %v %v", rows, err)
			}
		})
	}
}

type runVectorStore struct {
	*fakeVectorStore
	write func(context.Context)
}

func (s *runVectorStore) UpsertPhotos(ctx context.Context, records []qdrant.PhotoRecord) error {
	s.write(ctx)
	return s.fakeVectorStore.UpsertPhotos(ctx, records)
}

func TestEmbeddingExecutorCancellationAfterReservationSettlesSuccess(t *testing.T) {
	f := newEmbeddingRunFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.service.vectors = &runVectorStore{fakeVectorStore: f.vectors, write: func(writeCtx context.Context) {
		cancel()
		if writeCtx.Err() != nil {
			t.Fatal("reserved side effect inherited run cancellation")
		}
		claim := catalog.LocalEmbeddingClaim{Blob: catalog.Blob{Hash: f.item.Hash}, Token: f.item.ClaimToken}
		if rows, err := f.cat.ReleaseLocalEmbeddingClaims(context.Background(), []catalog.LocalEmbeddingClaim{claim}); err != nil || len(rows) != 0 {
			t.Fatalf("release bypassed reservation: %v %v", rows, err)
		}
	}}
	f.execute(t, ctx, runs.Outcome{Status: "succeeded"})
	f.assertBlob(t, "target", catalog.EmbeddingStatusReady)
	if points, _ := f.vectors.snapshot(); len(points) != 1 {
		t.Fatalf("reserved vector did not commit: %v", points)
	}
}

func TestEmbeddingExecutorValidatesResultsBeforeVectorWrites(t *testing.T) {
	for _, vector := range []struct {
		name string
		data []float32
	}{
		{"wrong-dimension", []float32{1}},
		{"zero-norm", vec768(0)},
		{"nan", vec768(float32(math.NaN()))},
		{"infinity", vec768(float32(math.Inf(1)))},
	} {
		t.Run(vector.name, func(t *testing.T) {
			f := newEmbeddingRunFixture(t)
			f.service.embedder = runEmbedder{embed: func(context.Context, []byte) ([]float32, error) { return vector.data, nil }}
			f.execute(t, context.Background(), runs.Outcome{Status: "failed", ErrorCode: "processing_failed"})
			f.assertNoVectors(t)
			f.assertBlob(t, "target", catalog.EmbeddingStatusPending)
		})
	}
}

func TestEmbeddingExecutorFailureClassificationAndSafeCodes(t *testing.T) {
	for _, failure := range []string{"load", "inference", "timeout", "vector-upload", "panic", "disabled-model"} {
		t.Run(failure, func(t *testing.T) {
			f := newEmbeddingRunFixture(t)
			secret := errors.New("secret-token https://private.example/image?credential=secret")
			want := runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}
			blobStatus := catalog.EmbeddingStatusPending
			switch failure {
			case "load":
				f.images.before = func(context.Context) error { return secret }
				blobStatus = catalog.EmbeddingStatusFailed
			case "inference":
				f.service.embedder = runEmbedder{embed: func(context.Context, []byte) ([]float32, error) { return nil, secret }}
				blobStatus = catalog.EmbeddingStatusFailed
			case "timeout":
				f.service.embedder = runEmbedder{embed: func(context.Context, []byte) ([]float32, error) { return nil, context.DeadlineExceeded }}
			case "vector-upload":
				f.vectors.upsertErr = secret
				want.ErrorCode = "executor_failed"
			case "panic":
				f.service.embedder = runEmbedder{embed: func(context.Context, []byte) ([]float32, error) { panic(secret) }}
				want.ErrorCode = "executor_panic"
			case "disabled-model":
				f.service.modelErr = secret
				want.ErrorCode = "executor_failed"
			}
			f.execute(t, context.Background(), want)
			f.assertBlob(t, "target", blobStatus)
			blob, err := f.cat.GetBlob(context.Background(), "target")
			if err != nil || (blob.EmbeddingError != "" && blob.EmbeddingError != "processing_failed") {
				t.Fatalf("unsafe blob error=%+v err=%v", blob, err)
			}
			if failure != "vector-upload" {
				f.assertNoVectors(t)
			}
		})
	}
}
