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
	unknownCategory, err := store.AdminPhotos(ctx, AdminPhotoFilters{Encoding: "unknown"}, "", 0, 20)
	if err != nil || len(unknownCategory) != 2 {
		t.Fatalf("unknown encoding category=%+v err=%v; missing blob must not be an unknown blob", unknownCategory, err)
	}
	// The missingBlob filter is the photos-side integrity drilldown: it must
	// return exactly the rows whose hash has no blob, and its negation must
	// exclude them without disturbing any other filter.
	missing := true
	dangling, err := store.AdminPhotos(ctx, AdminPhotoFilters{MissingBlob: &missing}, "", 0, 20)
	if err != nil || len(dangling) != 1 || dangling[0].Name != "dangling.jpg" || dangling[0].Hash != "missing-blob" || !dangling[0].MissingBlob {
		t.Fatalf("missing-blob list=%+v err=%v want only the dangling photo row", dangling, err)
	}
	present := false
	attached, err := store.AdminPhotos(ctx, AdminPhotoFilters{MissingBlob: &present, AlbumID: "album-a"}, "", 0, 20)
	if err != nil || len(attached) != 6 {
		t.Fatalf("present-blob list=%+v err=%v want album-a's six blob-backed photos", attached, err)
	}
	for _, photo := range attached {
		if photo.MissingBlob {
			t.Fatalf("present-blob filter returned a dangling photo: %+v", photo)
		}
	}
	photo, err := store.AdminPhotoAt(ctx, "album-b", 2)
	if err != nil || photo.Name != "eligible.jpg" || photo.EncodingError != "encode err" {
		t.Fatalf("photo detail=%+v err=%v", photo, err)
	}
	danglingDetail, err := store.AdminPhotoAt(ctx, "album-a", 6)
	if err != nil || !danglingDetail.MissingBlob || danglingDetail.SizeBytes != nil || danglingDetail.ContentType != nil || danglingDetail.EncodingRaw != "" || danglingDetail.Encoding != "" || danglingDetail.EmbeddingRaw != "" || danglingDetail.Embedding != "" {
		t.Fatalf("dangling photo=%+v err=%v want explicit missing blob and unavailable metadata", danglingDetail, err)
	}
}

func TestAdminBlobDetailRelationsAndAlbumMetadata(t *testing.T) {
	store := openAdminBrowseCatalog(t)
	seedAdminBrowseCatalog(t, store)
	ctx := context.Background()

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

	albums, err := store.AdminAlbums(ctx, AdminAlbumFilters{Status: string(AlbumStatusFailed)}, "", 10)
	if err != nil || len(albums) != 1 || albums[0].ID != "album-b" || albums[0].Error != "extract error" || albums[0].PhotoCount != 3 {
		t.Fatalf("failed album list=%+v err=%v", albums, err)
	}
}
