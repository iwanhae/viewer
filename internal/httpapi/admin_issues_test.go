package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"viewer/internal/admin"
	"viewer/internal/catalog"
)

func seedIssueCatalog(t *testing.T, store *catalog.Store) {
	t.Helper()
	ctx := context.Background()
	for _, album := range []catalog.Album{
		{ID: "album-a", OriginalFilename: "alpha.zip", Status: catalog.AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "beta.zip", Status: catalog.AlbumStatusFailed, Error: "extract password=must-not-appear"},
		{ID: "album-c", OriginalFilename: "gamma.zip", Status: catalog.AlbumStatusReady},
		{ID: "album-empty-failed", OriginalFilename: "empty.zip", Status: catalog.AlbumStatusFailed},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatal(err)
		}
	}
	for _, blob := range []catalog.Blob{
		{Hash: "shared-failure", SizeBytes: 30, ContentType: "image/jpeg"},
		{Hash: "legacy-failure", SizeBytes: 20, ContentType: "image/jpeg"},
		{Hash: "resolved-history", SizeBytes: 10, ContentType: "image/jpeg"},
	} {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatal(err)
		}
	}
	for _, photo := range []catalog.Photo{
		{AlbumID: "album-a", Index: 0, Name: "one.jpg", Hash: "shared-failure"},
		{AlbumID: "album-a", Index: 1, Name: "duplicate.jpg", Hash: "shared-failure"},
		{AlbumID: "album-b", Index: 0, Name: "shared.jpg", Hash: "shared-failure"},
		{AlbumID: "album-b", Index: 1, Name: "legacy.jpg", Hash: "legacy-failure"},
		{AlbumID: "album-c", Index: 0, Name: "resolved.jpg", Hash: "resolved-history"},
	} {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatal(err)
		}
	}

	encodingJobs, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(encodingJobs) != 1 || encodingJobs[0].Hash != "shared-failure" {
		t.Fatalf("claim encoding failure: jobs=%+v err=%v", encodingJobs, err)
	}
	if ok, err := store.SkipEncoding(ctx, "shared-failure", encodingJobs[0].Token, "failed", "image/jpeg", "invalid source blob for encoding", 10); err != nil || !ok {
		t.Fatalf("record encoding failure: ok=%v err=%v", ok, err)
	}
	legacyEncoding, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(legacyEncoding) != 1 || legacyEncoding[0].Hash != "legacy-failure" {
		t.Fatalf("claim legacy encoding failure: jobs=%+v err=%v", legacyEncoding, err)
	}
	if ok, err := store.SkipEncoding(ctx, "legacy-failure", legacyEncoding[0].Token, "failed", "image/jpeg", "secret legacy raw message", 10); err != nil || !ok {
		t.Fatalf("record legacy encoding failure: ok=%v err=%v", ok, err)
	}
	embeddingJobs, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(time.Minute))
	if err != nil || len(embeddingJobs) != 3 {
		t.Fatalf("claim embedding failures and history: jobs=%+v err=%v", embeddingJobs, err)
	}
	if _, _, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{{
		Hash: "shared-failure", Status: catalog.EmbeddingStatusFailed, Error: "embed image: password=private-worker-detail",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{{
		Hash: "resolved-history", Status: catalog.EmbeddingStatusFailed, Error: "embed image: old failure",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetFailedEmbeddings(ctx, nil); err != nil {
		t.Fatal(err)
	}
	currentRetry, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(time.Minute))
	if err != nil || len(currentRetry) != 2 {
		t.Fatalf("claim post-retry embeddings: jobs=%+v err=%v", currentRetry, err)
	}
	if _, _, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{{
		Hash: "shared-failure", Status: catalog.EmbeddingStatusFailed, Error: "embed image: current failure",
	}}); err != nil {
		t.Fatal(err)
	}

	// The alpha failure belongs to a prior run and is resolved by the latest
	// successful run; beta and gamma each have one current failed entry.
	oldRun, err := store.BeginExtractionRun(ctx, "album-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, oldRun, []catalog.ExtractionEntryOutcome{{
		Ordinal: 0, Name: "old-secret.jpg", Candidate: true, Outcome: "failed", ReasonCode: "invalid_image", ErrorDetail: "should not appear",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, oldRun, "failed", "entry_failed", "old run detail", true); err != nil {
		t.Fatal(err)
	}
	newRun, err := store.BeginExtractionRun(ctx, "album-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, newRun, []catalog.ExtractionEntryOutcome{{
		Ordinal: 0, Name: "now-good.jpg", Candidate: true, Outcome: "registered", PhotoIndex: intPointer(5), Hash: "ok-hash",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, newRun, "succeeded", "", "", true); err != nil {
		t.Fatal(err)
	}
	for _, albumID := range []string{"album-b", "album-c"} {
		runID, err := store.BeginExtractionRun(ctx, albumID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordExtractionEntries(ctx, runID, []catalog.ExtractionEntryOutcome{{
			Ordinal: 1, Name: albumID + "-bad.jpg", Candidate: true, Outcome: "failed", ReasonCode: "invalid_image", ErrorDetail: "secret detail",
		}}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishExtractionRun(ctx, runID, "failed", "entry_failed", "private run detail", true); err != nil {
			t.Fatal(err)
		}
	}
}

func intPointer(value int) *int { return &value }

func TestAdminIssuesGroupsOnlyCurrentFailuresAndPagesLatestExtractionEntries(t *testing.T) {
	store := openAdminTestCatalog(t)
	seedIssueCatalog(t, store)
	router := New(nil, nil, nil, nil, "", admin.NewService(store, nil, nil), "sekrit").Router()
	auth := basicAuthHeader("operator", "sekrit")

	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/issues", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/extraction-failures?reasonCode=invalid_image", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated extraction failures status=%d body=%s", rec.Code, rec.Body.String())
	}
	issuesResponse := doAdminRequest(t, router, http.MethodGet, "/admin/api/issues", auth)
	if issuesResponse.Code != http.StatusOK {
		t.Fatalf("issues status=%d body=%s", issuesResponse.Code, issuesResponse.Body.String())
	}
	if strings.Contains(issuesResponse.Body.String(), "must-not-appear") || strings.Contains(issuesResponse.Body.String(), "private-worker-detail") || strings.Contains(issuesResponse.Body.String(), "secret legacy raw") {
		t.Fatalf("issue response leaked raw error text: %s", issuesResponse.Body.String())
	}
	var issues admin.Issues
	if err := json.Unmarshal(issuesResponse.Body.Bytes(), &issues); err != nil {
		t.Fatal(err)
	}
	if issues.Truncated || issues.RemainingGroups != 0 || issues.TotalGroups != 5 {
		t.Fatalf("issue cap metadata=%+v", issues)
	}
	groups := map[string]admin.IssueGroup{}
	for _, group := range issues.Groups {
		groups[group.Stage+":"+group.ErrorCode] = group
	}
	encoding := groups["encoding:invalid_source"]
	if encoding.FailureCount != 1 || encoding.CountUnit != "blobs" {
		t.Fatalf("shared encoding issue counts=%+v; want one blob", encoding)
	}
	embedding := groups["embedding:embedding_failed"]
	if embedding.FailureCount != 1 || embedding.CountUnit != "blobs" {
		t.Fatalf("shared embedding issue counts=%+v; same blob should be independently counted", embedding)
	}
	legacy := groups["encoding:legacy_unknown"]
	if legacy.FailureCount != 1 || legacy.CountUnit != "blobs" {
		t.Fatalf("legacy issue=%+v", legacy)
	}
	archive := groups["album_extraction:legacy_unknown"]
	if archive.FailureCount != 2 || archive.CountUnit != "albums" {
		t.Fatalf("failed album issue=%+v; raw album errors must not split groups", archive)
	}
	entries := groups["extraction_entry:invalid_image"]
	if entries.FailureCount != 2 || entries.CountUnit != "entries" {
		t.Fatalf("latest extraction entry issue=%+v; resolved old run must not count", entries)
	}
	for _, removedKey := range []string{`"blobCount"`, `"photoCount"`, `"albumCount"`, `"entryCount"`} {
		if strings.Contains(issuesResponse.Body.String(), removedKey) {
			t.Fatalf("issues contract contains obsolete relation count %s: %s", removedKey, issuesResponse.Body.String())
		}
	}
	if _, exists := groups["embedding:legacy_unknown"]; exists {
		t.Fatalf("resolved failed embedding was reported current: %+v", groups)
	}

	// Error-code filters use the same legacy fallback as issue grouping and are
	// usable on both paged photo and blob resources.
	for _, target := range []string{
		"/admin/api/photos?encoding=failed&encodingErrorCode=invalid_source",
		"/admin/api/blobs?encoding=failed&encodingErrorCode=invalid_source",
		"/admin/api/photos?encoding=failed&encodingErrorCode=legacy_unknown",
		"/admin/api/blobs?encoding=failed&encodingErrorCode=legacy_unknown",
	} {
		rec := doAdminRequest(t, router, http.MethodGet, target, auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("error-code filter %s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
	}

	first := doAdminRequest(t, router, http.MethodGet, "/admin/api/extraction-failures?reasonCode=invalid_image&limit=1", auth)
	if first.Code != http.StatusOK {
		t.Fatalf("latest extraction failure page status=%d body=%s", first.Code, first.Body.String())
	}
	var firstPage admin.ExtractionFailurePage
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil || len(firstPage.Failures) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("first latest failures page=%+v err=%v", firstPage, err)
	}
	if strings.Contains(first.Body.String(), "old-secret") || strings.Contains(first.Body.String(), "private run detail") {
		t.Fatalf("paged failure response included historical or raw data: %s", first.Body.String())
	}
	secondTarget := "/admin/api/extraction-failures?reasonCode=invalid_image&limit=1&cursor=" + url.QueryEscape(*firstPage.NextCursor)
	second := doAdminRequest(t, router, http.MethodGet, secondTarget, auth)
	var secondPage admin.ExtractionFailurePage
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &secondPage) != nil || len(secondPage.Failures) != 1 || secondPage.NextCursor != nil {
		t.Fatalf("second latest failures page status=%d body=%s", second.Code, second.Body.String())
	}
	if firstPage.Failures[0].AlbumID == secondPage.Failures[0].AlbumID {
		t.Fatalf("keyset pages repeated album entry: first=%+v second=%+v", firstPage, secondPage)
	}

	for _, target := range []string{
		"/admin/api/issues?stage=embedding",
		"/admin/api/diagnostics?extra=1",
		"/admin/api/photos?encodingErrorCode=invalid%21code",
		"/admin/api/blobs?embeddingLeaseExpired=yes",
		"/admin/api/blobs?encodingPhase=committing",
		"/admin/api/extraction-failures?reasonCode=INVALID_IMAGE",
	} {
		if rec := doAdminRequest(t, router, http.MethodGet, target, auth); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid request %s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
	}
	for _, target := range []string{fmt.Sprintf("/admin/api/issues"), "/admin/api/diagnostics"} {
		if rec := doAdminRequest(t, router, http.MethodGet, target, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthorized %s status=%d", target, rec.Code)
		}
	}
}

func TestAdminIssuesEmptyCatalogAndDiagnosticsVectorAvailability(t *testing.T) {
	store := openAdminTestCatalog(t)
	service := admin.NewService(store, stubAdminVectorCounter{err: fmt.Errorf("qdrant secret endpoint")}, nil).WithEncodingEnabled(false)
	router := New(nil, nil, nil, nil, "", service, "sekrit").Router()
	auth := basicAuthHeader("operator", "sekrit")

	empty := doAdminRequest(t, router, http.MethodGet, "/admin/api/issues", auth)
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"groups":[]`) || !strings.Contains(empty.Body.String(), `"truncated":false`) {
		t.Fatalf("empty issues status=%d body=%s", empty.Code, empty.Body.String())
	}
	diagnostics := doAdminRequest(t, router, http.MethodGet, "/admin/api/diagnostics", auth)
	if diagnostics.Code != http.StatusOK {
		t.Fatalf("diagnostics status=%d body=%s", diagnostics.Code, diagnostics.Body.String())
	}
	if strings.Contains(diagnostics.Body.String(), "qdrant secret") || !strings.Contains(diagnostics.Body.String(), `"status":"unavailable"`) || !strings.Contains(diagnostics.Body.String(), `"countAgreement":null`) {
		t.Fatalf("unavailable vector count should not fail or leak details: %s", diagnostics.Body.String())
	}
	if !strings.Contains(diagnostics.Body.String(), `"encodingEnabled":false`) || !strings.Contains(diagnostics.Body.String(), `"modelEnabled":false`) || !strings.Contains(diagnostics.Body.String(), "not heartbeats") {
		t.Fatalf("diagnostics feature/activity contract missing: %s", diagnostics.Body.String())
	}

	disabled := admin.NewService(store, nil, nil)
	result, err := disabled.Diagnostics(context.Background())
	if err != nil || result.VectorCount.Status != "disabled" || result.VectorCount.ActualPoints != nil || result.VectorCount.CountAgreement != nil {
		t.Fatalf("disabled vector diagnostics=%+v err=%v; zeros must not look measured", result.VectorCount, err)
	}
}
