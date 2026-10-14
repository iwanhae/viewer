package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAdminOverviewPipelineBreakdownsAndClaimEligibility(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	for _, albumID := range []string{"album-a", "album-b"} {
		if err := store.CreateAlbum(ctx, Album{ID: albumID, Status: AlbumStatusReady}); err != nil {
			t.Fatalf("create %s: %v", albumID, err)
		}
	}

	items := []struct {
		hash, contentType, encodingStatus, embeddingStatus string
		gate                                               bool
	}{
		{"shared", "image/jpeg", "pending", "pending", false},
		{"gate-failed", "image/jpeg", "failed", "pending", true},
		{"encoding-leased", "image/jpeg", "leased", "pending", true},
		{"encoding-received", "image/jpeg", "received", "processing", true},
		{"encoding-committing", "image/jpeg", "committing", "ready", true},
		{"encoding-done", "image/jpeg", "done", "failed", true},
		{"skipped-webp", "image/webp", "skipped", "pending", true},
		{"skipped-nonwebp", "image/png", "skipped", "pending", true},
		{"encoding-failed", "image/jpeg", "failed", "ready", false},
		{"gate-pending", "image/jpeg", "pending", "pending", true},
		{"future", "image/jpeg", "future-encoding", "future-embedding", true},
	}
	for _, item := range items {
		if err := store.UpsertBlob(ctx, Blob{Hash: item.hash, ContentType: item.contentType, EncodingGate: item.gate}); err != nil {
			t.Fatalf("upsert %s: %v", item.hash, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, embedding_status=? WHERE hash=?`,
			item.encodingStatus, item.embeddingStatus, item.hash); err != nil {
			t.Fatalf("set state for %s: %v", item.hash, err)
		}
	}

	photos := []Photo{
		{AlbumID: "album-a", Index: 0, Hash: "shared"},
		{AlbumID: "album-a", Index: 1, Hash: "shared"},
		{AlbumID: "album-a", Index: 2, Hash: "gate-failed"},
		{AlbumID: "album-a", Index: 3, Hash: "encoding-leased"},
		{AlbumID: "album-a", Index: 4, Hash: "encoding-received"},
		{AlbumID: "album-a", Index: 5, Hash: "encoding-committing"},
		{AlbumID: "album-a", Index: 6, Hash: "encoding-done"},
		{AlbumID: "album-b", Index: 0, Hash: "shared"},
		{AlbumID: "album-b", Index: 1, Hash: "skipped-webp"},
		{AlbumID: "album-b", Index: 2, Hash: "skipped-nonwebp"},
		{AlbumID: "album-b", Index: 3, Hash: "encoding-failed"},
		{AlbumID: "album-b", Index: 4, Hash: "gate-pending"},
		{AlbumID: "album-a", Index: 7, Name: "dangling.jpg", Hash: "missing-blob"},
	}
	for _, photo := range photos {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}

	snapshot, err := store.AdminOverview(ctx)
	if err != nil {
		t.Fatalf("admin overview: %v", err)
	}
	if snapshot.Albums != 2 || snapshot.Photos != 13 || snapshot.Blobs != 11 || snapshot.UnreferencedBlobs != 1 || snapshot.MissingBlobPhotos != 1 {
		t.Fatalf("library counts=%+v want albums=2 photos=13 blobs=11 unreferenced=1 missingBlobPhotos=1", snapshot)
	}

	// Category blob/photo totals partition the catalog. AlbumCount is distinct
	// per category, so the shared blob can make those category counts overlap.
	assertPipelineTotals(t, "encoding", snapshot.EncodingCategories, 11, 12)
	assertPipelineTotals(t, "embedding", snapshot.EmbeddingCategories, 11, 12)
	assertPipelineTotals(t, "encoding raw states", snapshot.EncodingRawStates, 11, 12)
	assertPipelineTotals(t, "embedding raw states", snapshot.EmbeddingRawStates, 11, 12)
	var pipelinePhotoRows int64
	for _, counts := range snapshot.EncodingCategories {
		pipelinePhotoRows += counts.PhotoCount
	}
	if pipelinePhotoRows+snapshot.MissingBlobPhotos != snapshot.Photos {
		t.Fatalf("photo accounting pipeline=%d missingBlobPhotos=%d total=%d", pipelinePhotoRows, snapshot.MissingBlobPhotos, snapshot.Photos)
	}

	assertCounts(t, "encoding queued", snapshot.EncodingCategories["queued"], 2, 4, 2)
	assertCounts(t, "encoding processing", snapshot.EncodingCategories["processing"], 1, 1, 1)
	assertCounts(t, "encoding validating", snapshot.EncodingCategories["validating"], 1, 1, 1)
	assertCounts(t, "encoding committing", snapshot.EncodingCategories["committing"], 1, 1, 1)
	assertCounts(t, "encoding completed", snapshot.EncodingCategories["completed"], 1, 1, 1)
	assertCounts(t, "encoding skipped", snapshot.EncodingCategories["skipped"], 2, 2, 1)
	assertCounts(t, "encoding failed", snapshot.EncodingCategories["failed"], 2, 2, 2)
	assertCounts(t, "encoding unknown includes unreferenced", snapshot.EncodingCategories["unknown"], 1, 0, 0)
	assertCounts(t, "skipped WebP", snapshot.EncodingSkippedReasons["webp"], 1, 1, 1)
	assertCounts(t, "skipped non-WebP", snapshot.EncodingSkippedReasons["nonWebp"], 1, 1, 1)
	if _, ok := snapshot.EncodingRawStates["future-encoding"]; !ok {
		t.Fatalf("unknown encoding raw state was lost: %v", snapshot.EncodingRawStates)
	}

	assertCounts(t, "embedding queued", snapshot.EmbeddingCategories["queued"], 4, 6, 2)
	assertCounts(t, "embedding blocked", snapshot.EmbeddingCategories["blocked"], 2, 2, 2)
	assertCounts(t, "embedding processing", snapshot.EmbeddingCategories["processing"], 1, 1, 1)
	assertCounts(t, "embedding completed", snapshot.EmbeddingCategories["completed"], 2, 2, 2)
	assertCounts(t, "embedding failed", snapshot.EmbeddingCategories["failed"], 1, 1, 1)
	assertCounts(t, "embedding unknown", snapshot.EmbeddingCategories["unknown"], 1, 0, 0)
	if _, ok := snapshot.EmbeddingRawStates["future-embedding"]; !ok {
		t.Fatalf("unknown embedding raw state was lost: %v", snapshot.EmbeddingRawStates)
	}

	// Prove queued is exactly the claim query's gate predicate, including the
	// gate=0/pending-encoding and gate=1/failed-encoding cases.
	claimed, err := store.ClaimPendingEmbeddings(ctx, 100, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("claim eligible embeddings: %v", err)
	}
	claimedHashes := make(map[string]bool, len(claimed))
	for _, blob := range claimed {
		claimedHashes[blob.Hash] = true
	}
	wantClaimed := map[string]bool{
		"shared": true, "gate-failed": true, "skipped-webp": true, "skipped-nonwebp": true,
	}
	if len(claimedHashes) != len(wantClaimed) {
		t.Fatalf("claimable blobs=%v want=%v", claimedHashes, wantClaimed)
	}
	for hash := range wantClaimed {
		if !claimedHashes[hash] {
			t.Errorf("claim query omitted %s: %v", hash, claimedHashes)
		}
	}
	if int64(len(claimedHashes)) != snapshot.EmbeddingCategories["queued"].BlobCount {
		t.Fatalf("overview queued=%d but claim returned %d", snapshot.EmbeddingCategories["queued"].BlobCount, len(claimedHashes))
	}
}

func assertPipelineTotals(t *testing.T, name string, groups map[string]PipelineCounts, blobs, photos int64) {
	t.Helper()
	var gotBlobs, gotPhotos int64
	for _, counts := range groups {
		gotBlobs += counts.BlobCount
		gotPhotos += counts.PhotoCount
	}
	if gotBlobs != blobs || gotPhotos != photos {
		t.Fatalf("%s totals blobs=%d photos=%d want blobs=%d photos=%d (%v)", name, gotBlobs, gotPhotos, blobs, photos, groups)
	}
}

func assertCounts(t *testing.T, name string, got PipelineCounts, blobs, photos, albums int64) {
	t.Helper()
	want := PipelineCounts{BlobCount: blobs, PhotoCount: photos, AlbumCount: albums}
	if got != want {
		t.Errorf("%s=%+v want=%+v", name, got, want)
	}
}
