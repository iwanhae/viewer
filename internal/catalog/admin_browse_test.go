package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openAdminBrowseCatalog(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedAdminBrowseCatalog(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	for _, album := range []Album{
		{ID: "album-a", OriginalFilename: "Alpha.zip", Status: AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "Beta.zip", Status: AlbumStatusFailed, Error: "extract error"},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album %s: %v", album.ID, err)
		}
	}
	blobs := []struct {
		hash, contentType, encoding, embedding, encodingError, embeddingError string
		gate                                                                  bool
		size                                                                  int64
	}{
		{"shared", "image/jpeg", "done", "ready", "", "", false, 12},
		{"blocked", "image/jpeg", "pending", "pending", "", "", true, 13},
		{"webp", "image/webp", "skipped", "pending", "", "", true, 14},
		{"png", "image/png", "skipped", "pending", "skip err", "embed err", true, 15},
		{"unknown", "image/jpeg", "future-encoding", "future-embedding", "enc err", "embed err", true, 16},
		{"gate-open", "image/jpeg", "failed", "pending", "encode err", "", true, 17},
		{"orphan", "image/jpeg", "future-encoding", "future-embedding", "orphan enc", "orphan embed", true, 18},
		{"unknown-other", "image/jpeg", "future-encoding-other", "future-embedding-other", "enc err 2", "embed err 2", true, 19},
	}
	for _, blob := range blobs {
		if err := store.UpsertBlob(ctx, Blob{Hash: blob.hash, SizeBytes: blob.size, ContentType: blob.contentType}); err != nil {
			t.Fatalf("upsert blob %s: %v", blob.hash, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, embedding_status=?, encoding_error=?, embedding_error=?, encoding_gate=? WHERE hash=?`,
			blob.encoding, blob.embedding, blob.encodingError, blob.embeddingError, blob.gate, blob.hash); err != nil {
			t.Fatalf("set blob states %s: %v", blob.hash, err)
		}
	}
	photos := []Photo{
		{AlbumID: "album-a", Index: 0, Name: `img%_\special.jpg`, Hash: "shared", Width: 10, Height: 20},
		{AlbumID: "album-a", Index: 1, Name: "duplicate.jpg", Hash: "shared", Width: 10, Height: 20},
		{AlbumID: "album-a", Index: 2, Name: "blocked.jpg", Hash: "blocked"},
		{AlbumID: "album-a", Index: 3, Name: "webp.jpg", Hash: "webp"},
		{AlbumID: "album-a", Index: 4, Name: "unknown.jpg", Hash: "unknown"},
		{AlbumID: "album-a", Index: 5, Name: "unknown-other.jpg", Hash: "unknown-other"},
		{AlbumID: "album-a", Index: 6, Name: "dangling.jpg", Hash: "missing-blob"},
		{AlbumID: "album-b", Index: 0, Name: "shared.jpg", Hash: "shared"},
		{AlbumID: "album-b", Index: 1, Name: "png.jpg", Hash: "png"},
		{AlbumID: "album-b", Index: 2, Name: "eligible.jpg", Hash: "gate-open"},
	}
	for _, photo := range photos {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo %s:%d: %v", photo.AlbumID, photo.Index, err)
		}
	}
}

func TestAdminPhotoBrowseFiltersAndLiteralSearch(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	seedAdminBrowseCatalog(t, store)
	ctx := context.Background()

	photos, err := store.AdminPhotos(ctx, AdminPhotoFilters{Query: `%_\`}, "", 0, 20)
	if err != nil {
		t.Fatalf("literal search: %v", err)
	}
	if len(photos) != 1 || photos[0].Name != `img%_\special.jpg` {
		t.Fatalf("literal search matches=%+v want only the literal wildcard-name photo", photos)
	}
	if photos[0].AlbumID != "album-a" || photos[0].AlbumName != "Alpha.zip" || photos[0].Width != 10 || photos[0].Height != 20 || photos[0].SizeBytes == nil || *photos[0].SizeBytes != 12 || photos[0].ContentType == nil || *photos[0].ContentType != "image/jpeg" {
		t.Fatalf("photo metadata=%+v", photos[0])
	}
	if photos[0].EncodingRaw != "done" || photos[0].Encoding != "completed" || photos[0].EmbeddingRaw != "ready" || photos[0].Embedding != "completed" {
		t.Fatalf("photo pipeline state=%+v", photos[0])
	}

	queued, err := store.AdminPhotos(ctx, AdminPhotoFilters{Embedding: "queued"}, "", 0, 20)
	if err != nil || len(queued) != 4 || queued[0].Name != "blocked.jpg" || queued[1].Name != "webp.jpg" || queued[2].Name != "png.jpg" || queued[3].Name != "eligible.jpg" {
		t.Fatalf("queued list=%+v err=%v; want all pending rows including legacy gated rows", queued, err)
	}
	unknown, err := store.AdminPhotos(ctx, AdminPhotoFilters{Encoding: "unknown", Embedding: "unknown", EncodingRaw: "future-encoding", EmbeddingRaw: "future-embedding"}, "", 0, 20)
	if err != nil || len(unknown) != 1 || unknown[0].EncodingRaw != "future-encoding" || unknown[0].EmbeddingRaw != "future-embedding" {
		t.Fatalf("unknown raw drilldown=%+v err=%v", unknown, err)
	}
	unknownCategory, err := store.AdminPhotos(ctx, AdminPhotoFilters{Encoding: "unknown"}, "", 0, 20)
	if err != nil || len(unknownCategory) != 2 {
		t.Fatalf("unknown encoding category=%+v err=%v; missing blob must not be an unknown blob", unknownCategory, err)
	}
	skipped, err := store.AdminPhotos(ctx, AdminPhotoFilters{AlbumID: "album-a", SkippedReason: "webp"}, "", 0, 20)
	if err != nil || len(skipped) != 1 || skipped[0].SkippedReason != "webp" {
		t.Fatalf("skipped webp list=%+v err=%v", skipped, err)
	}
	if skipped, err = store.AdminPhotos(ctx, AdminPhotoFilters{SkippedReason: "nonWebp"}, "", 0, 20); err != nil || len(skipped) != 1 || skipped[0].SkippedReason != "nonWebp" {
		t.Fatalf("skipped nonWebp list=%+v err=%v", skipped, err)
	}
	photo, err := store.AdminPhotoAt(ctx, "album-b", 2)
	if err != nil || photo.Name != "eligible.jpg" || photo.EncodingError != "encode err" {
		t.Fatalf("photo detail=%+v err=%v", photo, err)
	}
	dangling, err := store.AdminPhotoAt(ctx, "album-a", 6)
	if err != nil || !dangling.MissingBlob || dangling.SizeBytes != nil || dangling.ContentType != nil || dangling.EncodingRaw != "" || dangling.Encoding != "" || dangling.EmbeddingRaw != "" || dangling.Embedding != "" {
		t.Fatalf("dangling photo=%+v err=%v want explicit missing blob and unavailable metadata", dangling, err)
	}
}

func TestAdminBlobBrowseOrphansRelationsAndAlbumPipelineCounts(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	seedAdminBrowseCatalog(t, store)
	ctx := context.Background()

	orphaned := true
	blobs, err := store.AdminBlobs(ctx, AdminBlobFilters{Unreferenced: &orphaned, Encoding: "unknown", EmbeddingRaw: "future-embedding"}, "", 20)
	if err != nil || len(blobs) != 1 || blobs[0].Hash != "orphan" || blobs[0].PhotoCount != 0 || blobs[0].AlbumCount != 0 {
		t.Fatalf("orphan blob list=%+v err=%v", blobs, err)
	}
	shared, err := store.AdminBlobByHash(ctx, "shared")
	if err != nil || shared.PhotoCount != 3 || shared.AlbumCount != 2 || shared.Encoding != "completed" {
		t.Fatalf("shared blob=%+v err=%v want three photo refs in two albums", shared, err)
	}
	if _, err := store.AdminBlobByHash(ctx, "absent"); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("missing blob error=%v want ErrBlobNotFound", err)
	}

	detail, err := store.AdminAlbumByID(ctx, "album-a")
	if err != nil {
		t.Fatalf("album detail: %v", err)
	}
	if detail.Status != string(AlbumStatusReady) || detail.PhotoCount != 7 || detail.MissingBlobPhotos != 1 {
		t.Fatalf("album metadata=%+v want SUCCEEDED, 7 photos, missingBlobPhotos=1", detail)
	}
	if got := detail.Encoding.Categories["completed"]; got != (PipelineCounts{BlobCount: 1, PhotoCount: 2, AlbumCount: 1}) {
		t.Fatalf("album encoding completed=%+v want shared blob once, two photo rows", got)
	}
	if got := detail.Encoding.SkippedReasons["webp"]; got != (PipelineCounts{BlobCount: 1, PhotoCount: 1, AlbumCount: 1}) {
		t.Fatalf("album skipped webp=%+v", got)
	}
	if got := detail.Embedding.Categories["queued"]; got != (PipelineCounts{BlobCount: 2, PhotoCount: 2, AlbumCount: 1}) {
		t.Fatalf("album queued embedding=%+v", got)
	}
	if got := detail.Encoding.Categories["unknown"]; got != (PipelineCounts{BlobCount: 2, PhotoCount: 2, AlbumCount: 1}) {
		t.Fatalf("album unknown encoding aggregate=%+v want distinct album and both raw unknown states", got)
	}
	if _, ok := detail.Encoding.RawStates["future-encoding"]; !ok {
		t.Fatalf("album raw unknown state missing: %v", detail.Encoding.RawStates)
	}
	if _, ok := detail.Encoding.RawStates["future-encoding-other"]; !ok {
		t.Fatalf("album second raw unknown state missing: %v", detail.Encoding.RawStates)
	}
	var pipelinePhotos int64
	for _, counts := range detail.Encoding.Categories {
		pipelinePhotos += counts.PhotoCount
	}
	if pipelinePhotos+detail.MissingBlobPhotos != detail.PhotoCount {
		t.Fatalf("album photo accounting pipeline=%d missing=%d total=%d", pipelinePhotos, detail.MissingBlobPhotos, detail.PhotoCount)
	}

	albums, err := store.AdminAlbums(ctx, AdminAlbumFilters{Status: string(AlbumStatusFailed)}, "", 10)
	if err != nil || len(albums) != 1 || albums[0].ID != "album-b" || albums[0].Error != "extract error" || albums[0].PhotoCount != 3 {
		t.Fatalf("failed album list=%+v err=%v", albums, err)
	}
}
