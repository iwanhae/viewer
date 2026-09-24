package catalog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// vec768 builds an EmbeddingDim-length vector whose leading values are vals and
// whose tail is zeros. The catalog no longer stores vectors, but it still
// validates that ready results carry a full-width one, so write-back tests need
// shapes that pass that check.
func vec768(vals ...float32) []float32 {
	vec := make([]float32, EmbeddingDim)
	copy(vec, vals)
	return vec
}

// seedBlobStatus flips a blob's embedding status straight in the database: the
// production path (claim then ApplyEmbeddingResults) is exercised by its own
// tests, and these only need rows in a given state to read back.
func seedBlobStatus(t *testing.T, store *Store, hash string, status EmbeddingStatus) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE blobs SET embedding_status = ? WHERE hash = ?`, string(status), hash); err != nil {
		t.Fatalf("seed blob %s status %s: %v", hash, status, err)
	}
}

func TestEncodingCountsAggregateProgressAndSavings(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	items := []struct {
		hash        string
		size        int64
		contentType string
		status      string
		sourceSize  int64
	}{
		{hash: "pending", size: 600, contentType: "image/jpeg", status: "pending"},
		{hash: "leased", size: 500, contentType: "image/png", status: "leased"},
		{hash: "committing", size: 400, contentType: "image/png", status: "committing"},
		{hash: "converted", size: 100, contentType: "image/webp", status: "done", sourceSize: 1000},
		{hash: "not-smaller", size: 300, contentType: "image/jpeg", status: "skipped"},
		{hash: "failed", size: 200, contentType: "image/png", status: "failed"},
		{hash: "already-webp", size: 80, contentType: "image/webp", status: "skipped"},
	}
	for _, item := range items {
		if err := store.UpsertBlob(ctx, Blob{Hash: item.hash, SizeBytes: item.size, ContentType: item.contentType}); err != nil {
			t.Fatalf("upsert %s: %v", item.hash, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, encoding_source_size_bytes=? WHERE hash=?`, item.status, item.sourceSize, item.hash); err != nil {
			t.Fatalf("set encoding state for %s: %v", item.hash, err)
		}
	}

	counts, err := store.EncodingCounts(ctx)
	if err != nil {
		t.Fatalf("encoding counts: %v", err)
	}
	want := EncodingCounts{
		Candidates: 6, Pending: 1, Processing: 2, Converted: 1,
		NotSmaller: 1, Failed: 1, AlreadyWebP: 1,
		RemainingBytes: 1500, SourceBytes: 1000, OutputBytes: 100,
		BytesSaved: 900, SavedPercent: 90,
	}
	if counts != want {
		t.Fatalf("encoding counts=%+v want=%+v", counts, want)
	}
}

func TestAlbumLifecycleAndStatusTransitions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateAlbum(ctx, Album{
		ID:               "album-a",
		OriginalFilename: "holiday.zip",
		SizeBytes:        100,
		Status:           AlbumStatusQueued,
		SourceKey:        "uploads/album-a.zip",
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	album, err := store.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != AlbumStatusQueued || album.PhotoCount != 0 {
		t.Fatalf("unexpected album: %+v", album)
	}
	if album.CreatedAt == "" || album.UpdatedAt == "" {
		t.Fatalf("expected timestamps to be populated: %+v", album)
	}

	// CreateAlbum is idempotent and must not clobber progress.
	if err := store.SetAlbumStatus(ctx, "album-a", AlbumStatusProcessing, ""); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if err := store.CreateAlbum(ctx, Album{ID: "album-a", OriginalFilename: "other.zip"}); err != nil {
		t.Fatalf("create album again: %v", err)
	}
	album, err = store.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.OriginalFilename != "holiday.zip" || album.Status != AlbumStatusProcessing {
		t.Fatalf("CreateAlbum clobbered existing row: %+v", album)
	}

	if err := store.MarkAlbumReady(ctx, "album-a", 2); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	album, err = store.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != AlbumStatusReady || album.PhotoCount != 2 || album.Error != "" {
		t.Fatalf("unexpected ready album: %+v", album)
	}

	if _, err := store.GetAlbum(ctx, "missing"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
}

func TestUpsertAlbumRefreshesUploadFieldsOnly(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateAlbum(ctx, Album{
		ID:               "album-a",
		OriginalFilename: "first.zip",
		SizeBytes:        10,
		Status:           AlbumStatusReady,
		SourceKey:        "uploads/album-a.zip",
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := store.MarkAlbumReady(ctx, "album-a", 7); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	if err := store.UpsertAlbum(ctx, Album{
		ID:               "album-a",
		OriginalFilename: "second.zip",
		SizeBytes:        20,
		Status:           AlbumStatusQueued,
		SourceKey:        "uploads/album-a.zip",
	}); err != nil {
		t.Fatalf("upsert album: %v", err)
	}

	album, err := store.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.OriginalFilename != "second.zip" || album.SizeBytes != 20 {
		t.Fatalf("expected refreshed upload fields, got %+v", album)
	}
	if album.Status != AlbumStatusReady || album.PhotoCount != 7 {
		t.Fatalf("expected progress to be preserved, got %+v", album)
	}
}

func TestGetAlbumBySourceKey(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateAlbum(ctx, Album{
		ID:               "album-a",
		OriginalFilename: "trip.zip",
		SizeBytes:        10,
		Status:           AlbumStatusQueued,
		SourceKey:        "uploads/2026/June/trip.zip",
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	album, err := store.GetAlbumBySourceKey(ctx, "uploads/2026/June/trip.zip")
	if err != nil {
		t.Fatalf("get album by source key: %v", err)
	}
	if album.ID != "album-a" || album.Status != AlbumStatusQueued {
		t.Fatalf("unexpected album: %+v", album)
	}

	if _, err := store.GetAlbumBySourceKey(ctx, "uploads/missing.zip"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("error = %v, want ErrAlbumNotFound", err)
	}
}

func TestPhotoInsertAndLookup(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedReadyAlbum(t, store, "album-a")

	photos := []Photo{
		{AlbumID: "album-a", Index: 0, Name: "b.png", Hash: "hash-b", Width: 4, Height: 2, Ratio: 2},
		{AlbumID: "album-a", Index: 1, Name: "a.png", Hash: "hash-a", Width: 2, Height: 4, Ratio: 0.5},
	}
	for _, photo := range photos {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}

	got, err := store.PhotosByAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("photos by album: %v", err)
	}
	if len(got) != 2 || got[0].Name != "b.png" || got[1].Name != "a.png" {
		t.Fatalf("photos should be ordered by index, got %+v", got)
	}

	photo, err := store.PhotoAt(ctx, "album-a", 1)
	if err != nil {
		t.Fatalf("photo at: %v", err)
	}
	if photo.Hash != "hash-a" || photo.Width != 2 || photo.Height != 4 {
		t.Fatalf("unexpected photo: %+v", photo)
	}

	if _, err := store.PhotoAt(ctx, "album-a", 9); !errors.Is(err, ErrPhotoNotFound) {
		t.Fatalf("expected ErrPhotoNotFound, got %v", err)
	}

	if err := store.DeletePhotos(ctx, "album-a"); err != nil {
		t.Fatalf("delete photos: %v", err)
	}
	got, err = store.PhotosByAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("photos by album: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected photos to be deleted, got %d", len(got))
	}
}

func TestUpsertBlobPreservesEmbeddingStatus(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 11, ContentType: "image/png"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	seedBlobStatus(t, store, "hash-a", EmbeddingStatusReady)

	// Re-extracting the same bytes must not reset an existing embedding state.
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 22, ContentType: "image/png"}); err != nil {
		t.Fatalf("upsert blob again: %v", err)
	}

	blob, err := store.GetBlob(ctx, "hash-a")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if blob.SizeBytes != 11 {
		t.Fatalf("duplicate upload changed stored size to %d", blob.SizeBytes)
	}
	if blob.EmbeddingStatus != EmbeddingStatusReady {
		t.Fatalf("expected embedding status preserved, got %+v", blob)
	}
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 22, ContentType: "image/jpeg", SourceRestored: true}); err != nil {
		t.Fatalf("restore missing object metadata: %v", err)
	}
	blob, err = store.GetBlob(ctx, "hash-a")
	if err != nil || blob.SizeBytes != 22 || blob.ContentType != "image/jpeg" || blob.EmbeddingStatus != EmbeddingStatusReady {
		t.Fatalf("restored object metadata: %+v %v", blob, err)
	}

	if _, err := store.GetBlob(ctx, "missing"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("expected ErrBlobNotFound, got %v", err)
	}
}

func TestEmbeddingCountsAndClaim(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for _, hash := range []string{"ready", "failed", "pending"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
	}
	seedBlobStatus(t, store, "ready", EmbeddingStatusReady)
	seedBlobStatus(t, store, "failed", EmbeddingStatusFailed)

	counts, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("embedding counts: %v", err)
	}
	if counts.Total != 3 || counts.Ready != 1 || counts.Failed != 1 || counts.Pending != 1 || counts.Processing != 0 {
		t.Fatalf("unexpected counts: %+v", counts)
	}

	// A claim moves the pending blob to processing, which keeps counting as
	// pending (not done yet) but is no longer claimable.
	leaseUntil := time.Now().Add(10 * time.Minute)
	claimed, err := store.ClaimPendingEmbeddings(ctx, 10, leaseUntil)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Hash != "pending" {
		t.Fatalf("expected only pending blob, got %+v", claimed)
	}
	if claimed[0].EmbeddingStatus != EmbeddingStatusProcessing {
		t.Fatalf("expected processing status, got %s", claimed[0].EmbeddingStatus)
	}

	counts, err = store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("embedding counts after claim: %v", err)
	}
	if counts.Pending != 1 || counts.Processing != 1 {
		t.Fatalf("expected pending=1 processing=1, got %+v", counts)
	}

	if again, err := store.ClaimPendingEmbeddings(ctx, 10, leaseUntil); err != nil || len(again) != 0 {
		t.Fatalf("expected empty claim, got %+v err=%v", again, err)
	}
}

// seedClaimBlobs inserts one blob per hash with a distinct created_at so claim
// ordering is deterministic (RFC3339Nano strings with trimmed trailing zeros do
// not sort chronologically on their own).
func seedClaimBlobs(t *testing.T, store *Store, hashes ...string) {
	t.Helper()
	ctx := context.Background()
	for i, hash := range hashes {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
		createdAt := time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
		if _, err := store.db.Exec(`UPDATE blobs SET created_at = ? WHERE hash = ?`, createdAt, hash); err != nil {
			t.Fatalf("backdate blob %s: %v", hash, err)
		}
	}
}

func claimedHashes(blobs []Blob) []string {
	hashes := make([]string, 0, len(blobs))
	for _, blob := range blobs {
		hashes = append(hashes, blob.Hash)
	}
	return hashes
}

func TestClaimOrdersByCreatedAndReclaimsExpiredLeases(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, store, "old", "middle", "new")

	claimed, err := store.ClaimPendingEmbeddings(ctx, 2, time.Now().Add(10*time.Minute))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := claimedHashes(claimed); len(got) != 2 || got[0] != "old" || got[1] != "middle" {
		t.Fatalf("expected oldest two blobs in created order, got %v", got)
	}

	// Let both leases lapse, then reclaim: all three blobs come back, oldest
	// first, including the still-pending "new".
	if _, err := store.RenewEmbeddingLeases(ctx, claimedHashes(claimed), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	reclaimed, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(10*time.Minute))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if got := claimedHashes(reclaimed); len(got) != 3 || got[0] != "old" || got[1] != "middle" || got[2] != "new" {
		t.Fatalf("expected expired and pending blobs reclaimed in order, got %v", got)
	}

	// Releasing hands blobs back to pending, so the next claim sees them again.
	if err := store.ReleaseEmbeddingClaims(ctx, claimedHashes(reclaimed)); err != nil {
		t.Fatalf("release: %v", err)
	}
	if again, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(10*time.Minute)); err != nil || len(again) != 3 {
		t.Fatalf("expected released blobs claimable again, got %d err=%v", len(again), err)
	}

	// Renewals and releases for hashes nobody claimed are no-ops.
	if _, err := store.RenewEmbeddingLeases(ctx, []string{"ghost"}, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("renew unknown: %v", err)
	}
	if err := store.ReleaseEmbeddingClaims(ctx, []string{"ghost"}); err != nil {
		t.Fatalf("release unknown: %v", err)
	}
}

func TestApplyEmbeddingResultsMixedBatch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, store, "a", "b", "c", "d")

	if _, err := store.ClaimPendingEmbeddings(ctx, 3, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	longError := strings.Repeat("x", 600)
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{
		{Hash: "a", Status: EmbeddingStatusReady, Vector: vec768(1)},
		{Hash: "b", Status: EmbeddingStatusFailed, Error: longError},
		{Hash: "c", Status: EmbeddingStatusPending},
		{Hash: "d", Status: EmbeddingStatusReady, Vector: vec768(9)},
		{Hash: "ghost", Status: EmbeddingStatusReady, Vector: vec768(9)},
	})
	if err != nil {
		t.Fatalf("apply results: %v", err)
	}
	if len(applied) != 2 || applied[0] != "a" || applied[1] != "b" {
		t.Fatalf("expected a and b applied, got %v", applied)
	}
	wantRejected := []RejectedEmbedding{
		{Hash: "c", Reason: EmbeddingRejectInvalidState},
		{Hash: "d", Reason: EmbeddingRejectNotClaimed},
		{Hash: "ghost", Reason: EmbeddingRejectUnknownHash},
	}
	if len(rejected) != len(wantRejected) {
		t.Fatalf("expected %d rejections, got %+v", len(wantRejected), rejected)
	}
	for i, want := range wantRejected {
		if rejected[i] != want {
			t.Fatalf("rejection %d: expected %+v, got %+v", i, want, rejected[i])
		}
	}

	blobA, err := store.GetBlob(ctx, "a")
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	if blobA.EmbeddingStatus != EmbeddingStatusReady || blobA.EmbeddingError != "" {
		t.Fatalf("expected a clean ready row, got %+v", blobA)
	}
	blobB, err := store.GetBlob(ctx, "b")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if blobB.EmbeddingStatus != EmbeddingStatusFailed || blobB.EmbeddingError != strings.Repeat("x", 512) {
		t.Fatalf("expected truncated failed error on b, got %+v", blobB)
	}
	// A rejected processing blob keeps its claim, so the worker that holds it
	// can still finish it.
	blobC, err := store.GetBlob(ctx, "c")
	if err != nil {
		t.Fatalf("get c: %v", err)
	}
	if blobC.EmbeddingStatus != EmbeddingStatusProcessing {
		t.Fatalf("expected c still processing, got %s", blobC.EmbeddingStatus)
	}

	if _, _, err := store.ApplyEmbeddingResults(ctx, nil); err != nil {
		t.Fatalf("empty batch should be a no-op: %v", err)
	}
}

func TestOpenAddsLeaseColumnAndResetsProcessing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")

	// Build a database with the pre-lease schema and a blob stuck in
	// processing, as a crashed process would have left it.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE blobs (
			hash             TEXT PRIMARY KEY,
			size_bytes       INTEGER NOT NULL,
			content_type     TEXT NOT NULL DEFAULT 'application/octet-stream',
			embedding_status TEXT NOT NULL DEFAULT 'pending',
			embedding        BLOB,
			embedding_error  TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL
		);
		INSERT INTO blobs (hash, size_bytes, content_type, embedding_status, created_at)
		VALUES ('stuck', 1, 'image/jpeg', 'processing', '2024-01-01T00:00:00Z');`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer store.Close()

	// The column exists and the stuck blob is claimable again.
	claimed, err := store.ClaimPendingEmbeddings(context.Background(), 10, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("claim after migration: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Hash != "stuck" {
		t.Fatalf("expected stuck blob reclaimed after migration, got %+v", claimed)
	}
}

func TestSearchAlbumsByNameMatchesSubstringsAndOnlyReady(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	seed := []struct {
		id     string
		name   string
		status AlbumStatus
	}{
		{id: "ready-new", name: "Holiday Trip.zip", status: AlbumStatusReady},
		{id: "ready-old", name: "holiday family.zip", status: AlbumStatusReady},
		{id: "pending", name: "Holiday Pending.zip", status: AlbumStatusQueued},
		{id: "other", name: "Weekend.zip", status: AlbumStatusReady},
	}
	for _, item := range seed {
		if err := store.CreateAlbum(ctx, Album{ID: item.id, OriginalFilename: item.name, Status: item.status}); err != nil {
			t.Fatalf("create album %s: %v", item.id, err)
		}
	}

	// Prefix queries still work: they are just substrings at position one.
	got, err := store.SearchAlbumsByName(ctx, "  HoLiDaY ", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 ready matches, got %d (%+v)", len(got), got)
	}
	for _, result := range got {
		if result.Album.ID == "pending" {
			t.Fatalf("pending album should not be searchable")
		}
	}

	// Mid-string queries must match too, not just prefixes.
	midString, err := store.SearchAlbumsByName(ctx, "liday", 10)
	if err != nil {
		t.Fatalf("search mid-string: %v", err)
	}
	if len(midString) != 2 {
		t.Fatalf("expected mid-string query to match both holiday albums, got %d (%+v)", len(midString), midString)
	}

	limited, err := store.SearchAlbumsByName(ctx, "", 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected limit to apply, got %d", len(limited))
	}

	// '%' must be treated literally, not as a LIKE wildcard.
	if err := store.CreateAlbum(ctx, Album{ID: "wild", OriginalFilename: "100%.zip", Status: AlbumStatusReady}); err != nil {
		t.Fatalf("create wildcard album: %v", err)
	}
	if err := store.CreateAlbum(ctx, Album{ID: "plain", OriginalFilename: "1000.zip", Status: AlbumStatusReady}); err != nil {
		t.Fatalf("create plain album: %v", err)
	}
	wildcard, err := store.SearchAlbumsByName(ctx, "100%", 10)
	if err != nil {
		t.Fatalf("search wildcard: %v", err)
	}
	if len(wildcard) != 1 || wildcard[0].Album.ID != "wild" {
		t.Fatalf("expected literal wildcard match, got %+v", wildcard)
	}
	// A wildcard in the middle must not act as "match anything".
	noMatch, err := store.SearchAlbumsByName(ctx, "1%0", 10)
	if err != nil {
		t.Fatalf("search wildcard: %v", err)
	}
	if len(noMatch) != 0 {
		t.Fatalf("expected '%%' to be literal, got %+v", noMatch)
	}
}

func TestSearchAlbumsByNameAttachesCoverFromFirstPhoto(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	seedReadyAlbum(t, store, "with-cover")
	seedReadyAlbum(t, store, "without-photos")
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "with-cover", Index: 0, Name: "cover.png", Hash: "hash-cover", Width: 4, Height: 2, Ratio: 2}); err != nil {
		t.Fatalf("insert cover photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "with-cover", Index: 1, Name: "second.png", Hash: "hash-second", Width: 3, Height: 3, Ratio: 1}); err != nil {
		t.Fatalf("insert second photo: %v", err)
	}

	got, err := store.SearchAlbumsByName(ctx, "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both albums, got %d (%+v)", len(got), got)
	}
	byID := make(map[string]AlbumSearchResult, len(got))
	for _, result := range got {
		byID[result.Album.ID] = result
	}

	cover := byID["with-cover"].Cover
	if cover == nil {
		t.Fatalf("expected cover from photo at index 0")
	}
	if cover.Index != 0 || cover.Width != 4 || cover.Height != 2 || cover.Ratio != 2 {
		t.Fatalf("unexpected cover: %+v", cover)
	}
	if byID["without-photos"].Cover != nil {
		t.Fatalf("expected no cover for an album without photos")
	}
}

func TestListReadyAlbumCovers(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedReadyAlbum(t, store, "album-a")
	seedReadyAlbum(t, store, "album-b")
	if err := store.CreateAlbum(ctx, Album{ID: "album-queued", OriginalFilename: "q.zip", Status: AlbumStatusQueued}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	for _, albumID := range []string{"album-a", "album-b", "album-queued"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: "hash-" + albumID, SizeBytes: 5}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
		if err := store.InsertPhoto(ctx, Photo{AlbumID: albumID, Index: 0, Name: "cover.png", Hash: "hash-" + albumID, Width: 1, Height: 1, Ratio: 1}); err != nil {
			t.Fatalf("insert cover: %v", err)
		}
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 1, Name: "second.png", Hash: "hash-album-a", Width: 2, Height: 2, Ratio: 1}); err != nil {
		t.Fatalf("insert second photo: %v", err)
	}

	covers, err := store.ListReadyAlbumCovers(ctx)
	if err != nil {
		t.Fatalf("list ready album covers: %v", err)
	}
	if len(covers) != 2 {
		t.Fatalf("expected only ready albums with photos, got %d: %+v", len(covers), covers)
	}
	byID := make(map[string]AlbumCover, len(covers))
	for _, cover := range covers {
		byID[cover.AlbumID] = cover
	}
	for _, albumID := range []string{"album-a", "album-b"} {
		cover, ok := byID[albumID]
		if !ok {
			t.Fatalf("missing cover for %s", albumID)
		}
		if cover.Cover.Index != 0 || cover.Cover.Name != "cover.png" {
			t.Fatalf("expected the index-0 cover for %s, got %+v", albumID, cover.Cover)
		}
		if cover.CreatedAt == "" {
			t.Fatalf("expected createdAt on the cover row for %s", albumID)
		}
	}
}

func TestListReadyAlbumPhotosAndPairs(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedReadyAlbum(t, store, "album-a")
	seedReadyAlbum(t, store, "album-b")
	if err := store.CreateAlbum(ctx, Album{ID: "album-pending", OriginalFilename: "p.zip", Status: AlbumStatusQueued}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-shared", SizeBytes: 5}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-shared", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-b", Index: 0, Name: "b.png", Hash: "hash-shared", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-pending", Index: 0, Name: "c.png", Hash: "hash-shared", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	photos, err := store.ListReadyAlbumPhotos(ctx)
	if err != nil {
		t.Fatalf("list ready album photos: %v", err)
	}
	if len(photos) != 2 {
		t.Fatalf("expected only ready album photos, got %d", len(photos))
	}

	albumPairs, err := store.ListPhotoBlobPairsByAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("list pairs by album: %v", err)
	}
	if len(albumPairs) != 1 || albumPairs[0].Photo.AlbumID != "album-a" {
		t.Fatalf("unexpected album pairs: %+v", albumPairs)
	}
}

func TestListPhotoBlobPairsByHashes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedReadyAlbum(t, store, "album-b")
	seedReadyAlbum(t, store, "album-a")

	for _, hash := range []string{"hash-x", "hash-y", "hash-orphan"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 5, ContentType: "image/png"}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
	}
	seedBlobStatus(t, store, "hash-y", EmbeddingStatusReady)
	// hash-orphan has no photo rows: the write-back resolves photos per hash,
	// and an unreferenced blob must simply not come back.
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-b", Index: 1, Name: "b1.png", Hash: "hash-x", Width: 2, Height: 3, Ratio: 0.5}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-b", Index: 0, Name: "b0.png", Hash: "hash-x", Width: 4, Height: 2, Ratio: 2}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 0, Name: "a0.png", Hash: "hash-x", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 1, Name: "a1.png", Hash: "hash-y", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	// Empty input answers empty without touching the database.
	empty, err := store.ListPhotoBlobPairsByHashes(ctx, nil)
	if err != nil {
		t.Fatalf("list by hashes (nil): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no pairs for nil input, got %+v", empty)
	}

	pairs, err := store.ListPhotoBlobPairsByHashes(ctx, []string{"hash-y", "hash-x", "hash-orphan", "hash-ghost"})
	if err != nil {
		t.Fatalf("list by hashes: %v", err)
	}
	if len(pairs) != 4 {
		t.Fatalf("expected 4 pairs, got %d: %+v", len(pairs), pairs)
	}
	// (album, idx) order, so the write-back streams batches in the keyset
	// order the migration and point IDs assume.
	want := []struct {
		album string
		idx   int
		hash  string
	}{
		{"album-a", 0, "hash-x"},
		{"album-a", 1, "hash-y"},
		{"album-b", 0, "hash-x"},
		{"album-b", 1, "hash-x"},
	}
	for i, w := range want {
		got := pairs[i]
		if got.Photo.AlbumID != w.album || got.Photo.Index != w.idx || got.Photo.Hash != w.hash {
			t.Fatalf("pair %d = %s:%d hash %s, want %s:%d hash %s", i, got.Photo.AlbumID, got.Photo.Index, got.Photo.Hash, w.album, w.idx, w.hash)
		}
		if got.Blob.Hash != w.hash || got.Blob.SizeBytes != 5 || got.Blob.ContentType != "image/png" {
			t.Fatalf("pair %d blob not joined: %+v", i, got.Blob)
		}
	}
	// The joined status comes from the blob row, ready for hash-y.
	if pairs[1].Blob.EmbeddingStatus != EmbeddingStatusReady {
		t.Fatalf("expected hash-y joined ready, got %s", pairs[1].Blob.EmbeddingStatus)
	}
}

func TestReadyPairCount(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedReadyAlbum(t, store, "album-a")
	seedReadyAlbum(t, store, "album-b")

	for _, hash := range []string{"hash-ready", "hash-pending", "hash-orphan"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
	}
	seedBlobStatus(t, store, "hash-ready", EmbeddingStatusReady)
	seedBlobStatus(t, store, "hash-orphan", EmbeddingStatusReady)

	// One ready blob shown twice (two albums) counts once per photo; the
	// pending pair and the ready blob no photo references do not count.
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 0, Name: "a0.png", Hash: "hash-ready", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-b", Index: 0, Name: "b0.png", Hash: "hash-ready", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 1, Name: "a1.png", Hash: "hash-pending", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	count, err := store.ReadyPairCount(ctx)
	if err != nil {
		t.Fatalf("ready pair count: %v", err)
	}
	if count != 2 {
		t.Fatalf("ready pair count = %d, want 2", count)
	}
}

func TestListReadyAlbumPhotoCounts(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateAlbum(ctx, Album{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady, PhotoCount: 2}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := store.CreateAlbum(ctx, Album{ID: "album-b", OriginalFilename: "b.zip", Status: AlbumStatusReady}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := store.CreateAlbum(ctx, Album{ID: "album-pending", OriginalFilename: "p.zip", Status: AlbumStatusQueued, PhotoCount: 3}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 5}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: i, Name: "a.png", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1}); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}

	counts, err := store.ListReadyAlbumPhotoCounts(ctx)
	if err != nil {
		t.Fatalf("list ready album photo counts: %v", err)
	}
	if len(counts) != 1 {
		t.Fatalf("expected only the ready album with photos, got %+v", counts)
	}
	if counts[0].AlbumID != "album-a" || counts[0].PhotoCount != 2 {
		t.Fatalf("unexpected count: %+v", counts[0])
	}
}

func TestOpenRequiresPath(t *testing.T) {
	if _, err := Open("   ", nil); err == nil {
		t.Fatalf("expected error for empty path")
	}
}

// TestOpenMigratesLegacyAlbumStatuses covers a catalog written before album
// statuses matched the wire format: READY must become SUCCEEDED and PENDING
// must fold into QUEUED, and reopening the migrated file must be harmless.
// The rewrite lives in migration 0001, so the legacy rows have to predate the
// first Open — a fresh catalog never holds them.
func TestOpenMigratesLegacyAlbumStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	ctx := context.Background()

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE albums (
			id                TEXT PRIMARY KEY,
			original_filename TEXT NOT NULL,
			size_bytes        INTEGER NOT NULL DEFAULT 0,
			status            TEXT NOT NULL,
			source_key        TEXT NOT NULL DEFAULT '',
			photo_count       INTEGER NOT NULL DEFAULT 0,
			error             TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL,
			updated_at        TEXT NOT NULL
		);
		INSERT INTO albums (id, original_filename, status, created_at, updated_at) VALUES
			('legacy-ready',  'ready.zip',   'READY',   '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z'),
			('legacy-pending','pending.zip', 'PENDING', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z');`); err != nil {
		t.Fatalf("seed legacy statuses: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// The first open rewrites the legacy rows; a second open is a no-op.
	for attempt := 0; attempt < 2; attempt++ {
		store, err := Open(path, nil)
		if err != nil {
			t.Fatalf("open catalog (attempt %d): %v", attempt, err)
		}
		ready, err := store.GetAlbum(ctx, "legacy-ready")
		if err != nil {
			t.Fatalf("get legacy-ready: %v", err)
		}
		if ready.Status != AlbumStatusReady {
			t.Fatalf("legacy READY status=%s want=%s", ready.Status, AlbumStatusReady)
		}
		pending, err := store.GetAlbum(ctx, "legacy-pending")
		if err != nil {
			t.Fatalf("get legacy-pending: %v", err)
		}
		if pending.Status != AlbumStatusQueued {
			t.Fatalf("legacy PENDING status=%s want=%s", pending.Status, AlbumStatusQueued)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close catalog (attempt %d): %v", attempt, err)
		}
	}
}

// TestBackupToRoundTrips verifies the snapshot the S3 backup uploads is a real
// catalog: it carries every album, photo and blob — embedding bookkeeping
// included — and opens cleanly on its own. Vectors themselves are no longer in
// the file once migration 0003 has run; only the statuses survive.
func TestBackupToRoundTrips(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateAlbum(ctx, Album{
		ID:               "album-a",
		OriginalFilename: "trip.zip",
		SizeBytes:        10,
		Status:           AlbumStatusReady,
		SourceKey:        "uploads/album-a.zip",
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 5, ContentType: "image/png"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	seedBlobStatus(t, store, "hash-a", EmbeddingStatusReady)
	if err := store.InsertPhoto(ctx, Photo{AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-a", Width: 2, Height: 1, Ratio: 2}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := store.BackupTo(ctx, backupPath); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}

	restored, err := Open(backupPath, nil)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer restored.Close()

	album, err := restored.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album from snapshot: %v", err)
	}
	if album.Status != AlbumStatusReady || album.SourceKey != "uploads/album-a.zip" || album.PhotoCount != 0 {
		t.Fatalf("unexpected album in snapshot: %+v", album)
	}
	blob, err := restored.GetBlob(ctx, "hash-a")
	if err != nil {
		t.Fatalf("get blob from snapshot: %v", err)
	}
	if blob.EmbeddingStatus != EmbeddingStatusReady || blob.EmbeddingError != "" {
		t.Fatalf("embedding status did not survive the snapshot: %+v", blob)
	}
	photos, err := restored.PhotosByAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("photos from snapshot: %v", err)
	}
	if len(photos) != 1 || photos[0].Hash != "hash-a" {
		t.Fatalf("unexpected photos in snapshot: %+v", photos)
	}
}

// TestBackupToRefusesAnExistingTarget pins the VACUUM INTO contract the
// finalizer relies on: it never overwrites, so a fresh temp path is required.
func TestBackupToRefusesAnExistingTarget(t *testing.T) {
	store := openTestStore(t)

	target := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(target, []byte("occupied"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	if err := store.BackupTo(context.Background(), target); err == nil {
		t.Fatalf("expected BackupTo to refuse an existing target")
	}
}

func seedReadyAlbum(t *testing.T, store *Store, albumID string) {
	t.Helper()
	if err := store.CreateAlbum(context.Background(), Album{
		ID:               albumID,
		OriginalFilename: albumID + ".zip",
		Status:           AlbumStatusReady,
	}); err != nil {
		t.Fatalf("create album %s: %v", albumID, err)
	}
}

func TestEmbeddingCountsByAlbumCountsSharedBlobsPerAlbum(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer store.Close()

	seedReadyAlbum(t, store, "album-a")
	seedReadyAlbum(t, store, "album-b")

	// The two albums share one blob, so the global count is smaller than the
	// sum of the per-album counts. Per-album progress has to count the shared
	// blob for both, because both albums are waiting for it.
	photos := []Photo{
		{AlbumID: "album-a", Index: 0, Name: "a.jpg", Hash: "hash-shared", Width: 4, Height: 2, Ratio: 2},
		{AlbumID: "album-a", Index: 1, Name: "b.jpg", Hash: "hash-only-a", Width: 4, Height: 2, Ratio: 2},
		{AlbumID: "album-b", Index: 0, Name: "a.jpg", Hash: "hash-shared", Width: 4, Height: 2, Ratio: 2},
	}
	for _, photo := range photos {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo %s:%d: %v", photo.AlbumID, photo.Index, err)
		}
	}
	for _, hash := range []string{"hash-shared", "hash-only-a"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
	}
	seedBlobStatus(t, store, "hash-shared", EmbeddingStatusReady)

	global, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("global counts: %v", err)
	}
	if global.Total != 2 || global.Ready != 1 || global.Pending != 1 {
		t.Fatalf("global counts=%+v want total=2 ready=1 pending=1", global)
	}

	albumA, err := store.EmbeddingCountsByAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("album-a counts: %v", err)
	}
	if albumA.Total != 2 || albumA.Ready != 1 || albumA.Pending != 1 || albumA.Failed != 0 {
		t.Fatalf("album-a counts=%+v want total=2 ready=1 pending=1 failed=0", albumA)
	}

	albumB, err := store.EmbeddingCountsByAlbum(ctx, "album-b")
	if err != nil {
		t.Fatalf("album-b counts: %v", err)
	}
	if albumB.Total != 1 || albumB.Ready != 1 || albumB.Pending != 0 {
		t.Fatalf("album-b counts=%+v want total=1 ready=1 pending=0", albumB)
	}

	empty, err := store.EmbeddingCountsByAlbum(ctx, "album-missing")
	if err != nil {
		t.Fatalf("missing album counts: %v", err)
	}
	if empty.Total != 0 || empty.Pending != 0 {
		t.Fatalf("missing album counts=%+v want all zero", empty)
	}
}

func TestApplyEmbeddingResultsRejectsBlankHash(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{
		{Hash: "   ", Status: EmbeddingStatusReady, Vector: []float32{1}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("applied=%v want none", applied)
	}
	if len(rejected) != 1 || rejected[0].Reason != EmbeddingRejectInvalidHash {
		t.Fatalf("rejected=%+v want invalid_hash", rejected)
	}
}

func TestApplyEmbeddingResultsReadyClearsErrorAndTruncatesUTF8(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A failed report whose message is cut mid-rune still stores valid UTF-8:
	// the cut lands inside the Korean run, and the trailing partial rune is
	// dropped instead of being persisted as garbage.
	torn := strings.Repeat("에", 200) + "abc"
	_, _, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{
		{Hash: "hash-a", Status: EmbeddingStatusFailed, Error: torn},
	})
	if err != nil {
		t.Fatalf("apply failed report: %v", err)
	}
	blob, err := store.GetBlob(ctx, "hash-a")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if len(blob.EmbeddingError) > maxEmbeddingErrorBytes {
		t.Fatalf("error text=%d bytes want<=%d", len(blob.EmbeddingError), maxEmbeddingErrorBytes)
	}
	if !utf8.ValidString(blob.EmbeddingError) || !strings.HasSuffix(blob.EmbeddingError, "에") {
		t.Fatalf("truncation tore a rune: %q", blob.EmbeddingError[len(blob.EmbeddingError)-4:])
	}

	// A ready result that still carries an error field stores no error: a
	// success row quoting a failure would read as a contradiction everywhere
	// the blob is shown. (A failed blob itself is terminal, so the reverse
	// order is unreachable by design.)
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-b", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert blob-b: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("claim blob-b: %v", err)
	}
	if _, _, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{
		{Hash: "hash-b", Status: EmbeddingStatusReady, Vector: vec768(1), Error: "stale failure"},
	}); err != nil {
		t.Fatalf("apply ready report: %v", err)
	}
	blob, err = store.GetBlob(ctx, "hash-b")
	if err != nil {
		t.Fatalf("get blob-b: %v", err)
	}
	if blob.EmbeddingStatus != EmbeddingStatusReady || blob.EmbeddingError != "" {
		t.Fatalf("ready blob=%+v want clean ready", blob)
	}
}

func TestRenewEmbeddingLeasesReportsWhatRenewed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, hash := range []string{"hash-a", "hash-b", "hash-c"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
	}
	claimed, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claimed=%d want=3", len(claimed))
	}

	// A renewal covers exactly the still-processing rows: claimed ones yes, a
	// pending one it never held and a ghost no.
	renewed, err := store.RenewEmbeddingLeases(ctx,
		[]string{"hash-a", "hash-b", "hash-c", "blob-pending", "ghost"},
		time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	want := []string{"hash-a", "hash-b", "hash-c"}
	if strings.Join(renewed, ",") != strings.Join(want, ",") {
		t.Fatalf("renewed=%v want=%v", renewed, want)
	}
}

// TestApplyEmbeddingResultsRejectsWrongDimVector pins the batch contract: a
// ready result that does not fill the model's fixed width is refused per item —
// it must not abort the batch's transaction nor strand the blob's claim.
func TestApplyEmbeddingResultsRejectsWrongDimVector(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, store, "a", "b")

	if _, err := store.ClaimPendingEmbeddings(ctx, 2, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []EmbeddingResult{
		{Hash: "a", Status: EmbeddingStatusReady, Vector: []float32{1, 2, 3}},
		{Hash: "b", Status: EmbeddingStatusReady, Vector: vec768(1)},
	})
	if err != nil {
		t.Fatalf("one bad vector must not fail the batch: %v", err)
	}
	if len(applied) != 1 || applied[0] != "b" {
		t.Fatalf("applied=%v want only b", applied)
	}
	if len(rejected) != 1 || rejected[0].Hash != "a" || rejected[0].Reason != EmbeddingRejectWrongDim {
		t.Fatalf("rejected=%+v want a/wrong_dim", rejected)
	}

	// The refused blob keeps its processing claim so its worker can retry it
	// with a correct payload, and the accepted one is terminal.
	blobA, err := store.GetBlob(ctx, "a")
	if err != nil || blobA.EmbeddingStatus != EmbeddingStatusProcessing {
		t.Fatalf("blob a=%+v err=%v want still processing", blobA, err)
	}
	blobB, err := store.GetBlob(ctx, "b")
	if err != nil || blobB.EmbeddingStatus != EmbeddingStatusReady {
		t.Fatalf("blob b=%+v err=%v want ready", blobB, err)
	}
}

// seedBlobLease sets a blob's error text and lease directly, the fields the
// production write-back path manages but GetBlob does not surface. The reset
// and lease tests need to see them.
func seedBlobLease(t *testing.T, store *Store, hash string, errText string, leaseUntil int64) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE blobs SET embedding_error = ?, embedding_lease_until = ? WHERE hash = ?`, errText, leaseUntil, hash); err != nil {
		t.Fatalf("seed blob %s lease: %v", hash, err)
	}
}

// blobLeaseRow reads the raw embedding row back, including the lease column
// the Blob struct leaves out.
func blobLeaseRow(t *testing.T, store *Store, hash string) (EmbeddingStatus, string, int64) {
	t.Helper()
	var status, errText string
	var lease int64
	if err := store.db.QueryRow(`SELECT embedding_status, embedding_error, embedding_lease_until FROM blobs WHERE hash = ?`, hash).Scan(&status, &errText, &lease); err != nil {
		t.Fatalf("read blob %s: %v", hash, err)
	}
	return EmbeddingStatus(status), errText, lease
}

// TestAlbumPhotoBlobCounts pins the three library totals the admin page shows.
// One blob shared by two albums proves the blob count is unique content, not
// photo references.
func TestAlbumPhotoBlobCounts(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	albums := []Album{
		{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "b.zip", Status: AlbumStatusFailed},
		{ID: "album-c", OriginalFilename: "c.zip", Status: AlbumStatusQueued},
	}
	for _, album := range albums {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album: %v", err)
		}
	}
	photos := []Photo{
		{AlbumID: "album-a", Index: 0, Hash: "hash-shared"},
		{AlbumID: "album-b", Index: 0, Hash: "hash-shared"},
		{AlbumID: "album-b", Index: 1, Hash: "hash-only-b"},
	}
	for _, photo := range photos {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
		if err := store.UpsertBlob(ctx, Blob{Hash: photo.Hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
	}

	total, byStatus, err := store.AlbumCounts(ctx)
	if err != nil {
		t.Fatalf("album counts: %v", err)
	}
	if total != 3 {
		t.Fatalf("album total=%d want=3", total)
	}
	if len(byStatus) != 3 || byStatus[string(AlbumStatusReady)] != 1 ||
		byStatus[string(AlbumStatusFailed)] != 1 || byStatus[string(AlbumStatusQueued)] != 1 {
		t.Fatalf("album byStatus=%v want one of each", byStatus)
	}

	photoCount, err := store.PhotoCount(ctx)
	if err != nil || photoCount != 3 {
		t.Fatalf("photo count=%d err=%v want=3", photoCount, err)
	}
	blobCount, err := store.BlobCount(ctx)
	if err != nil || blobCount != 2 {
		t.Fatalf("blob count=%d err=%v want=2 (hash-shared is one blob)", blobCount, err)
	}
}

// TestAlbumCountsOnEmptyCatalog pins the empty case: zero albums means a zero
// total and an empty (not nil-panicking) breakdown.
func TestAlbumCountsOnEmptyCatalog(t *testing.T) {
	store := openTestStore(t)

	total, byStatus, err := store.AlbumCounts(context.Background())
	if err != nil {
		t.Fatalf("album counts: %v", err)
	}
	if total != 0 || len(byStatus) != 0 {
		t.Fatalf("album total=%d byStatus=%v want all empty", total, byStatus)
	}
}

// TestResetReadyEmbeddings pins the recovery path: ready and failed rows go
// back to pending with their error text and lease cleared, while processing
// rows are left strictly alone — a worker still holds that lease, and
// flipping the row under it would race the in-flight write-back.
func TestResetReadyEmbeddings(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for _, hash := range []string{"hash-ready", "hash-failed", "hash-processing", "hash-pending"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	seedBlobStatus(t, store, "hash-ready", EmbeddingStatusReady)
	seedBlobStatus(t, store, "hash-failed", EmbeddingStatusFailed)
	seedBlobStatus(t, store, "hash-processing", EmbeddingStatusProcessing)
	// Error text and lease on the rows that carry them, so the reset has
	// something to clear (and the processing row proves nothing was cleared).
	seedBlobLease(t, store, "hash-ready", "stale success note", 111)
	seedBlobLease(t, store, "hash-failed", "embed exploded", 222)
	seedBlobLease(t, store, "hash-processing", "worker still running", 333)

	reset, err := store.ResetReadyEmbeddings(ctx)
	if err != nil {
		t.Fatalf("reset ready embeddings: %v", err)
	}
	if reset != 2 {
		t.Fatalf("reset=%d want=2 (one ready, one failed)", reset)
	}

	status, errText, lease := blobLeaseRow(t, store, "hash-ready")
	if status != EmbeddingStatusPending || errText != "" || lease != 0 {
		t.Fatalf("hash-ready=(%s %q %d) want (pending \"\" 0)", status, errText, lease)
	}
	status, errText, lease = blobLeaseRow(t, store, "hash-failed")
	if status != EmbeddingStatusPending || errText != "" || lease != 0 {
		t.Fatalf("hash-failed=(%s %q %d) want (pending \"\" 0)", status, errText, lease)
	}
	status, errText, lease = blobLeaseRow(t, store, "hash-processing")
	if status != EmbeddingStatusProcessing || errText != "worker still running" || lease != 333 {
		t.Fatalf("hash-processing=(%s %q %d) want untouched processing row", status, errText, lease)
	}
	status, _, lease = blobLeaseRow(t, store, "hash-pending")
	if status != EmbeddingStatusPending || lease != 0 {
		t.Fatalf("hash-pending=(%s %d) want untouched pending row", status, lease)
	}

	// A second reset is a no-op: nothing terminal is left to flip.
	reset, err = store.ResetReadyEmbeddings(ctx)
	if err != nil || reset != 0 {
		t.Fatalf("second reset=%d err=%v want 0", reset, err)
	}
}
