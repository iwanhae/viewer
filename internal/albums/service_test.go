package albums

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
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

func TestSearchAlbumsByNamePrefixMatchesCaseInsensitiveAndOrdersByCreatedAt(t *testing.T) {
	s := &Service{
		albumCache: map[string]*models.AlbumIndex{
			"album-new": {
				AlbumID:          "album-new",
				OriginalFilename: "Holiday Trip.zip",
				CreatedAt:        "2026-02-17T10:00:00Z",
				PhotoCount:       2,
			},
			"album-old": {
				AlbumID:          "album-old",
				OriginalFilename: "holiday family.zip",
				CreatedAt:        "2026-02-16T10:00:00Z",
				PhotoCount:       1,
			},
			"album-other": {
				AlbumID:          "album-other",
				OriginalFilename: "Weekend.zip",
				CreatedAt:        "2026-02-18T10:00:00Z",
				PhotoCount:       1,
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

type fakeAlbumStore struct {
	forEachAlbumObjectKeyFn func(ctx context.Context, fn func(key string) error) error
	forEachAlbumIndexKeyFn  func(ctx context.Context, fn func(key string) error) error
	readJSONFn              func(ctx context.Context, key string, out any) error
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

func (f *fakeAlbumStore) ForEachAlbumObjectKey(ctx context.Context, fn func(key string) error) error {
	if f.forEachAlbumObjectKeyFn == nil {
		panic("unexpected call")
	}
	return f.forEachAlbumObjectKeyFn(ctx, fn)
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

	summary, err := s.RefreshFromStorage(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if _, ok := s.albumCache["good"]; !ok {
		t.Fatalf("expected album id fallback from key path to cache album")
	}
	if len(s.albumCache) != 1 {
		t.Fatalf("expected exactly one cached album, got %d", len(s.albumCache))
	}
	if summary.Discovered != 3 || summary.Loaded != 1 || summary.Failed != 2 {
		t.Fatalf("unexpected refresh summary: %+v", summary)
	}
}

func TestRefreshFromStorageEmitsLoadedAlbums(t *testing.T) {
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
	summary, err := s.RefreshFromStorage(context.Background(), func(idx models.AlbumIndex) {
		loaded = append(loaded, idx.AlbumID)
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	sort.Strings(loaded)
	if !reflect.DeepEqual(loaded, []string{"album-a", "album-b"}) {
		t.Fatalf("unexpected loaded albums: %v", loaded)
	}
	if summary.Discovered != 2 || summary.Loaded != 2 || summary.Failed != 0 {
		t.Fatalf("unexpected refresh summary: %+v", summary)
	}
}

func TestRefreshFromStorageReturnsListingError(t *testing.T) {
	s := &Service{
		store: &fakeAlbumStore{
			forEachAlbumIndexKeyFn: func(ctx context.Context, fn func(key string) error) error {
				if err := fn("albums/album-a/index.json"); err != nil {
					return err
				}
				return errors.New("listing failed")
			},
			readJSONFn: func(ctx context.Context, key string, out any) error {
				idx := out.(*models.AlbumIndex)
				idx.AlbumID = "album-a"
				return nil
			},
		},
		albumCache: make(map[string]*models.AlbumIndex),
	}

	summary, err := s.RefreshFromStorage(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected listing error")
	}
	if summary.Discovered != 1 || summary.Loaded != 1 || summary.Failed != 0 {
		t.Fatalf("unexpected partial summary before listing error: %+v", summary)
	}
}

func TestQueuePendingFinalizationsQueuesPendingAndSkipsTracked(t *testing.T) {
	s := &Service{
		store: &fakeAlbumStore{
			forEachAlbumObjectKeyFn: func(ctx context.Context, fn func(key string) error) error {
				for _, key := range []string{
					"albums/a-new/source.zip",
					"albums/a-failed/source.zip",
					"albums/a-queued/source.zip",
					"albums/a-processing/source.zip",
					"albums/a-succeeded/source.zip",
					"albums/z-indexed/source.zip",
					"albums/z-indexed/index.json",
					"albums/invalid",
				} {
					if err := fn(key); err != nil {
						return err
					}
				}
				return nil
			},
		},
		finalizeJobs: map[string]*FinalizeState{
			"a-failed": {
				AlbumID:   "a-failed",
				Status:    FinalizeStatusFailed,
				Error:     "previous error",
				UpdatedAt: "2026-02-19T00:00:00Z",
			},
			"a-queued": {
				AlbumID:   "a-queued",
				Status:    FinalizeStatusQueued,
				UpdatedAt: "2026-02-19T00:00:00Z",
			},
			"a-processing": {
				AlbumID:   "a-processing",
				Status:    FinalizeStatusProcessing,
				UpdatedAt: "2026-02-19T00:00:00Z",
			},
			"a-succeeded": {
				AlbumID:   "a-succeeded",
				Status:    FinalizeStatusSucceeded,
				UpdatedAt: "2026-02-19T00:00:00Z",
			},
		},
		finalizeQueue: make(chan string, 10),
	}

	summary, err := s.QueuePendingFinalizations(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if summary.ObjectsDiscovered != 8 {
		t.Fatalf("objects discovered=%d want=8", summary.ObjectsDiscovered)
	}
	if summary.SourceObjects != 6 {
		t.Fatalf("source objects=%d want=6", summary.SourceObjects)
	}
	if summary.IndexObjects != 1 {
		t.Fatalf("index objects=%d want=1", summary.IndexObjects)
	}
	if summary.PendingCandidates != 5 {
		t.Fatalf("pending candidates=%d want=5", summary.PendingCandidates)
	}
	if summary.Enqueued != 2 {
		t.Fatalf("enqueued=%d want=2", summary.Enqueued)
	}
	if summary.AlreadyTracked != 3 {
		t.Fatalf("already tracked=%d want=3", summary.AlreadyTracked)
	}
	if summary.EnqueueFailed != 0 {
		t.Fatalf("enqueue failed=%d want=0", summary.EnqueueFailed)
	}

	queued := make([]string, 0, 2)
	for len(queued) < 2 {
		select {
		case albumID := <-s.finalizeQueue:
			queued = append(queued, albumID)
		default:
			t.Fatalf("expected 2 queued albums, got %v", queued)
		}
	}
	if queued[0] != "a-failed" || queued[1] != "a-new" {
		t.Fatalf("unexpected queue order: %v", queued)
	}

	if state := s.finalizeJobs["a-failed"]; state == nil || state.Status != FinalizeStatusQueued || state.Error != "" {
		t.Fatalf("failed album should be reset and queued, got %+v", state)
	}
	if state := s.finalizeJobs["a-new"]; state == nil || state.Status != FinalizeStatusQueued {
		t.Fatalf("new album should be queued, got %+v", state)
	}
}

func TestQueuePendingFinalizationsMarksQueueFullAsFailed(t *testing.T) {
	s := &Service{
		store: &fakeAlbumStore{
			forEachAlbumObjectKeyFn: func(ctx context.Context, fn func(key string) error) error {
				return fn("albums/full/source.zip")
			},
		},
		finalizeJobs:  make(map[string]*FinalizeState),
		finalizeQueue: make(chan string),
	}

	summary, err := s.QueuePendingFinalizations(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if summary.PendingCandidates != 1 {
		t.Fatalf("pending candidates=%d want=1", summary.PendingCandidates)
	}
	if summary.Enqueued != 0 {
		t.Fatalf("enqueued=%d want=0", summary.Enqueued)
	}
	if summary.EnqueueFailed != 1 {
		t.Fatalf("enqueue failed=%d want=1", summary.EnqueueFailed)
	}

	state := s.finalizeJobs["full"]
	if state == nil {
		t.Fatalf("expected failed state to be recorded")
	}
	if state.Status != FinalizeStatusFailed {
		t.Fatalf("status=%s want=%s", state.Status, FinalizeStatusFailed)
	}
	if state.Error != "finalize queue is full" {
		t.Fatalf("error=%q want=finalize queue is full", state.Error)
	}
}
