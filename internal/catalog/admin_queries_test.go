package catalog

import (
	"context"
	"fmt"
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
		if err := store.UpsertBlob(ctx, Blob{Hash: item.hash, ContentType: item.contentType}); err != nil {
			t.Fatalf("upsert %s: %v", item.hash, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, embedding_status=?, encoding_gate=? WHERE hash=?`,
			item.encodingStatus, item.embeddingStatus, item.gate, item.hash); err != nil {
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
	if snapshot.Albums != 2 || snapshot.Photos != 13 || snapshot.Blobs != 11 {
		t.Fatalf("library counts=%+v want albums=2 photos=13 blobs=11", snapshot)
	}

	// Each blob is counted once per partition, regardless of repeated photo
	// references or albums. The shared blob is present in three photo rows.
	assertBlobTotals(t, "encoding", snapshot.EncodingCategories, 11)
	assertBlobTotals(t, "embedding", snapshot.EmbeddingCategories, 11)

	assertBlobCount(t, "encoding queued", snapshot.EncodingCategories["queued"], 2)
	assertBlobCount(t, "encoding processing", snapshot.EncodingCategories["processing"], 1)
	assertBlobCount(t, "encoding validating", snapshot.EncodingCategories["validating"], 1)
	assertBlobCount(t, "encoding committing", snapshot.EncodingCategories["committing"], 1)
	assertBlobCount(t, "encoding completed", snapshot.EncodingCategories["completed"], 1)
	assertBlobCount(t, "encoding skipped", snapshot.EncodingCategories["skipped"], 2)
	assertBlobCount(t, "encoding failed", snapshot.EncodingCategories["failed"], 2)
	assertBlobCount(t, "encoding unknown includes unreferenced", snapshot.EncodingCategories["unknown"], 1)

	assertBlobCount(t, "embedding queued", snapshot.EmbeddingCategories["queued"], 6)
	assertBlobCount(t, "embedding processing", snapshot.EmbeddingCategories["processing"], 1)
	assertBlobCount(t, "embedding completed", snapshot.EmbeddingCategories["completed"], 2)
	assertBlobCount(t, "embedding failed", snapshot.EmbeddingCategories["failed"], 1)
	assertBlobCount(t, "embedding unknown", snapshot.EmbeddingCategories["unknown"], 1)

	// The health numbers ride in the same snapshot: "future" is the only
	// unreferenced blob, the dangling photo has no blob, and the two
	// embedding-ready blobs (encoding-committing, encoding-failed) back one
	// photo row each.
	if snapshot.UnreferencedBlobs != 1 || snapshot.MissingBlobPhotos != 1 || snapshot.ExpectedVectorPoints != 2 {
		t.Fatalf("health counts unreferenced=%d missingBlobPhotos=%d expectedVectorPoints=%d want 1/1/2",
			snapshot.UnreferencedBlobs, snapshot.MissingBlobPhotos, snapshot.ExpectedVectorPoints)
	}
	// The raw UPDATEs above leave both held rows (encoding-leased,
	// encoding-received) on the epoch-zero lease default, which the expired
	// comparison reads as long past — the same verdict the browse filter
	// gives them.
	if snapshot.EncodingExpiredLeases != 1 || snapshot.EmbeddingExpiredLeases != 1 {
		t.Fatalf("expired leases=%d/%d want 1/1 (held rows keep the epoch-zero lease default)",
			snapshot.EncodingExpiredLeases, snapshot.EmbeddingExpiredLeases)
	}

	// Every pending embedding is queued regardless of legacy gates or encoding.
	// The processing row has an expired lease and is also reclaimable.
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
		"encoding-leased": true, "gate-pending": true, "encoding-received": true,
	}
	if len(claimedHashes) != len(wantClaimed) {
		t.Fatalf("claimable blobs=%v want=%v", claimedHashes, wantClaimed)
	}
	for hash := range wantClaimed {
		if !claimedHashes[hash] {
			t.Errorf("claim query omitted %s: %v", hash, claimedHashes)
		}
	}
	if int64(len(claimedHashes)) != snapshot.EmbeddingCategories["queued"].BlobCount+1 {
		t.Fatalf("overview queued=%d but claim returned %d (including one expired lease)", snapshot.EmbeddingCategories["queued"].BlobCount, len(claimedHashes))
	}
}

func TestAdminOverviewEmbeddingClaimsIgnoreEncodingAndLegacyGates(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	encodingStates := []string{"pending", "leased", "received", "committing", "done", "skipped", "failed", "future"}
	for _, encodingStatus := range encodingStates {
		for _, gate := range []bool{false, true} {
			hash := fmt.Sprintf("%s-gate-%t", encodingStatus, gate)
			if err := store.UpsertBlob(ctx, Blob{Hash: hash, ContentType: "image/jpeg"}); err != nil {
				t.Fatalf("upsert %s: %v", hash, err)
			}
			// Simulate persisted gates even after the neutralizing migration.
			if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, encoding_gate=?, embedding_status='pending' WHERE hash=?`, encodingStatus, gate, hash); err != nil {
				t.Fatalf("set state for %s: %v", hash, err)
			}
		}
	}

	snapshot, err := store.AdminOverview(ctx)
	if err != nil {
		t.Fatalf("admin overview: %v", err)
	}
	if got := snapshot.EmbeddingCategories["queued"].BlobCount; got != 16 {
		t.Fatalf("queued blobs=%d want all 16 regardless of encoding or legacy gate", got)
	}
	if _, ok := snapshot.EmbeddingCategories["blocked"]; ok {
		t.Fatal("obsolete blocked category is still present")
	}

	claimed, err := store.ClaimPendingEmbeddings(ctx, 32, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("claim pending embeddings: %v", err)
	}
	claimedHashes := make(map[string]bool, len(claimed))
	for _, item := range claimed {
		claimedHashes[item.Hash] = true
	}
	if int64(len(claimedHashes)) != snapshot.EmbeddingCategories["queued"].BlobCount {
		t.Fatalf("overview queued=%d but claim returned %d", snapshot.EmbeddingCategories["queued"].BlobCount, len(claimedHashes))
	}
	for _, status := range encodingStates {
		for _, gate := range []bool{false, true} {
			hash := fmt.Sprintf("%s-gate-%t", status, gate)
			if !claimedHashes[hash] {
				t.Errorf("claim omitted %s", hash)
			}
			var embeddingStatus, encodingStatus string
			var lease int64
			if err := store.db.QueryRowContext(ctx, `SELECT embedding_status, embedding_lease_until, encoding_status FROM blobs WHERE hash=?`, hash).
				Scan(&embeddingStatus, &lease, &encodingStatus); err != nil {
				t.Fatal(err)
			}
			if embeddingStatus != "processing" || lease <= time.Now().UnixMilli() || encodingStatus != status {
				t.Errorf("claim %s: embedding=%s lease=%d encoding=%s", hash, embeddingStatus, lease, encodingStatus)
			}
		}
	}
}

// TestAdminOverviewCountsExpiredLeasesPerStage pins the health section's
// expired-lease figures: each stage counts only rows it currently holds
// (leased/processing) whose recorded lease deadline has already passed. A
// never-set deadline is the epoch default 0 — long past, and expired by the
// same comparison the browse filter and reclaim sweep use — while rows not in
// a held state never count, whatever their lease column says.
func TestAdminOverviewCountsExpiredLeasesPerStage(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	for _, blob := range []Blob{
		{Hash: "expired-encoding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "epoch-encoding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "live-encoding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "expired-embedding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "live-embedding", SizeBytes: 10, ContentType: "image/jpeg"},
		{Hash: "pending-embedding", SizeBytes: 10, ContentType: "image/jpeg"},
	} {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatalf("upsert %s: %v", blob.Hash, err)
		}
	}
	expired := time.Now().Add(-time.Minute).UnixMilli()
	live := time.Now().Add(5 * time.Minute).UnixMilli()
	updates := []struct {
		query string
		args  []any
	}{
		{`UPDATE blobs SET encoding_status='leased', encoding_lease_until=? WHERE hash='expired-encoding'`, []any{expired}},
		// encoding_lease_until keeps its NOT NULL DEFAULT 0: a leased row whose
		// deadline was never set reads as epoch zero and counts as expired.
		{`UPDATE blobs SET encoding_status='leased' WHERE hash='epoch-encoding'`, nil},
		{`UPDATE blobs SET encoding_status='leased', encoding_lease_until=? WHERE hash='live-encoding'`, []any{live}},
		{`UPDATE blobs SET embedding_status='processing', embedding_lease_until=? WHERE hash='expired-embedding'`, []any{expired}},
		{`UPDATE blobs SET embedding_status='processing', embedding_lease_until=? WHERE hash='live-embedding'`, []any{live}},
		// Not held by the stage, so its epoch-zero lease column is irrelevant.
		{`UPDATE blobs SET embedding_status='pending' WHERE hash='pending-embedding'`, nil},
	}
	for _, update := range updates {
		if _, err := store.db.ExecContext(ctx, update.query, update.args...); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := store.AdminOverview(ctx)
	if err != nil {
		t.Fatalf("admin overview: %v", err)
	}
	if snapshot.EncodingExpiredLeases != 2 || snapshot.EmbeddingExpiredLeases != 1 {
		t.Fatalf("expired leases=%d/%d want 2/1: only held rows with a passed deadline may count",
			snapshot.EncodingExpiredLeases, snapshot.EmbeddingExpiredLeases)
	}
	if snapshot.Blobs != 6 {
		t.Fatalf("blob total=%d want 6", snapshot.Blobs)
	}
}

func assertBlobTotals(t *testing.T, name string, groups map[string]BlobCounts, blobs int64) {
	t.Helper()
	var gotBlobs int64
	for _, counts := range groups {
		gotBlobs += counts.BlobCount
	}
	if gotBlobs != blobs {
		t.Fatalf("%s totals blobs=%d want blobs=%d (%v)", name, gotBlobs, blobs, groups)
	}
}

func assertBlobCount(t *testing.T, name string, got BlobCounts, blobs int64) {
	t.Helper()
	want := BlobCounts{BlobCount: blobs}
	if got != want {
		t.Errorf("%s=%+v want=%+v", name, got, want)
	}
}
