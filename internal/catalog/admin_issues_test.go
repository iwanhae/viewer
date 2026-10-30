package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAdminFailureCodeAndLeaseFiltersMatchCurrentRows(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	seedAdminBrowseCatalog(t, store)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='failed', embedding_status='failed'
		WHERE hash='shared'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET error_code='invalid_source'
		WHERE blob_hash='shared' AND stage='encoding'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET error_code='embedding_failed'
		WHERE blob_hash='shared' AND stage='embedding'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='leased', encoding_lease_until=? WHERE hash='blocked'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	encodingCode := "invalid_source"
	photos, err := store.AdminPhotos(ctx, AdminPhotoFilters{Encoding: "failed", EncodingErrorCode: encodingCode}, "", 0, 20)
	if err != nil || len(photos) != 3 || photos[0].EncodingErrorCode != encodingCode {
		t.Fatalf("encoding code photo filter rows=%+v err=%v", photos, err)
	}
	embeddingCode := "embedding_failed"
	photos, err = store.AdminPhotos(ctx, AdminPhotoFilters{EmbeddingErrorCode: embeddingCode}, "", 0, 20)
	if err != nil || len(photos) != 3 || photos[0].EmbeddingErrorCode != embeddingCode {
		t.Fatalf("embedding code photo filter rows=%+v err=%v", photos, err)
	}
	expired := true
	photos, err = store.AdminPhotos(ctx, AdminPhotoFilters{EncodingLeaseExpired: &expired}, "", 0, 20)
	if err != nil || len(photos) != 1 || photos[0].Hash != "blocked" {
		t.Fatalf("expired lease filter rows=%+v err=%v", photos, err)
	}
}

func TestAdminIssuesExplicitlyReportsTruncatedReasonGroups(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: "many-reasons", OriginalFilename: "many.zip", Status: AlbumStatusReady}); err != nil {
		t.Fatal(err)
	}
	runID, err := store.BeginExtractionRun(ctx, "many-reasons")
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]ExtractionEntryOutcome, 101)
	for i := range entries {
		entries[i] = ExtractionEntryOutcome{Ordinal: i, Name: fmt.Sprintf("entry-%03d.jpg", i), Candidate: true, Outcome: "failed", ReasonCode: fmt.Sprintf("reason_%03d", i)}
	}
	for start := 0; start < len(entries); start += ExtractionOutcomeBatchSize {
		end := start + ExtractionOutcomeBatchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := store.RecordExtractionEntries(ctx, runID, entries[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishExtractionRun(ctx, runID, "failed", "entry_failed", "", true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.AdminIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Groups) != maxAdminIssueGroups || snapshot.TotalGroups != 101 {
		t.Fatalf("bounded issue groups=%d total=%d; want first %d of 101", len(snapshot.Groups), snapshot.TotalGroups, maxAdminIssueGroups)
	}
}

func TestAdminIssuesLatestRunningExtractionRunSuppressesPriorTerminalFailure(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: "running-latest", OriginalFilename: "running.zip", Status: AlbumStatusReady}); err != nil {
		t.Fatal(err)
	}
	priorRun, err := store.BeginExtractionRun(ctx, "running-latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, priorRun, []ExtractionEntryOutcome{{
		Ordinal: 0, Name: "prior-failure.jpg", Candidate: true, Outcome: "failed", ReasonCode: "prior_failure",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, priorRun, "failed", "entry_failed", "", true); err != nil {
		t.Fatal(err)
	}
	latestRun, err := store.BeginExtractionRun(ctx, "running-latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, latestRun, []ExtractionEntryOutcome{{
		Ordinal: 0, Name: "in-progress-failure.jpg", Candidate: true, Outcome: "failed", ReasonCode: "in_progress_failure",
	}}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.AdminIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range snapshot.Groups {
		if group.Stage == "extraction_entry" {
			t.Fatalf("latest running run must suppress both its unfinished result and prior terminal failure: %+v", group)
		}
	}
	for _, reason := range []string{"prior_failure", "in_progress_failure"} {
		rows, err := store.AdminLatestExtractionFailures(ctx, reason, "", -1, 10)
		if err != nil || len(rows) != 0 {
			t.Fatalf("latest running run reason=%s rows=%+v err=%v", reason, rows, err)
		}
	}
}
