package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractionRunBatchesAppendCountsAndRedactsDetails(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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

	run, found, err := store.LatestExtractionRun(ctx, "archive")
	if err != nil || !found {
		t.Fatalf("latest run=%+v found=%v err=%v", run, found, err)
	}
	if run.Status != "failed" || run.EntriesRecorded != 3 || run.CandidateEntriesRecorded != 2 || run.TotalEntries == nil || *run.TotalEntries != 3 || run.CandidateEntries == nil || *run.CandidateEntries != 2 || run.RegisteredCount != 1 || run.SkippedCount != 1 || run.FailedCount != 1 {
		t.Fatalf("run summary=%+v", run)
	}
	entries, err := store.ListExtractionEntries(ctx, "archive", runID, "failed", -1, 10)
	if err != nil || len(entries) != 1 || entries[0].Name != "same.jpg" || entries[0].Ordinal != 2 || entries[0].PhotoIndex != nil {
		t.Fatalf("failed entry=%+v err=%v", entries, err)
	}
	for _, secret := range []string{"https://", "secret", "private"} {
		if strings.Contains(entries[0].ErrorDetail, secret) || strings.Contains(run.ErrorDetail, secret) {
			t.Fatalf("sensitive detail persisted: entry=%q run=%q", entries[0].ErrorDetail, run.ErrorDetail)
		}
	}
	if entries[0].ErrorDetail == "" || !strings.Contains(entries[0].ErrorDetail, "redacted") {
		t.Fatalf("expected useful bounded redaction, got %q", entries[0].ErrorDetail)
	}
}

func TestLegacyAlbumHasNoSyntheticExtractionRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
	runs, err := store.ListExtractionRuns(context.Background(), "legacy", 0, 10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("legacy runs=%+v err=%v", runs, err)
	}
}

func TestExtractionEntryPagingRequiresTerminalRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, Album{ID: "archive", Status: AlbumStatusQueued}); err != nil {
		t.Fatal(err)
	}
	runID, err := store.BeginExtractionRun(ctx, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, runID, []ExtractionEntryOutcome{{Ordinal: 4, Name: "later.jpg", Candidate: true, Outcome: "failed", ReasonCode: "invalid_image"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListExtractionEntries(ctx, "archive", runID, "", -1, 10); !errors.Is(err, ErrExtractionRunInProgress) {
		t.Fatalf("running run entry page err=%v, want ErrExtractionRunInProgress", err)
	}
	if err := store.FinishExtractionRun(ctx, runID, "incomplete", "outcome_recording_failed", "", false); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListExtractionEntries(ctx, "archive", runID, "", -1, 10)
	if err != nil || len(entries) != 1 || entries[0].Ordinal != 4 {
		t.Fatalf("incomplete terminal run entries=%+v err=%v", entries, err)
	}
}

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
