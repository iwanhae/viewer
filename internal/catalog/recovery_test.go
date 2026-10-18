package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func seedRecoveryBlob(t *testing.T, store *Store, hash, encoding, embedding string, encodingLease, embeddingLease int64) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("upsert %s: %v", hash, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, embedding_status=?, encoding_lease_until=?, embedding_lease_until=? WHERE hash=?`, encoding, embedding, encodingLease, embeddingLease, hash); err != nil {
		t.Fatalf("seed recovery state %s: %v", hash, err)
	}
}

func seedRecoveryAlbumPhoto(t *testing.T, store *Store, albumID string, idx int, hash string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: albumID, OriginalFilename: albumID + ".zip", Status: AlbumStatusReady}); err != nil {
		t.Fatalf("create album %s: %v", albumID, err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: albumID, Index: idx, Name: "photo.jpg", Hash: hash, Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo %s:%d: %v", albumID, idx, err)
	}
}

func recoveryOperationForTest(t *testing.T, store *Store, preview RecoveryPreview, actor, key string) RecoveryOperation {
	t.Helper()
	op, _, err := store.CreateRecoveryOperation(context.Background(), actor, preview.ID, preview.Action, key, "same-payload", time.Now())
	if err != nil {
		t.Fatalf("create recovery operation: %v", err)
	}
	return op
}

func drainRecoveryForTest(t *testing.T, store *Store, opID string) RecoveryOperation {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.ClaimRecoveryOperation(ctx)
	if err != nil || claimed != opID {
		t.Fatalf("claimed operation=%q err=%v want %q", claimed, err, opID)
	}
	for {
		didWork, err := store.ProcessRecoveryTarget(ctx, opID)
		if err != nil {
			t.Fatalf("process recovery target: %v", err)
		}
		if !didWork {
			break
		}
	}
	if _, err := store.FinishRecoveryOperation(ctx, opID); err != nil {
		t.Fatalf("finish recovery operation: %v", err)
	}
	op, err := store.RecoveryOperation(ctx, opID)
	if err != nil {
		t.Fatalf("read operation: %v", err)
	}
	return op
}

func TestRecoveryPreviewIsReadOnlyFrozenAndCountsSharedBlobImpact(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedRecoveryBlob(t, store, "shared", "failed", "failed", 0, 0)
	seedRecoveryBlob(t, store, "local", "pending", "failed", 0, 0)
	seedRecoveryAlbumPhoto(t, store, "album-a", 0, "shared")
	seedRecoveryAlbumPhoto(t, store, "album-a", 1, "local")
	seedRecoveryAlbumPhoto(t, store, "album-b", 0, "shared")
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEmbedding, RecoverySelection{Albums: []string{"album-a"}}, time.Now())
	if err != nil {
		t.Fatalf("create preview: %v", err)
	}
	if preview.TargetCount != 2 || preview.EligibleCount != 2 || preview.SkippedCount != 0 {
		t.Fatalf("preview target counts=%+v", preview)
	}
	if preview.SelectedPhotos != 2 || preview.AffectedPhotos != 3 || preview.AffectedAlbums != 2 || !preview.SharedBlobWarning {
		t.Fatalf("shared impact counts=%+v want selected=2 affected=3 albums=2 warning", preview)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT embedding_status FROM blobs WHERE hash='shared'`).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("preview mutated catalog status=%q err=%v", status, err)
	}
	// A newly matching blob is not added to the stored target set.
	seedRecoveryBlob(t, store, "later", "pending", "failed", 0, 0)
	op := recoveryOperationForTest(t, store, preview, "operator", "preview-frozen")
	completed := drainRecoveryForTest(t, store, op.ID)
	if completed.Status != "completed" || completed.Total != 2 || completed.Succeeded != 2 {
		t.Fatalf("operation summary=%+v", completed)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT embedding_status FROM blobs WHERE hash='later'`).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("later match was included: status=%q err=%v", status, err)
	}
	results, err := store.RecoveryResults(ctx, op.ID, 1, "")
	if err != nil || len(results.Results) != 1 || results.NextCursor == nil {
		t.Fatalf("paged results=%+v err=%v", results, err)
	}
	second, err := store.RecoveryResults(ctx, op.ID, 1, *results.NextCursor)
	if err != nil || len(second.Results) != 1 || second.Results[0].Ordinal <= results.Results[0].Ordinal {
		t.Fatalf("second result page=%+v err=%v", second, err)
	}
}

func TestRecoveryFilterSnapshotCapAndExpiry(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedRecoveryBlob(t, store, "filtered", "failed", "pending", 0, 0)
	seedRecoveryBlob(t, store, "not-filtered", "pending", "pending", 0, 0)
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEncoding,
		RecoverySelection{Filter: &AdminBlobFilters{Encoding: "failed"}}, time.Now())
	if err != nil || preview.TargetCount != 1 || preview.EligibleCount != 1 {
		t.Fatalf("filter preview=%+v err=%v", preview, err)
	}
	if _, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEmbedding,
		RecoverySelection{Hashes: make([]string, RecoveryTargetLimit+1)}, time.Now()); !errors.Is(err, ErrInvalidRecoverySelection) {
		t.Fatalf("oversized selection error=%v want ErrInvalidRecoverySelection", err)
	}
	if _, _, err := store.CreateRecoveryOperation(ctx, "operator", preview.ID, preview.Action, "expired", "same-payload", time.Now().Add(RecoveryPreviewTTL+time.Second)); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("expired preview consume err=%v want conflict", err)
	}
}

func TestRecoveryOperationsNewestFirstUsesFixedPrecisionCursor(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedRecoveryBlob(t, store, "operation-time", "pending", "failed", 0, 0)
	base := time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		base,
		base.Add(100 * time.Millisecond),
		base.Add(900 * time.Millisecond),
		base.Add(900 * time.Millisecond),
	}
	operations := make([]RecoveryOperation, 0, len(timestamps))
	for i, createdAt := range timestamps {
		preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEmbedding,
			RecoverySelection{Hashes: []string{"operation-time"}}, createdAt)
		if err != nil {
			t.Fatalf("create preview %d: %v", i, err)
		}
		operation, _, err := store.CreateRecoveryOperation(ctx, "operator", preview.ID, preview.Action,
			fmt.Sprintf("operation-time-%d", i), "payload", createdAt)
		if err != nil {
			t.Fatalf("create operation %d: %v", i, err)
		}
		operations = append(operations, operation)
	}
	if want := "2026-10-14T12:00:00.000000000Z"; operations[0].CreatedAt != want {
		t.Fatalf("zero-fraction operation timestamp=%q want fixed-width %q", operations[0].CreatedAt, want)
	}

	wantNewest := []string{operations[2].ID, operations[3].ID}
	if wantNewest[0] < wantNewest[1] {
		wantNewest[0], wantNewest[1] = wantNewest[1], wantNewest[0]
	}
	wantAll := append(wantNewest, operations[1].ID, operations[0].ID)
	first, err := store.RecoveryOperations(ctx, RecoveryOperationListOptions{Limit: 2})
	if err != nil || len(first.Operations) != 2 || first.NextCursor == nil {
		t.Fatalf("first operation page=%+v err=%v", first, err)
	}
	for i, want := range wantAll[:2] {
		if first.Operations[i].ID != want {
			t.Fatalf("first page item %d id=%q want %q", i, first.Operations[i].ID, want)
		}
	}
	cursorBytes, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatalf("decode next operation cursor: %v", err)
	}
	var cursorParts []string
	if err := json.Unmarshal(cursorBytes, &cursorParts); err != nil || len(cursorParts) != 2 {
		t.Fatalf("decode operation cursor parts=%q err=%v", cursorBytes, err)
	}
	second, err := store.RecoveryOperations(ctx, RecoveryOperationListOptions{
		Limit: 2, AfterCreated: cursorParts[0], AfterID: cursorParts[1],
	})
	if err != nil || len(second.Operations) != 2 {
		t.Fatalf("second operation page=%+v err=%v", second, err)
	}
	for i, want := range wantAll[2:] {
		if second.Operations[i].ID != want {
			t.Fatalf("second page item %d id=%q want %q", i, second.Operations[i].ID, want)
		}
	}
}

func TestRecoveryGenerationConflictDoesNotClobberReadyState(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedRecoveryBlob(t, store, "failed", "pending", "failed", 0, 0)
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEmbedding,
		RecoverySelection{Hashes: []string{"failed"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op := recoveryOperationForTest(t, store, preview, "operator", "generation-conflict")
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET embedding_status='ready' WHERE hash='failed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := observeStage(ctx, store.db, "failed", "embedding", "test_ready", "ready", "", time.Now(), false, true, false, "", "unknown"); err != nil {
		t.Fatal(err)
	}
	completed := drainRecoveryForTest(t, store, op.ID)
	if completed.Status != "failed" || completed.Conflicts != 1 || completed.Succeeded != 0 {
		t.Fatalf("operation=%+v want safe conflict", completed)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT embedding_status FROM blobs WHERE hash='failed'`).Scan(&status); err != nil || status != "ready" {
		t.Fatalf("ready generation clobbered status=%q err=%v", status, err)
	}
}

func TestRecoveryPartialResultsAggregateConflictAndSuccess(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedRecoveryBlob(t, store, "a-success", "pending", "failed", 0, 0)
	seedRecoveryBlob(t, store, "b-conflict", "pending", "failed", 0, 0)
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEmbedding,
		RecoverySelection{Hashes: []string{"a-success", "b-conflict"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op := recoveryOperationForTest(t, store, preview, "operator", "partial-results")
	if id, err := store.ClaimRecoveryOperation(ctx); err != nil || id != op.ID {
		t.Fatalf("claim=%q err=%v", id, err)
	}
	if did, err := store.ProcessRecoveryTarget(ctx, op.ID); err != nil || !did {
		t.Fatalf("process first target did=%v err=%v", did, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET embedding_status='ready' WHERE hash='b-conflict'`); err != nil {
		t.Fatal(err)
	}
	if _, err := observeStage(ctx, store.db, "b-conflict", "embedding", "test_ready", "ready", "", time.Now(), false, true, false, "", "unknown"); err != nil {
		t.Fatal(err)
	}
	for {
		did, err := store.ProcessRecoveryTarget(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !did {
			break
		}
	}
	if _, err := store.FinishRecoveryOperation(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := store.RecoveryOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "partially_failed" || completed.Succeeded != 1 || completed.Conflicts != 1 {
		t.Fatalf("partial operation=%+v want one success and one safe conflict", completed)
	}
}

func TestRecoveryRetryFailedEncodingAndEmbeddingLeaseRelease(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedRecoveryBlob(t, store, "failed-encoding", "failed", "pending", 0, 0)
	seedRecoveryBlob(t, store, "not-smaller", "skipped", "pending", 0, 0)
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_error='old error' WHERE hash='failed-encoding'`); err != nil {
		t.Fatal(err)
	}
	encodingPreview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEncoding,
		RecoverySelection{Hashes: []string{"failed-encoding", "not-smaller"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if encodingPreview.EligibleCount != 1 || encodingPreview.SkippedReasons["not_smaller"] != 1 {
		t.Fatalf("encoding preview=%+v", encodingPreview)
	}
	encodingOp := recoveryOperationForTest(t, store, encodingPreview, "operator", "retry-encoding")
	if done := drainRecoveryForTest(t, store, encodingOp.ID); done.Succeeded != 1 || done.Skipped != 1 {
		t.Fatalf("encoding operation=%+v", done)
	}
	var encodingStatus, encodingError string
	if err := store.db.QueryRowContext(ctx, `SELECT encoding_status,encoding_error FROM blobs WHERE hash='failed-encoding'`).Scan(&encodingStatus, &encodingError); err != nil || encodingStatus != "pending" || encodingError != "" {
		t.Fatalf("retried encoding state=(%s,%q) err=%v", encodingStatus, encodingError, err)
	}

	now := time.Now()
	seedRecoveryBlob(t, store, "embed-expired", "pending", "processing", 0, now.Add(-time.Minute).UnixMilli())
	seedRecoveryBlob(t, store, "embed-live", "pending", "processing", 0, now.Add(time.Hour).UnixMilli())
	embedPreview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryReleaseEmbedding,
		RecoverySelection{Hashes: []string{"embed-expired", "embed-live"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if embedPreview.EligibleCount != 1 || embedPreview.SkippedReasons["lease_live"] != 1 {
		t.Fatalf("embedding lease preview=%+v", embedPreview)
	}
	embedOp := recoveryOperationForTest(t, store, embedPreview, "operator", "release-embedding")
	if done := drainRecoveryForTest(t, store, embedOp.ID); done.Succeeded != 1 || done.Skipped != 1 {
		t.Fatalf("embedding lease operation=%+v", done)
	}
	var expiredStatus, liveStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT embedding_status FROM blobs WHERE hash='embed-expired'`).Scan(&expiredStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT embedding_status FROM blobs WHERE hash='embed-live'`).Scan(&liveStatus); err != nil {
		t.Fatal(err)
	}
	if expiredStatus != "pending" || liveStatus != "processing" {
		t.Fatalf("embedding statuses expired=%s live=%s", expiredStatus, liveStatus)
	}
}

func TestRecoveryFilterRejectsOversizedTargetSetWithoutTruncation(t *testing.T) {
	store := openTestStore(t)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= RecoveryTargetLimit; i++ {
		if _, err := tx.Exec(`INSERT INTO blobs(hash,size_bytes,content_type,embedding_status,encoding_status,created_at) VALUES (?,1,'image/jpeg','pending','pending',?)`, fmt.Sprintf("cap-%05d", i), observationTime(time.Now())); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateRecoveryPreview(context.Background(), "operator", RecoveryRetryFailedEmbedding,
		RecoverySelection{Filter: &AdminBlobFilters{}}, time.Now())
	if !errors.Is(err, ErrInvalidRecoverySelection) {
		t.Fatalf("oversized filter error=%v want explicit reject", err)
	}
}

func TestRecoveryExpiredEncodingNeverResetsLiveReceivedOrCommitting(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, hash := range []string{"expired", "live", "received", "committing"} {
		seedRecoveryBlob(t, store, hash, "pending", "pending", 0, 0)
	}
	for hash, status := range map[string]string{"expired": "leased", "live": "leased", "received": "received", "committing": "committing"} {
		lease := now.Add(-time.Minute).UnixMilli()
		if hash == "live" {
			lease = now.Add(time.Hour).UnixMilli()
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, encoding_token=?, encoding_stage_key=?, encoding_lease_until=?, encoding_source_size_bytes=? WHERE hash=?`,
			status, "secret-"+hash, "staging/"+hash, lease, 789, hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET phase='reconciling' WHERE blob_hash='committing' AND stage='encoding'`); err != nil {
		t.Fatal(err)
	}
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryReleaseEncoding,
		RecoverySelection{Hashes: []string{"expired", "live", "received", "committing"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if preview.EligibleCount != 1 || preview.SkippedCount != 3 || preview.SkippedReasons["lease_live"] != 1 || preview.SkippedReasons["unsafe_phase"]+preview.SkippedReasons["reconciling"] != 2 {
		t.Fatalf("lease preview=%+v", preview)
	}
	op := recoveryOperationForTest(t, store, preview, "operator", "release-expired")
	completed := drainRecoveryForTest(t, store, op.ID)
	if completed.Succeeded != 1 || completed.Skipped != 3 {
		t.Fatalf("operation=%+v", completed)
	}
	for hash, want := range map[string]string{"expired": "pending", "live": "leased", "received": "received", "committing": "committing"} {
		var status, token, stage string
		var lease, source int64
		if err := store.db.QueryRowContext(ctx, `SELECT encoding_status,encoding_token,encoding_stage_key,encoding_lease_until,encoding_source_size_bytes FROM blobs WHERE hash=?`, hash).
			Scan(&status, &token, &stage, &lease, &source); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("%s status=%s want=%s", hash, status, want)
		}
		if hash == "expired" && (token != "" || stage != "" || lease != 0 || source != 789) {
			t.Fatalf("expired lease secrets/bookkeeping not cleared: %q %q %d %d", token, stage, lease, source)
		}
		if hash != "expired" && (token != "secret-"+hash || stage != "staging/"+hash || source != 789) {
			t.Fatalf("unsafe target %s changed: %q %q %d", hash, token, stage, source)
		}
	}
}

func TestRecoveryEncodingReleaseSkipsLocalAndReleasesExternal(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	lease := now.Add(-time.Minute).UnixMilli()
	for _, owner := range []string{"local", "external"} {
		seedRecoveryBlob(t, store, owner, "leased", "pending", lease, 0)
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_owner=?,encoding_token=?,encoding_stage_key=? WHERE hash=?`, owner, "secret-"+owner, "staging/"+owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.GetEncodingJob(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryReleaseEncoding,
		RecoverySelection{Hashes: []string{"local", "external"}}, now)
	if err != nil || preview.EligibleCount != 1 || preview.SkippedCount != 1 || preview.SkippedReasons["local_owned"] != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	op := recoveryOperationForTest(t, store, preview, "operator", "release-external-only")
	if completed := drainRecoveryForTest(t, store, op.ID); completed.Succeeded != 1 || completed.Skipped != 1 {
		t.Fatalf("operation=%+v", completed)
	}
	results, err := store.RecoveryResults(ctx, op.ID, 10, "")
	if err != nil || len(results.Results) != 2 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	for _, result := range results.Results {
		if result.Hash == "local" && (result.Status != "skipped" || result.Reason != "local_owned") {
			t.Fatalf("local result=%+v", result)
		}
	}
	after, err := store.GetEncodingJob(ctx, "local")
	if err != nil || after != before {
		t.Fatalf("local lease changed: before=%+v after=%+v err=%v", before, after, err)
	}
	external, err := store.GetEncodingJob(ctx, "external")
	if err != nil || external.Status != "pending" || external.Owner != "external" || external.Token != "" || external.StageKey != "" || external.LeaseUntil.UnixMilli() != 0 {
		t.Fatalf("external lease was not released: %+v err=%v", external, err)
	}
}

func TestRecoveryEncodingReleaseFencesOwnerChangedAfterPreview(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	seedRecoveryBlob(t, store, "target", "leased", "pending", now.Add(-time.Minute).UnixMilli(), 0)
	before, err := readRecoveryTargetState(ctx, store.db, "target", "encoding")
	if err != nil || before.EncodingOwner != "external" {
		t.Fatalf("external state=%+v err=%v", before, err)
	}
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryReleaseEncoding,
		RecoverySelection{Hashes: []string{"target"}}, now)
	if err != nil || preview.EligibleCount != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	op := recoveryOperationForTest(t, store, preview, "operator", "owner-changed")
	// Change only ownership, retaining the token, lease and observations so
	// ownership itself must invalidate the preview generation.
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_owner='local' WHERE hash='target'`); err != nil {
		t.Fatal(err)
	}
	after, err := readRecoveryTargetState(ctx, store.db, "target", "encoding")
	if err != nil || after.EncodingOwner != "local" || recoveryGeneration(RecoveryReleaseEncoding, before) == recoveryGeneration(RecoveryReleaseEncoding, after) {
		t.Fatalf("owner change did not invalidate generation: before=%+v after=%+v err=%v", before, after, err)
	}
	// Also exercise the SQL guard directly with the stale external state; a
	// caller cannot bypass ownership fencing by skipping the generation check.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, applyErr := applyRecoveryTransition(ctx, tx, RecoveryReleaseEncoding, "target", before, now)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if applyErr != nil || changed {
		t.Fatalf("SQL released local lease with stale state: changed=%v err=%v", changed, applyErr)
	}
	if completed := drainRecoveryForTest(t, store, op.ID); completed.Conflicts != 1 || completed.Succeeded != 0 {
		t.Fatalf("operation=%+v", completed)
	}
	current, err := readRecoveryTargetState(ctx, store.db, "target", "encoding")
	if err != nil || current != after {
		t.Fatalf("local state changed: before=%+v after=%+v err=%v", after, current, err)
	}
}

func TestRecoveryIdempotencyConcurrencyAndRestartResume(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	store, err := Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"one", "two"} {
		seedRecoveryBlob(t, store, hash, "pending", "failed", 0, 0)
	}
	preview, err := store.CreateRecoveryPreview(ctx, "operator", RecoveryRetryFailedEmbedding,
		RecoverySelection{Hashes: []string{"one", "two"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payloadHash := "stable-payload"
	var wg sync.WaitGroup
	ops := make([]RecoveryOperation, 2)
	errs := make([]error, 2)
	for i := range ops {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ops[i], _, errs[i] = store.CreateRecoveryOperation(ctx, "operator", preview.ID, preview.Action, "same-key", payloadHash, time.Now())
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent idempotent create: %v", err)
		}
	}
	if ops[0].ID != ops[1].ID {
		t.Fatalf("concurrent calls created different operations: %+v", ops)
	}
	if _, _, err := store.CreateRecoveryOperation(ctx, "operator", preview.ID, preview.Action, "same-key", "different-payload", time.Now()); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("mismatched idempotency binding err=%v want conflict", err)
	}
	opID := ops[0].ID
	if id, err := store.ClaimRecoveryOperation(ctx); err != nil || id != opID {
		t.Fatalf("claim=%q err=%v", id, err)
	}
	if did, err := store.ProcessRecoveryTarget(ctx, opID); err != nil || !did {
		t.Fatalf("first target did=%v err=%v", did, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	resumed, err := store.RecoveryOperation(ctx, opID)
	if err != nil || resumed.Status != "queued" || resumed.Succeeded != 1 {
		t.Fatalf("resumed operation=%+v err=%v", resumed, err)
	}
	completed := drainRecoveryForTest(t, store, opID)
	if completed.Status != "completed" || completed.Succeeded != 2 {
		t.Fatalf("completed after restart=%+v", completed)
	}
}
