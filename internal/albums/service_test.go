package albums

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestListAlbumsSnapshotInvalidatesOnCacheMutation(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{
			"album-a": {AlbumID: "album-a", CreatedAt: "2026-02-15T00:00:01Z"},
		},
	}

	first, err := s.ListAlbums(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected one album in first snapshot, got %d", len(first))
	}

	s.mu.Lock()
	s.albumCache["album-b"] = &models.AlbumIndex{AlbumID: "album-b", CreatedAt: "2026-02-15T00:00:02Z"}
	s.invalidateSnapshotsLocked()
	s.mu.Unlock()

	second, err := s.ListAlbums(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("expected two albums after invalidation, got %d", len(second))
	}
}

func TestSearchAlbumsByNamePrefixMatchesCaseInsensitiveAndOrdersByCreatedAt(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{
			"album-new": {
				AlbumID:          "album-new",
				OriginalFilename: "Holiday Trip.zip",
				CreatedAt:        "2026-02-17T10:00:00Z",
				PhotoCount:       2,
				Photos: []models.PhotoMeta{
					{I: 0},
					{I: 1},
				},
			},
			"album-old": {
				AlbumID:          "album-old",
				OriginalFilename: "holiday family.zip",
				CreatedAt:        "2026-02-16T10:00:00Z",
				PhotoCount:       1,
				Photos: []models.PhotoMeta{
					{I: 0},
				},
			},
			"album-other": {
				AlbumID:          "album-other",
				OriginalFilename: "Weekend.zip",
				CreatedAt:        "2026-02-18T10:00:00Z",
				PhotoCount:       1,
				Photos: []models.PhotoMeta{
					{I: 0},
				},
			},
		},
	}

	got, err := s.SearchAlbumsByNamePrefix(context.Background(), "  HoLiDaY ", 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 albums, got %d", len(got))
	}
	if got[0].AlbumID != "album-new" || got[1].AlbumID != "album-old" {
		t.Fatalf("unexpected ordering: %+v", got)
	}
}

func TestSearchAlbumsByNamePrefixSupportsEmptyQueryAndLimit(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{
			"album-a": {AlbumID: "album-a", OriginalFilename: "A.zip", CreatedAt: "2026-02-15T10:00:00Z"},
			"album-b": {AlbumID: "album-b", OriginalFilename: "B.zip", CreatedAt: "2026-02-17T10:00:00Z"},
			"album-c": {AlbumID: "album-c", OriginalFilename: "C.zip", CreatedAt: "2026-02-16T10:00:00Z"},
		},
	}

	got, err := s.SearchAlbumsByNamePrefix(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 album due to limit, got %d", len(got))
	}
	if got[0].AlbumID != "album-b" {
		t.Fatalf("expected newest album first, got %q", got[0].AlbumID)
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

func TestAllAlbumsSnapshotReturnsDefensiveCopies(t *testing.T) {
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

	first := s.AllAlbums()
	if len(first) != 1 {
		t.Fatalf("expected one album, got %d", len(first))
	}
	first[0].AlbumID = "changed"
	first[0].Photos[0].Name = "changed.png"

	second := s.AllAlbums()
	if second[0].AlbumID != "album-a" {
		t.Fatalf("expected cached snapshot clone to keep original album id, got %q", second[0].AlbumID)
	}
	if second[0].Photos[0].Name != "001.png" {
		t.Fatalf("expected cached snapshot clone to keep original photo name, got %q", second[0].Photos[0].Name)
	}
}

type fakeAlbumStore struct {
	forEachAlbumIndexKeyFn func(ctx context.Context, fn func(key string) error) error
	readJSONFn             func(ctx context.Context, key string, out any) error
}

func (f *fakeAlbumStore) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error) {
	panic("unexpected call")
}

func (f *fakeAlbumStore) HeadObject(ctx context.Context, key string) (bool, int64, error) {
	panic("unexpected call")
}

func (f *fakeAlbumStore) GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error) {
	panic("unexpected call")
}

func (f *fakeAlbumStore) PutJSON(ctx context.Context, key string, v any) error {
	panic("unexpected call")
}

func (f *fakeAlbumStore) ReadJSON(ctx context.Context, key string, out any) error {
	if f.readJSONFn == nil {
		panic("unexpected call")
	}
	return f.readJSONFn(ctx, key, out)
}

func (f *fakeAlbumStore) ForEachAlbumIndexKey(ctx context.Context, fn func(key string) error) error {
	if f.forEachAlbumIndexKeyFn == nil {
		panic("unexpected call")
	}
	return f.forEachAlbumIndexKeyFn(ctx, fn)
}

func TestRefreshFromStorageStreamsBeforeListingCompletes(t *testing.T) {
	releaseSecond := make(chan struct{})
	firstRead := make(chan struct{})
	callDone := make(chan error, 1)

	s := &Service{
		store: &fakeAlbumStore{
			forEachAlbumIndexKeyFn: func(ctx context.Context, fn func(key string) error) error {
				if err := fn("albums/album-a/index.json"); err != nil {
					return err
				}
				<-releaseSecond
				if err := fn("albums/album-b/index.json"); err != nil {
					return err
				}
				return nil
			},
			readJSONFn: func(ctx context.Context, key string, out any) error {
				idx := out.(*models.AlbumIndex)
				if strings.Contains(key, "album-a") {
					idx.AlbumID = "album-a"
					close(firstRead)
					return nil
				}
				idx.AlbumID = "album-b"
				return nil
			},
		},
		albumCache: make(map[string]*models.AlbumIndex),
	}

	var mu sync.Mutex
	var firstProgress *RefreshProgress
	go func() {
		callDone <- s.RefreshFromStorageWithProgress(context.Background(), func(progress RefreshProgress) {
			mu.Lock()
			defer mu.Unlock()
			if firstProgress == nil {
				cp := progress
				firstProgress = &cp
			}
		})
	}()

	select {
	case <-firstRead:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first album read")
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := firstProgress
		mu.Unlock()
		if got != nil {
			if got.ListingDone {
				t.Fatalf("expected first progress before listing completes")
			}
			if got.Discovered != 1 || got.Processed != 1 || got.Succeeded != 1 || got.Failed != 0 {
				t.Fatalf("unexpected first progress: %+v", *got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for progress callback")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	close(releaseSecond)
	if err := <-callDone; err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestRefreshFromStorageTracksFailuresAndFallbackAlbumID(t *testing.T) {
	s := &Service{
		store: &fakeAlbumStore{
			forEachAlbumIndexKeyFn: func(ctx context.Context, fn func(key string) error) error {
				for _, key := range []string{
					"albums/good/index.json",
					"albums/read-error/index.json",
					"/index.json",
				} {
					if err := fn(key); err != nil {
						return err
					}
				}
				return nil
			},
			readJSONFn: func(ctx context.Context, key string, out any) error {
				switch key {
				case "albums/good/index.json":
					idx := out.(*models.AlbumIndex)
					idx.AlbumID = ""
					return nil
				case "albums/read-error/index.json":
					return errors.New("boom")
				default:
					idx := out.(*models.AlbumIndex)
					idx.AlbumID = ""
					return nil
				}
			},
		},
		albumCache: make(map[string]*models.AlbumIndex),
	}

	var last RefreshProgress
	if err := s.RefreshFromStorageWithProgress(context.Background(), func(progress RefreshProgress) {
		last = progress
	}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if _, ok := s.albumCache["good"]; !ok {
		t.Fatalf("expected album id fallback from key path to cache album")
	}
	if len(s.albumCache) != 1 {
		t.Fatalf("expected exactly one cached album, got %d", len(s.albumCache))
	}
	if last.Discovered != 3 || last.Processed != 3 || last.Succeeded != 1 || last.Failed != 2 {
		t.Fatalf("unexpected final counters: %+v", last)
	}
}

func TestRefreshFromStorageWithProgressAndAlbumEmitsLoadedAlbums(t *testing.T) {
	s := &Service{
		store: &fakeAlbumStore{
			forEachAlbumIndexKeyFn: func(ctx context.Context, fn func(key string) error) error {
				for _, key := range []string{
					"albums/album-a/index.json",
					"albums/album-b/index.json",
				} {
					if err := fn(key); err != nil {
						return err
					}
				}
				return nil
			},
			readJSONFn: func(ctx context.Context, key string, out any) error {
				idx := out.(*models.AlbumIndex)
				if strings.Contains(key, "album-a") {
					idx.AlbumID = "album-a"
					return nil
				}
				idx.AlbumID = "album-b"
				return nil
			},
		},
		albumCache: make(map[string]*models.AlbumIndex),
	}

	var loaded []string
	var loadedMu sync.Mutex
	if err := s.RefreshFromStorageWithProgressAndAlbum(
		context.Background(),
		nil,
		func(idx models.AlbumIndex) {
			loadedMu.Lock()
			loaded = append(loaded, idx.AlbumID)
			loadedMu.Unlock()
		},
	); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	sort.Strings(loaded)
	if !reflect.DeepEqual(loaded, []string{"album-a", "album-b"}) {
		t.Fatalf("unexpected loaded albums: %v", loaded)
	}
}

func TestWarmupWorkerCount(t *testing.T) {
	if got := warmupWorkerCount(7); got != 7 {
		t.Fatalf("expected configured worker count, got %d", got)
	}
	if got := warmupWorkerCount(100); got != 100 {
		t.Fatalf("expected configured worker count without clamping, got %d", got)
	}
}
