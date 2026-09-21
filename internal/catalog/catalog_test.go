package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
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

func TestBlobUpsertPreservesEmbedding(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 11, ContentType: "image/png"}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := store.SetBlobEmbedding(ctx, "hash-a", EmbeddingStatusReady, []float32{0.25, -1, 3}, ""); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	// Re-extracting the same bytes must not reset an existing embedding.
	if err := store.UpsertBlob(ctx, Blob{Hash: "hash-a", SizeBytes: 22, ContentType: "image/png"}); err != nil {
		t.Fatalf("upsert blob again: %v", err)
	}

	blob, err := store.GetBlob(ctx, "hash-a")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if blob.SizeBytes != 22 {
		t.Fatalf("expected refreshed size, got %d", blob.SizeBytes)
	}
	if blob.EmbeddingStatus != EmbeddingStatusReady || len(blob.Embedding) != 3 {
		t.Fatalf("expected embedding preserved, got %+v", blob)
	}
	if blob.Embedding[0] != 0.25 || blob.Embedding[1] != -1 || blob.Embedding[2] != 3 {
		t.Fatalf("embedding roundtrip mismatch: %v", blob.Embedding)
	}

	if _, err := store.GetBlob(ctx, "missing"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("expected ErrBlobNotFound, got %v", err)
	}
}

func TestEmbeddingCountsAndPendingListing(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for _, hash := range []string{"ready", "failed", "pending"} {
		if err := store.UpsertBlob(ctx, Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob %s: %v", hash, err)
		}
	}
	if err := store.SetBlobEmbedding(ctx, "ready", EmbeddingStatusReady, []float32{1}, ""); err != nil {
		t.Fatalf("set ready: %v", err)
	}
	if err := store.SetBlobEmbedding(ctx, "failed", EmbeddingStatusFailed, nil, "boom"); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	counts, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("embedding counts: %v", err)
	}
	if counts.Total != 3 || counts.Ready != 1 || counts.Failed != 1 || counts.Pending != 1 {
		t.Fatalf("unexpected counts: %+v", counts)
	}

	awaiting, err := store.ListBlobsAwaitingEmbedding(ctx, 10)
	if err != nil {
		t.Fatalf("list awaiting: %v", err)
	}
	if len(awaiting) != 1 || awaiting[0].Hash != "pending" {
		t.Fatalf("expected only pending blob, got %+v", awaiting)
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

	pairs, err := store.ListPhotoBlobPairs(ctx)
	if err != nil {
		t.Fatalf("list pairs: %v", err)
	}
	if len(pairs) != 3 {
		t.Fatalf("expected all pairs, got %d", len(pairs))
	}
	for _, pair := range pairs {
		if pair.Blob.Hash != "hash-shared" {
			t.Fatalf("expected joined blob, got %+v", pair)
		}
	}

	albumPairs, err := store.ListPhotoBlobPairsByAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("list pairs by album: %v", err)
	}
	if len(albumPairs) != 1 || albumPairs[0].Photo.AlbumID != "album-a" {
		t.Fatalf("unexpected album pairs: %+v", albumPairs)
	}
}

func TestVectorEncodeDecodeRoundTrip(t *testing.T) {
	if got := DecodeVector(EncodeVector(nil)); got != nil {
		t.Fatalf("expected nil for empty vector, got %v", got)
	}
	if got := DecodeVector([]byte{1, 2, 3}); got != nil {
		t.Fatalf("expected nil for truncated vector, got %v", got)
	}

	in := []float32{0, 1.5, -2.25, 1e-8}
	out := DecodeVector(EncodeVector(in))
	if len(out) != len(in) {
		t.Fatalf("length mismatch: %d vs %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("value %d mismatch: %v vs %v", i, out[i], in[i])
		}
	}
}

func TestOpenRequiresPath(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Fatalf("expected error for empty path")
	}
}

// TestOpenMigratesLegacyAlbumStatuses covers a catalog written before album
// statuses matched the wire format: READY must become SUCCEEDED and PENDING
// must fold into QUEUED, and reopening the migrated file must be harmless.
func TestOpenMigratesLegacyAlbumStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	ctx := context.Background()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	for _, album := range []Album{
		{ID: "legacy-ready", OriginalFilename: "ready.zip", Status: AlbumStatusReady},
		{ID: "legacy-pending", OriginalFilename: "pending.zip", Status: AlbumStatusQueued},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album %s: %v", album.ID, err)
		}
	}
	if _, err := store.db.Exec(`
		UPDATE albums SET status = 'READY' WHERE id = 'legacy-ready';
		UPDATE albums SET status = 'PENDING' WHERE id = 'legacy-pending';`); err != nil {
		t.Fatalf("seed legacy statuses: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	// Opening rewrites the legacy rows, and a second open is a no-op.
	for attempt := 0; attempt < 2; attempt++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen catalog (attempt %d): %v", attempt, err)
		}
		ready, err := reopened.GetAlbum(ctx, "legacy-ready")
		if err != nil {
			t.Fatalf("get legacy-ready: %v", err)
		}
		if ready.Status != AlbumStatusReady {
			t.Fatalf("legacy READY status=%s want=%s", ready.Status, AlbumStatusReady)
		}
		pending, err := reopened.GetAlbum(ctx, "legacy-pending")
		if err != nil {
			t.Fatalf("get legacy-pending: %v", err)
		}
		if pending.Status != AlbumStatusQueued {
			t.Fatalf("legacy PENDING status=%s want=%s", pending.Status, AlbumStatusQueued)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("close reopened catalog: %v", err)
		}
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
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
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
	if err := store.SetBlobEmbedding(ctx, "hash-shared", EmbeddingStatusReady, []float32{1, 0}, ""); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

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
