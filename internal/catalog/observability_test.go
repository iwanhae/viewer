package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEncodingObservationsFenceStaleFinishesAndTrackReconciliation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "observed-encoding", SizeBytes: 100, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: jobs=%+v err=%v", jobs, err)
	}
	job := jobs[0]
	if ok, err := store.BeginEncodingCommit(ctx, job.Hash, job.Token, 100, time.Hour); err != nil || !ok {
		t.Fatalf("begin commit: ok=%v err=%v", ok, err)
	}
	if counts, err := store.ResetStuckEncodings(ctx, nil); err != nil || counts != (EncodingResetCounts{}) {
		t.Fatalf("reset must leave a committing row fenced: counts=%+v err=%v", counts, err)
	}
	if ok, err := store.MarkEncodingReconciling(ctx, job.Hash, job.Token); err != nil || !ok {
		t.Fatalf("mark reconciliation: ok=%v err=%v", ok, err)
	}
	if ok, err := store.FinishEncoding(ctx, job.Hash, "stale-token", "done", "image/webp", "", 50); err != nil || ok {
		t.Fatalf("stale finish: ok=%v err=%v, want rejected", ok, err)
	}
	if ok, err := store.FinishReconciledEncoding(ctx, job.Hash, job.Token, "done", "image/webp", "", 50); err != nil || !ok {
		t.Fatalf("reconciled finish: ok=%v err=%v", ok, err)
	}
	if ok, err := store.FinishEncoding(ctx, job.Hash, job.Token, "done", "image/webp", "", 50); err != nil || ok {
		t.Fatalf("duplicate stale finish: ok=%v err=%v, want rejected", ok, err)
	}

	observations, err := store.StageObservations(ctx, job.Hash)
	if err != nil {
		t.Fatal(err)
	}
	got := observations["encoding"]
	if got.Attempts != 1 || got.StartedAt == nil || got.FinishedAt == nil || got.StatusChangedAt == nil || got.Status != "done" || got.Phase != nil {
		t.Fatalf("final encoding observation=%+v", got)
	}
	page, err := store.PipelineHistory(ctx, job.Hash, "encoding", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, event := range page.Events {
		names = append(names, event.Event)
		if event.AttemptID != 1 {
			t.Fatalf("event attempt ID=%d want 1: %+v", event.AttemptID, event)
		}
	}
	want := []string{"claimed", "commit_started", "reconciliation_started", "reconciled_success"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want=%v", names, want)
	}

	// Pagination is based on monotonically increasing IDs and never exceeds its cap.
	first, err := store.PipelineHistory(ctx, job.Hash, "", 0, 2)
	if err != nil || len(first.Events) != 2 || first.NextCursor == nil {
		t.Fatalf("first history page=%+v err=%v", first, err)
	}
	second, err := store.PipelineHistory(ctx, job.Hash, "", mustParseEventID(t, *first.NextCursor), 2)
	if err != nil || len(second.Events) != 2 || second.Events[0].ID <= first.Events[1].ID {
		t.Fatalf("second history page=%+v err=%v", second, err)
	}
	if _, err := store.PipelineHistory(ctx, job.Hash, "", 0, 101); err == nil {
		t.Fatal("history page over 100 unexpectedly succeeded")
	}
}

func TestExpiredEncodingClaimCreatesNewAttemptAndRejectsOldToken(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "expired-encoding", SizeBytes: 100, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	oldJobs, err := store.ClaimEncoding(ctx, 1, -time.Second)
	if err != nil || len(oldJobs) != 1 {
		t.Fatalf("initial claim=%+v err=%v", oldJobs, err)
	}
	newJobs, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(newJobs) != 1 {
		t.Fatalf("reclaimed claim=%+v err=%v", newJobs, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, newJobs[0].Hash, oldJobs[0].Token, 100, time.Minute); err != nil || ok {
		t.Fatalf("old token begin commit: ok=%v err=%v, want rejected", ok, err)
	}
	if ok, err := store.FinishEncoding(ctx, newJobs[0].Hash, oldJobs[0].Token, "done", "image/webp", "", 50); err != nil || ok {
		t.Fatalf("old token finish: ok=%v err=%v, want rejected", ok, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, newJobs[0].Hash, newJobs[0].Token, 100, time.Minute); err != nil || !ok {
		t.Fatalf("current token begin commit: ok=%v err=%v", ok, err)
	}
	if ok, err := store.FinishEncoding(ctx, newJobs[0].Hash, newJobs[0].Token, "done", "image/webp", "", 50); err != nil || !ok {
		t.Fatalf("current token finish: ok=%v err=%v", ok, err)
	}
	observations, err := store.StageObservations(ctx, "expired-encoding")
	if err != nil {
		t.Fatal(err)
	}
	if observations["encoding"].Attempts != 2 || observations["encoding"].Status != "done" {
		t.Fatalf("encoding observation=%+v", observations["encoding"])
	}
	history, err := store.PipelineHistory(ctx, "expired-encoding", "encoding", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claimed", "lease_expired", "claimed", "commit_started", "done"}
	if len(history.Events) != len(want) {
		t.Fatalf("encoding events=%+v", history.Events)
	}
	for i, event := range history.Events {
		if event.Event != want[i] {
			t.Fatalf("event[%d]=%q want %q (all=%+v)", i, event.Event, want[i], history.Events)
		}
	}
}

func TestClaimEncodingReservesWriterBeforeSelectingCandidates(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	store, err := Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	other, err := Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := store.UpsertBlob(ctx, Blob{Hash: "writer-lock", SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='failed' WHERE hash='writer-lock'`); err != nil {
		t.Fatal(err)
	}
	blocker, err := other.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `UPDATE blobs SET encoding_status='pending' WHERE hash='writer-lock'`); err != nil {
		_ = blocker.Rollback()
		t.Fatal(err)
	}
	type claimResult struct {
		jobs []EncodingJob
		err  error
	}
	started := make(chan struct{})
	result := make(chan claimResult, 1)
	go func() {
		close(started)
		jobs, err := store.ClaimEncoding(ctx, 1, time.Minute)
		result <- claimResult{jobs: jobs, err: err}
	}()
	<-started
	select {
	case got := <-result:
		_ = blocker.Rollback()
		t.Fatalf("claim completed before competing writer released: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || len(got.jobs) != 1 || got.jobs[0].Hash != "writer-lock" {
			t.Fatalf("claim after writer release=%+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("claim did not finish after competing writer released")
	}
}

func TestSourceRestorationResetsOnlyEncodingObservation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "restored-source", SizeBytes: 100, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim=%+v err=%v", jobs, err)
	}
	if ok, err := store.SkipEncoding(ctx, jobs[0].Hash, jobs[0].Token, "failed", "image/jpeg", "invalid source blob for encoding", 100); err != nil || !ok {
		t.Fatalf("mark failure: ok=%v err=%v", ok, err)
	}
	if err := store.UpsertBlob(ctx, Blob{Hash: jobs[0].Hash, SizeBytes: 100, ContentType: "image/jpeg", SourceRestored: true}); err != nil {
		t.Fatal(err)
	}
	observations, err := store.StageObservations(ctx, jobs[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	if observations["encoding"].Status != "pending" || observations["encoding"].ErrorCode != "" || observations["encoding"].Attempts != 1 {
		t.Fatalf("restored encoding observation=%+v", observations["encoding"])
	}
	if observations["embedding"].Status != "pending" || observations["embedding"].Attempts != 0 || observations["embedding"].StatusChangedAt != nil {
		t.Fatalf("source restoration changed embedding observation=%+v", observations["embedding"])
	}
	history, err := store.PipelineHistory(ctx, jobs[0].Hash, "encoding", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if history.Events[len(history.Events)-1].Event != "source_restored" {
		t.Fatalf("source restoration event=%+v", history.Events)
	}
}

func TestEmbeddingRetryRetainsSafePriorErrorAndExpiredClaimIsObserved(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "observed-embedding", SizeBytes: 100, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimPendingEmbeddings(ctx, 1, time.Now().Add(time.Minute))
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	if applied, rejected, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{{
		Hash: first[0].Hash, Status: EmbeddingStatusFailed,
		Error: "embed image: authorization=do-not-copy https://example.test/path?token=private",
	}}); err != nil || len(applied) != 1 || len(rejected) != 0 {
		t.Fatalf("failed result applied=%v rejected=%v err=%v", applied, rejected, err)
	}
	if reset, err := store.ResetFailedEmbeddings(ctx, nil); err != nil || reset != 1 {
		t.Fatalf("manual retry reset=%d err=%v", reset, err)
	}
	second, err := store.ClaimPendingEmbeddings(ctx, 1, time.Now().Add(time.Minute))
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim=%+v err=%v", second, err)
	}
	observations, err := store.StageObservations(ctx, first[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	got := observations["embedding"]
	if got.Attempts != 2 || got.ErrorCode != "embedding_failed" || got.StartedAt == nil || got.FinishedAt != nil || got.Retryability != "unknown" {
		t.Fatalf("retry observation=%+v", got)
	}
	page, err := store.PipelineHistory(ctx, first[0].Hash, "embedding", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var retryClaim bool
	for _, event := range page.Events {
		if event.Event == "manual_retry" && event.ErrorCode == "embedding_failed" {
			retryClaim = true
		}
		if strings.Contains(event.ErrorCode, "private") || strings.Contains(event.ErrorCode, "example.test") {
			t.Fatalf("unsafe error data in event: %+v", event)
		}
	}
	if !retryClaim {
		t.Fatalf("missing retry event: %+v", page.Events)
	}
	if applied, rejected, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{{
		Hash: first[0].Hash, Status: EmbeddingStatusReady, Vector: vec768(1),
	}}); err != nil || len(applied) != 1 || len(rejected) != 0 {
		t.Fatalf("successful retry applied=%v rejected=%v err=%v", applied, rejected, err)
	}
	page, err = store.PipelineHistory(ctx, first[0].Hash, "embedding", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := page.Events[len(page.Events)-1]
	if last.Event != string(EmbeddingStatusReady) || last.ErrorCode != "" {
		t.Fatalf("success event retained prior error: %+v", last)
	}
	var rawEventData string
	if err := store.db.QueryRowContext(ctx, `SELECT group_concat(event || ':' || error_code || ':' || status || ':' || COALESCE(phase,''))
		FROM pipeline_events WHERE blob_hash=?`, first[0].Hash).Scan(&rawEventData); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawEventData, "do-not-copy") || strings.Contains(rawEventData, "private") || strings.Contains(rawEventData, "example.test") {
		t.Fatalf("raw error leaked into event storage: %q", rawEventData)
	}
	if err := store.UpsertBlob(ctx, Blob{Hash: "expired-embedding", SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	if expired, err := store.ClaimPendingEmbeddings(ctx, 1, time.Now().Add(-time.Second)); err != nil || len(expired) != 1 {
		t.Fatalf("expired claim=%+v err=%v", expired, err)
	}
	if reclaimed, err := store.ClaimPendingEmbeddings(ctx, 1, time.Now().Add(time.Minute)); err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaimed claim=%+v err=%v", reclaimed, err)
	}
	expiryHistory, err := store.PipelineHistory(ctx, "expired-embedding", "embedding", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(expiryHistory.Events) != 3 || expiryHistory.Events[1].Event != "lease_expired" {
		t.Fatalf("expired claim history=%+v", expiryHistory.Events)
	}
}

func TestPipelineHistoryVisibilityAndOpportunisticPruning(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const hash = "history-retention"
	if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	oldAt := observationTime(now.Add(-EventHistoryVisibility - time.Hour))
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO pipeline_events (blob_hash, stage, event, status, attempt_id, occurred_at)
		VALUES (?, 'encoding', 'old', 'done', 0, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if _, err := stmt.ExecContext(ctx, hash, oldAt); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := store.PipelineHistory(ctx, hash, "", 0, 100)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("idle history page=%+v err=%v, want no visible expired events", page, err)
	}
	var oldCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_events WHERE occurred_at=?`, oldAt).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 600 {
		t.Fatalf("idle history read physically pruned rows: count=%d want 600", oldCount)
	}
	for i := 0; i < 100; i++ {
		if _, err := observeStage(ctx, store.db, hash, "encoding", "maintenance", "done", "", now, false, false, false, "", "unknown"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_events WHERE occurred_at=?`, oldAt).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 100 {
		t.Fatalf("periodic pruning removed %d expired rows; want bounded batch of 500 and 100 remaining", 600-oldCount)
	}
	page, err = store.PipelineHistory(ctx, hash, "", 0, 100)
	if err != nil || len(page.Events) != 100 {
		t.Fatalf("visible history after writes has %d events, err=%v; want 100 recent events", len(page.Events), err)
	}
	if _, err := store.PipelineHistory(ctx, "absent-history-blob", "", 0, 100); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("missing-blob history error=%v, want ErrBlobNotFound", err)
	}
}

func mustParseEventID(t *testing.T, value string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
