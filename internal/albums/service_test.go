package albums

import (
	"context"
	"reflect"
	"testing"

	"viewer/internal/models"
)

func TestAllAlbumsReturnsDeterministicOrder(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{
			"album-c": {AlbumID: "album-c", CreatedAt: "2026-02-15T00:00:03Z"},
			"album-a": {AlbumID: "album-a", CreatedAt: "2026-02-15T00:00:01Z"},
			"album-b": {AlbumID: "album-b", CreatedAt: "2026-02-15T00:00:02Z"},
		},
	}

	want := []string{"album-a", "album-b", "album-c"}
	for i := 0; i < 20; i++ {
		gotAlbums := s.AllAlbums()
		got := make([]string, 0, len(gotAlbums))
		for _, album := range gotAlbums {
			got = append(got, album.AlbumID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("all albums order mismatch, got=%v want=%v", got, want)
		}
	}
}

func TestAllAlbumsReturnsCopies(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{
			"album-a": {
				AlbumID:   "album-a",
				CreatedAt: "2026-02-15T00:00:01Z",
				Photos: []models.PhotoMeta{
					{I: 0, Name: "001.png", W: 100, H: 100},
				},
			},
		},
	}

	got := s.AllAlbums()
	if len(got) != 1 {
		t.Fatalf("expected 1 album, got %d", len(got))
	}
	if got[0] == s.albumCache["album-a"] {
		t.Fatalf("expected AllAlbums to return a copy, got shared pointer")
	}

	got[0].AlbumID = "changed"
	got[0].Photos[0].Name = "changed.png"

	again := s.AllAlbums()
	if again[0].AlbumID != "album-a" {
		t.Fatalf("expected original album id to stay unchanged, got %q", again[0].AlbumID)
	}
	if again[0].Photos[0].Name != "001.png" {
		t.Fatalf("expected original photo name to stay unchanged, got %q", again[0].Photos[0].Name)
	}
}

func TestListAlbumsDoesNotBlockOnEmptyCache(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{},
	}

	albumsList, err := s.ListAlbums(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(albumsList) != 0 {
		t.Fatalf("expected empty albums list, got %d", len(albumsList))
	}
}

func TestMergeAlbumCachesPreservesExistingAndOverwritesScanned(t *testing.T) {
	existing := map[string]*models.AlbumIndex{
		"from-memory": {AlbumID: "from-memory", CreatedAt: "2026-02-15T00:00:01Z"},
		"shared":      {AlbumID: "shared", CreatedAt: "old"},
	}
	scanned := map[string]*models.AlbumIndex{
		"from-storage": {AlbumID: "from-storage", CreatedAt: "2026-02-15T00:00:02Z"},
		"shared":       {AlbumID: "shared", CreatedAt: "new"},
	}

	merged := mergeAlbumCaches(existing, scanned)
	if len(merged) != 3 {
		t.Fatalf("expected merged size 3, got %d", len(merged))
	}
	if _, ok := merged["from-memory"]; !ok {
		t.Fatalf("expected merged cache to keep existing in-memory album")
	}
	if _, ok := merged["from-storage"]; !ok {
		t.Fatalf("expected merged cache to include scanned storage album")
	}
	if merged["shared"].CreatedAt != "new" {
		t.Fatalf("expected scanned album to overwrite shared key, got %q", merged["shared"].CreatedAt)
	}
}
