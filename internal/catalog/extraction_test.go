package catalog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// extractionEntryRows reads one run's recorded outcomes straight from the
// table. The paginated admin entry browser is gone, so the catalog tests pin
// the recording contract at the storage level instead of through a browse API.
func extractionEntryRows(t *testing.T, store *Store, runID int64) []ExtractionEntryOutcome {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), `SELECT ordinal, name, candidate, outcome, reason_code, error_detail, photo_index, hash
		FROM extraction_entry_outcomes WHERE run_id=? ORDER BY ordinal ASC`, runID)
	if err != nil {
		t.Fatalf("read entry outcomes for run %d: %v", runID, err)
	}
	defer rows.Close()
	var items []ExtractionEntryOutcome
	for rows.Next() {
		var item ExtractionEntryOutcome
		var photoIndex any
		if err := rows.Scan(&item.Ordinal, &item.Name, &item.Candidate, &item.Outcome, &item.ReasonCode, &item.ErrorDetail, &photoIndex, &item.Hash); err != nil {
			t.Fatalf("scan entry outcome: %v", err)
		}
		if photoIndex != nil {
			value := int(photoIndex.(int64))
			item.PhotoIndex = &value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate entry outcomes: %v", err)
	}
	return items
}

func extractionRunCount(t *testing.T, store *Store, albumID string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM extraction_runs WHERE album_id=?`, albumID).Scan(&count); err != nil {
		t.Fatalf("count extraction runs for %s: %v", albumID, err)
	}
	return count
}

func openExtractionTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestExtractionRunBatchesAppendCountsAndRedactsDetails(t *testing.T) {
	store := openExtractionTestStore(t)
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: "archive", Status: AlbumStatusQueued}); err != nil {
		t.Fatal(err)
	}
	runID, err := store.BeginExtractionRun(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	photoIndex := 0
	batch := []ExtractionEntryOutcome{
		{Ordinal: 0, Name: "same.jpg", Candidate: true, Outcome: "registered", PhotoIndex: &photoIndex, Hash: "sha256-a"},
		{Ordinal: 1, Name: "notes.txt", Outcome: "skipped", ReasonCode: "unsupported_extension"},
		{Ordinal: 2, Name: "same.jpg", Candidate: true, Outcome: "failed", ReasonCode: "photo_catalog_failed", ErrorDetail: `request https://store.example/path?X-Amz-Signature=secret token=private`},
	}
	if err := store.RecordExtractionEntries(ctx, runID, batch); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, runID, "failed", "entry_failed", "failed https://store.example/?token=private", true); err != nil {
		t.Fatal(err)
	}

	detail, err := store.AdminAlbumByID(ctx, "archive")
	if err != nil {
		t.Fatalf("album detail: %v", err)
	}
	run := detail.LatestExtraction
	if run == nil || run.Status != "failed" || run.EntriesRecorded != 3 || run.CandidateEntriesRecorded != 2 || run.TotalEntries == nil || *run.TotalEntries != 3 || run.CandidateEntries == nil || *run.CandidateEntries != 2 || run.RegisteredCount != 1 || run.SkippedCount != 1 || run.FailedCount != 1 {
		t.Fatalf("run summary=%+v", run)
	}
	entries := extractionEntryRows(t, store, runID)
	if len(entries) != 3 || entries[2].Name != "same.jpg" || entries[2].Ordinal != 2 || entries[2].PhotoIndex != nil {
		t.Fatalf("recorded entries=%+v want the failed entry stored without a photo link", entries)
	}
	if entries[0].PhotoIndex == nil || *entries[0].PhotoIndex != 0 || entries[0].Hash != "sha256-a" {
		t.Fatalf("registered entry lost its photo link: %+v", entries[0])
	}
	for _, secret := range []string{"https://", "secret", "private"} {
		if strings.Contains(entries[2].ErrorDetail, secret) || strings.Contains(run.ErrorDetail, secret) {
			t.Fatalf("sensitive detail persisted: entry=%q run=%q", entries[2].ErrorDetail, run.ErrorDetail)
		}
	}
	if entries[2].ErrorDetail == "" || !strings.Contains(entries[2].ErrorDetail, "redacted") {
		t.Fatalf("expected useful bounded redaction, got %q", entries[2].ErrorDetail)
	}
}

func TestLegacyAlbumHasNoSyntheticExtractionRun(t *testing.T) {
	store := openExtractionTestStore(t)
	if err := store.CreateAlbum(context.Background(), Album{ID: "legacy", Status: AlbumStatusReady}); err != nil {
		t.Fatal(err)
	}
	detail, err := store.AdminAlbumByID(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if detail.ExtractionRecorded || detail.LatestExtraction != nil {
		t.Fatalf("legacy album has invented extraction observation: %+v", detail)
	}
	if count := extractionRunCount(t, store, "legacy"); count != 0 {
		t.Fatalf("legacy album has %d synthetic run rows, want none", count)
	}
}

// TestExtractionRunHistoryIsAppendOnlyAcrossRuns pins the durability property
// the issues page depends on: reprocessing appends a new run and never
// rewrites or drops the previous run's entry outcomes.
func TestExtractionRunHistoryIsAppendOnlyAcrossRuns(t *testing.T) {
	store := openExtractionTestStore(t)
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: "archive", Status: AlbumStatusQueued}); err != nil {
		t.Fatal(err)
	}
	firstRun, err := store.BeginExtractionRun(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, firstRun, []ExtractionEntryOutcome{
		{Ordinal: 0, Name: "first.jpg", Candidate: true, Outcome: "registered", PhotoIndex: intPtr(0), Hash: "hash-one"},
		{Ordinal: 1, Name: "notes.txt", Outcome: "skipped", ReasonCode: "unsupported_extension"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, firstRun, "succeeded", "", "", true); err != nil {
		t.Fatal(err)
	}
	secondRun, err := store.BeginExtractionRun(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	// Same ordinals as the first run: a new run reuses the ordinal space
	// without touching the old rows.
	if err := store.RecordExtractionEntries(ctx, secondRun, []ExtractionEntryOutcome{
		{Ordinal: 0, Name: "second.jpg", Candidate: true, Outcome: "registered", PhotoIndex: intPtr(0), Hash: "hash-two"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, secondRun, "succeeded", "", "", true); err != nil {
		t.Fatal(err)
	}

	detail, err := store.AdminAlbumByID(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if detail.LatestExtraction == nil || detail.LatestExtraction.RunID != secondRun || detail.LatestExtraction.RegisteredCount != 1 {
		t.Fatalf("latest run=%+v want the second run as the album's latest", detail.LatestExtraction)
	}
	oldEntries := extractionEntryRows(t, store, firstRun)
	if len(oldEntries) != 2 || oldEntries[0].Name != "first.jpg" || oldEntries[1].ReasonCode != "unsupported_extension" {
		t.Fatalf("old run entry history was overwritten: %+v", oldEntries)
	}
}

// TestLatestExtractionFailuresSkipRunningRuns pins the surviving read
// contract for entry outcomes: only each album's latest run contributes, and
// only once that run is terminal. A running latest run exposes nothing rather
// than falling back to an older run's failures.
func TestLatestExtractionFailuresSkipRunningRuns(t *testing.T) {
	store := openExtractionTestStore(t)
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: "archive", Status: AlbumStatusQueued}); err != nil {
		t.Fatal(err)
	}
	oldRun, err := store.BeginExtractionRun(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, oldRun, []ExtractionEntryOutcome{
		{Ordinal: 0, Name: "old-failure.jpg", Candidate: true, Outcome: "failed", ReasonCode: "invalid_image"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, oldRun, "failed", "entry_failed", "old failure", true); err != nil {
		t.Fatal(err)
	}
	failures, err := store.AdminLatestExtractionFailures(ctx, "invalid_image", "", -1, 10)
	if err != nil || len(failures) != 1 || failures[0].Name != "old-failure.jpg" || failures[0].RunID != oldRun {
		t.Fatalf("terminal latest run failures=%+v err=%v", failures, err)
	}

	runningRun, err := store.BeginExtractionRun(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, runningRun, []ExtractionEntryOutcome{
		{Ordinal: 0, Name: "new-failure.jpg", Candidate: true, Outcome: "failed", ReasonCode: "invalid_image"},
	}); err != nil {
		t.Fatal(err)
	}
	failures, err = store.AdminLatestExtractionFailures(ctx, "invalid_image", "", -1, 10)
	if err != nil || len(failures) != 0 {
		t.Fatalf("running latest run failures=%+v err=%v; neither the new nor the old run may leak", failures, err)
	}
}

func intPtr(value int) *int { return &value }

func TestSafeExtractionErrorDetailRedactsEnvironmentAndJSONCredentials(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		marker string
	}{
		{"aws secret environment variable", "AWS_SECRET_ACCESS_KEY=synthetic-aws-secret", "synthetic-aws-secret"},
		{"client secret", `client_secret: "synthetic-client-secret"`, "synthetic-client-secret"},
		{"refresh token", "refresh_token=synthetic-refresh-token", "synthetic-refresh-token"},
		{"api key", "api_key=synthetic-api-key", "synthetic-api-key"},
		{"aws access key id", "AWS_ACCESS_KEY_ID: synthetic-access-key", "synthetic-access-key"},
		{"json credential", `{"credentials":{"client_secret":"synthetic-json-secret"}}`, "synthetic-json-secret"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := SafeExtractionErrorDetail(test.input)
			if strings.Contains(got, test.marker) {
				t.Fatalf("credential value was not redacted (detail length %d)", len(got))
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("redaction marker missing (detail length %d)", len(got))
			}
		})
	}
}
