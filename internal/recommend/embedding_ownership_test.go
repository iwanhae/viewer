package recommend

import (
	"context"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/qdrant"
)

func seedOwnedPhoto(t *testing.T, svc *Service) catalog.LocalEmbeddingClaim {
	t.Helper()
	seedBlob(t, svc.catalog, "owned")
	seedAlbum(t, svc.catalog, "album", catalog.Photo{Index: 0, Name: "photo.jpg", Hash: "owned", Width: 1, Height: 1, Ratio: 1})
	claims, _, err := svc.ClaimLocalEmbeddings(context.Background(), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("local claim=%v err=%v", claims, err)
	}
	return claims[0]
}

func readyLocalResult(claim catalog.LocalEmbeddingClaim) catalog.LocalEmbeddingResult {
	return catalog.LocalEmbeddingResult{Token: claim.Token, EmbeddingResult: catalog.EmbeddingResult{
		Hash: claim.Hash, Status: catalog.EmbeddingStatusReady, Vector: vec768(1),
	}}
}

func TestLocalEmbeddingFencingBeforeVectorStore(t *testing.T) {
	for _, state := range []string{"wrong-token", "expired", "cancelled", "new-local", "new-external"} {
		t.Run(state, func(t *testing.T) {
			cat := newTestCatalog(t)
			vectors := newFakeVectorStore()
			svc := newTestService(t, cat, vectors, nil)
			claim := seedOwnedPhoto(t, svc)
			ctx := context.Background()
			switch state {
			case "wrong-token":
				claim.Token = "wrong-token"
			case "expired":
				if rows, err := cat.RenewLocalEmbeddingLeases(ctx, []catalog.LocalEmbeddingClaim{claim}, time.Now().Add(-time.Hour)); err != nil || len(rows) != 1 {
					t.Fatalf("expire=%v err=%v", rows, err)
				}
			default:
				if rows, err := svc.ReleaseLocalClaims(ctx, []catalog.LocalEmbeddingClaim{claim}); err != nil || len(rows) != 1 {
					t.Fatalf("cancel=%v err=%v", rows, err)
				}
				if state == "new-local" {
					if rows, _, err := svc.ClaimLocalEmbeddings(ctx, 1); err != nil || len(rows) != 1 {
						t.Fatalf("new local=%v err=%v", rows, err)
					}
				}
				if state == "new-external" {
					if rows, _, err := svc.ClaimExternalEmbeddings(ctx, 1); err != nil || len(rows) != 1 {
						t.Fatalf("new external=%v err=%v", rows, err)
					}
				}
			}
			if n, rejected, err := svc.ApplyLocalEmbeddingResults(ctx, []catalog.LocalEmbeddingResult{readyLocalResult(claim)}); err != nil || n != 0 || len(rejected) != 1 || rejected[0].Reason != catalog.EmbeddingRejectNotClaimed {
				t.Fatalf("stale apply=%d rejected=%v err=%v", n, rejected, err)
			}
			points, ops := vectors.snapshot()
			if len(ops) != 0 || len(points) != 0 {
				t.Fatalf("stale result reached vector store: points=%d ops=%v", len(points), ops)
			}
		})
	}
}

func TestExternalEmbeddingCannotWriteLocalOwner(t *testing.T) {
	cat := newTestCatalog(t)
	vectors := newFakeVectorStore()
	svc := newTestService(t, cat, vectors, nil)
	claim := seedOwnedPhoto(t, svc)
	ctx := context.Background()
	// Mix a local-owned hash with an external hash, retaining batch partial success.
	seedBlob(t, cat, "external")
	seedAlbum(t, cat, "external-album", catalog.Photo{Index: 0, Name: "photo.jpg", Hash: "external", Width: 1, Height: 1, Ratio: 1})
	if rows, _, err := svc.ClaimExternalEmbeddings(ctx, 8); err != nil || len(rows) != 1 || rows[0].Hash != "external" {
		t.Fatalf("external claim=%v err=%v", rows, err)
	}
	if n, rejected, err := svc.ApplyExternalEmbeddingResults(ctx, []catalog.EmbeddingResult{
		readyLocalResult(claim).EmbeddingResult,
		{Hash: "external", Status: catalog.EmbeddingStatusReady, Vector: vec768(2)},
	}); err != nil || n != 1 || len(rejected) != 1 || rejected[0].Hash != claim.Hash || rejected[0].Reason != catalog.EmbeddingRejectNotClaimed {
		t.Fatalf("external batch=%d rejected=%v err=%v", n, rejected, err)
	}
	points, ops := vectors.snapshot()
	if len(points) != 1 || len(ops) != 1 {
		t.Fatalf("unexpected vector writes: points=%d ops=%v", len(points), ops)
	}
	for _, point := range points {
		if point.Hash != "external" {
			t.Fatal("external result wrote local-owned vector")
		}
	}
	if n, rejected, err := svc.ApplyLocalEmbeddingResults(ctx, []catalog.LocalEmbeddingResult{readyLocalResult(claim)}); err != nil || n != 1 || len(rejected) != 0 {
		t.Fatalf("local apply=%d rejected=%v err=%v", n, rejected, err)
	}
}

type blockedEmbeddingVectorStore struct {
	*fakeVectorStore
	entered chan struct{}
	resume  chan struct{}
}

func (s *blockedEmbeddingVectorStore) UpsertPhotos(ctx context.Context, records []qdrant.PhotoRecord) error {
	close(s.entered)
	<-s.resume
	return s.fakeVectorStore.UpsertPhotos(ctx, records)
}

func TestLocalEmbeddingCancellationDuringVectorWrite(t *testing.T) {
	cat := newTestCatalog(t)
	vectors := &blockedEmbeddingVectorStore{fakeVectorStore: newFakeVectorStore(), entered: make(chan struct{}), resume: make(chan struct{})}
	svc := newTestService(t, cat, vectors, nil)
	claim := seedOwnedPhoto(t, svc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		n        int
		rejected []catalog.RejectedEmbedding
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		n, rejected, err := svc.ApplyLocalEmbeddingResults(ctx, []catalog.LocalEmbeddingResult{readyLocalResult(claim)})
		done <- outcome{n, rejected, err}
	}()
	// Always unblock the writer, including when an assertion fails.
	resumed := false
	defer func() {
		if !resumed {
			close(vectors.resume)
		}
	}()
	select {
	case <-vectors.entered:
	case <-ctx.Done():
		t.Fatal("vector write did not start")
	}
	if released, err := svc.ReleaseLocalClaims(ctx, []catalog.LocalEmbeddingClaim{claim}); err != nil || len(released) != 0 {
		t.Fatalf("cancellation must not win after reservation: %v %v", released, err)
	}
	if n, err := cat.ReleaseStuckEmbeddingClaims(ctx, false, nil); err != nil || n != 0 {
		t.Fatalf("recovery bypassed reservation: %d %v", n, err)
	}
	if claims, _, err := svc.ClaimExternalEmbeddings(ctx, 1); err != nil || len(claims) != 0 {
		t.Fatalf("external stole reserved claim: %v %v", claims, err)
	}
	close(vectors.resume)
	resumed = true
	select {
	case result := <-done:
		if result.err != nil || result.n != 1 || len(result.rejected) != 0 {
			t.Fatalf("finalization=%+v", result)
		}
	case <-ctx.Done():
		t.Fatal("finalization stalled")
	}
	points, ops := vectors.snapshot()
	if len(points) != 1 || len(ops) != 1 {
		t.Fatalf("vector write=%d ops=%v", len(points), ops)
	}
}
