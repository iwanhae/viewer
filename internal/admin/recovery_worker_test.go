package admin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"viewer/internal/catalog"
)

func TestRecoveryWorkerShutdownRequeuesAndResumesDurably(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()
	hashes := make([]string, 500)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("worker-%05d", i)
		if err := store.UpsertBlob(ctx, catalog.Blob{Hash: hashes[i], SizeBytes: 1}); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := store.ClaimPendingEmbeddings(ctx, len(hashes), time.Now().Add(-time.Minute))
	if err != nil || len(claimed) != len(hashes) {
		t.Fatalf("claim failed recovery fixtures: claimed=%d err=%v", len(claimed), err)
	}
	results := make([]catalog.EmbeddingResult, 0, len(claimed))
	for _, blob := range claimed {
		results = append(results, catalog.EmbeddingResult{Hash: blob.Hash, Status: catalog.EmbeddingStatusFailed, Error: "test failure"})
	}
	if applied, rejected, err := store.ApplyEmbeddingResults(ctx, results); err != nil || len(applied) != len(hashes) || len(rejected) != 0 {
		t.Fatalf("seed failed embeddings: applied=%d rejected=%v err=%v", len(applied), rejected, err)
	}
	service := NewService(store, nil, nil)
	preview, err := service.CreateRecoveryPreview(ctx, "operator", catalog.RecoveryRetryFailedEmbedding, catalog.RecoverySelection{Hashes: hashes})
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err := service.CreateRecoveryOperation(ctx, "operator", preview.ID, preview.Action, "shutdown-test", "payload")
	if err != nil {
		t.Fatal(err)
	}

	workerCtx, cancel := context.WithCancel(context.Background())
	service.StartRecoveryWorker(workerCtx, false)
	time.Sleep(50 * time.Millisecond)
	paused, err := store.RecoveryOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "queued" || paused.Succeeded != 0 {
		t.Fatalf("disabled recovery worker changed queued operation: %+v", paused)
	}
	if claimedID, err := store.ClaimRecoveryOperation(ctx); err != nil || claimedID != operation.ID {
		t.Fatalf("simulate interrupted operation claim=%q err=%v want %q", claimedID, err, operation.ID)
	}
	service.StartRecoveryWorker(workerCtx, true)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.RecoveryOperation(ctx, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Succeeded > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, err := store.RecoveryOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Succeeded == 0 || current.Status == "completed" {
		t.Fatalf("worker did not leave resumable work: %+v", current)
	}
	cancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	if err := service.Close(closeCtx); err != nil {
		t.Fatalf("close worker: %v", err)
	}
	current, err = store.RecoveryOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "queued" || current.Succeeded == 0 || current.Succeeded >= current.Total {
		t.Fatalf("shutdown state=%+v want queued with durable partial progress", current)
	}

	resumed := NewService(store, nil, nil)
	resumeCtx, resumeCancel := context.WithCancel(context.Background())
	defer resumeCancel()
	resumed.StartRecoveryWorker(resumeCtx, true)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := resumed.Close(ctx); err != nil {
			t.Errorf("close resumed worker: %v", err)
		}
	}()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err = store.RecoveryOperation(ctx, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "completed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if current.Status != "completed" || current.Succeeded != current.Total {
		t.Fatalf("resumed operation=%+v want all %d targets completed", current, current.Total)
	}
}
