package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAdminDiagnosticsUsesOnlyRecordedObservationTimes(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	ctx := context.Background()
	for _, blob := range []Blob{
		{Hash: "queued-known", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "queued-unknown", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "queued-malformed", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "expired-encoding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "expired-embedding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "committing", SizeBytes: 100, ContentType: "image/jpeg"},
	} {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatal(err)
		}
	}
	job, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(job) != 1 || job[0].Hash != "committing" {
		t.Fatalf("claim committing: jobs=%+v err=%v", job, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, job[0].Hash, job[0].Token, 100, time.Minute); err != nil || !ok {
		t.Fatalf("begin commit: ok=%v err=%v", ok, err)
	}
	if ok, err := store.MarkEncodingReconciling(ctx, job[0].Hash, job[0].Token); err != nil || !ok {
		t.Fatalf("mark reconciling: ok=%v err=%v", ok, err)
	}

	knownAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET status_changed_at=?, status='pending'
		WHERE blob_hash='queued-known' AND stage='embedding'`, knownAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET status_changed_at=NULL
		WHERE blob_hash='queued-unknown' AND stage='embedding'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET status_changed_at='not-a-time'
		WHERE blob_hash='queued-malformed' AND stage='embedding'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='leased', encoding_lease_until=? WHERE hash='expired-encoding'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET embedding_status='processing', embedding_lease_until=? WHERE hash='expired-embedding'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	diagnostics, err := store.AdminDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.EncodingExpiredLeases != 1 || diagnostics.EmbeddingExpiredLeases != 1 {
		t.Fatalf("expired leases=%d/%d want 1/1", diagnostics.EncodingExpiredLeases, diagnostics.EmbeddingExpiredLeases)
	}
	if diagnostics.EmbeddingQueue.Count != 5 || diagnostics.EmbeddingQueue.UnknownAgeCount != 4 || diagnostics.EmbeddingQueue.OldestStatusChangedAt == nil || *diagnostics.EmbeddingQueue.OldestStatusChangedAt != knownAt {
		t.Fatalf("embedding queue=%+v; should count only matching recorded observation timestamps", diagnostics.EmbeddingQueue)
	}
	if diagnostics.EncodingCommit.CommittingCount != 1 || diagnostics.EncodingCommit.ReconcilingCount != 1 || diagnostics.EncodingCommit.UnknownStartCount != 0 || diagnostics.EncodingCommit.OldestKnownStartedAt == nil {
		t.Fatalf("encoding commit diagnostics=%+v", diagnostics.EncodingCommit)
	}
	if diagnostics.ExpectedVectorPoints != 0 {
		t.Fatalf("expected vector points=%d want=0", diagnostics.ExpectedVectorPoints)
	}

	empty, err := openAdminBrowseCatalog(t).AdminDiagnostics(ctx)
	if err != nil || empty.EncodingQueue.Count != 0 || empty.EncodingCommit.ReconcilingCount != 0 {
		t.Fatalf("empty diagnostics=%+v err=%v", empty, err)
	}
}

func TestAdminDiagnosticsEmbeddingQueuesMatchBrowseCategories(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	ctx := context.Background()
	type fixture struct {
		hash, encodingStatus string
		gate                 bool
		changedAt            *string
	}
	queuedAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	blockedAt := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	fixtures := []fixture{
		{hash: "queued-gate-zero", encodingStatus: "pending", gate: false, changedAt: &queuedAt},
		{hash: "queued-failed-gate", encodingStatus: "failed", gate: true},
		{hash: "blocked-gate-one-known", encodingStatus: "pending", gate: true, changedAt: &blockedAt},
		{hash: "blocked-gate-one-unknown", encodingStatus: "pending", gate: true},
	}
	for _, item := range fixtures {
		if err := store.UpsertBlob(ctx, Blob{Hash: item.hash, SizeBytes: 10, ContentType: "image/jpeg", EncodingGate: item.gate}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, embedding_status='pending' WHERE hash=?`, item.encodingStatus, item.hash); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET status='pending', status_changed_at=? WHERE blob_hash=? AND stage='embedding'`, item.changedAt, item.hash); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := store.AdminDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.AdminBlobs(ctx, AdminBlobFilters{Embedding: "queued"}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := store.AdminBlobs(ctx, AdminBlobFilters{Embedding: "blocked"}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	assertQueueMatchesBrowse := func(label string, rows []AdminBlob, diagnostic AdminQueueDiagnostics, wantOldest string) {
		t.Helper()
		if diagnostic.Count != int64(len(rows)) {
			t.Errorf("%s diagnostics count=%d but exact browse returned %d rows", label, diagnostic.Count, len(rows))
		}
		var unknown int64
		for _, row := range rows {
			observations, err := store.StageObservations(ctx, row.Hash)
			if err != nil {
				t.Fatal(err)
			}
			observation := observations["embedding"]
			if observation.Status != "pending" || observation.StatusChangedAt == nil {
				unknown++
			}
		}
		if diagnostic.UnknownAgeCount != unknown {
			t.Errorf("%s unknown ages=%d but matching browse rows have %d unknown timestamps", label, diagnostic.UnknownAgeCount, unknown)
		}
		if diagnostic.OldestStatusChangedAt == nil || *diagnostic.OldestStatusChangedAt != wantOldest {
			t.Errorf("%s oldest timestamp=%v want %q", label, diagnostic.OldestStatusChangedAt, wantOldest)
		}
	}
	assertQueueMatchesBrowse("queued", queued, snapshot.EmbeddingQueue, queuedAt)
	assertQueueMatchesBrowse("blocked", blocked, snapshot.EmbeddingBlockedQueue, blockedAt)
	if len(queued) != 2 || queued[0].Hash != "queued-failed-gate" || queued[1].Hash != "queued-gate-zero" {
		t.Errorf("queued browse=%+v; gate=0 and failed-gate rows should be eligible", queued)
	}
	if len(blocked) != 2 || blocked[0].Hash != "blocked-gate-one-known" || blocked[1].Hash != "blocked-gate-one-unknown" {
		t.Errorf("blocked browse=%+v; gate=1 pending rows should be separate", blocked)
	}
}

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
	if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='committing' WHERE hash='webp'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE blob_stage_observations SET phase='reconciling' WHERE blob_hash='webp' AND stage='encoding'`); err != nil {
		t.Fatal(err)
	}

	encodingCode := "invalid_source"
	photos, err := store.AdminPhotos(ctx, AdminPhotoFilters{Encoding: "failed", EncodingErrorCode: encodingCode}, "", 0, 20)
	if err != nil || len(photos) != 3 || photos[0].EncodingErrorCode != encodingCode {
		t.Fatalf("encoding code photo filter rows=%+v err=%v", photos, err)
	}
	blobs, err := store.AdminBlobs(ctx, AdminBlobFilters{Encoding: "failed", EncodingErrorCode: encodingCode}, "", 20)
	if err != nil || len(blobs) != 1 || blobs[0].Hash != "shared" || blobs[0].EncodingErrorCode != encodingCode {
		t.Fatalf("encoding code blob filter rows=%+v err=%v", blobs, err)
	}
	embeddingCode := "embedding_failed"
	blobs, err = store.AdminBlobs(ctx, AdminBlobFilters{EmbeddingErrorCode: embeddingCode}, "", 20)
	if err != nil || len(blobs) != 1 || blobs[0].EmbeddingErrorCode != embeddingCode {
		t.Fatalf("embedding code blob filter rows=%+v err=%v", blobs, err)
	}
	expired := true
	blobs, err = store.AdminBlobs(ctx, AdminBlobFilters{EncodingLeaseExpired: &expired}, "", 20)
	if err != nil || len(blobs) != 1 || blobs[0].Hash != "blocked" {
		t.Fatalf("expired lease filter rows=%+v err=%v", blobs, err)
	}
	phase := "reconciling"
	blobs, err = store.AdminBlobs(ctx, AdminBlobFilters{EncodingPhase: phase}, "", 20)
	if err != nil || len(blobs) != 1 || blobs[0].Hash != "webp" {
		t.Fatalf("encoding phase filter rows=%+v err=%v", blobs, err)
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
