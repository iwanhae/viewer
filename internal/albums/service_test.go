package albums

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"viewer/internal/catalog"
	cfgpkg "viewer/internal/config"
	"viewer/internal/models"
	"viewer/internal/pipeline"
)

type fakeStore struct {
	objects      map[string]int64
	presignCalls []string
	presignTTL   time.Duration
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string]int64)}
}

func (f *fakeStore) PresignPut(_ context.Context, key string, ttl time.Duration) (string, map[string]string, error) {
	f.presignCalls = append(f.presignCalls, key)
	f.presignTTL = ttl
	return "https://s3.example/" + key, map[string]string{"x-amz-acl": "private"}, nil
}

func (f *fakeStore) HeadObject(_ context.Context, key string) (bool, int64, error) {
	size, ok := f.objects[key]
	if !ok {
		return false, 0, nil
	}
	return true, size, nil
}

type fakeEnqueuer struct {
	enqueued []string
	err      error
}

func (f *fakeEnqueuer) Enqueue(albumID string) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, albumID)
	return nil
}

func openTestCatalog(t *testing.T) *catalog.Store {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func newTestService(t *testing.T, store *fakeStore, enqueuer Enqueuer) (*Service, *catalog.Store) {
	t.Helper()
	cat := openTestCatalog(t)
	cfg := cfgpkg.Config{
		PresignTTL:     15 * time.Minute,
		MaxUploadBytes: 1024,
	}
	svc := NewService(cfg, cat, store)
	if enqueuer != nil {
		svc.SetEnqueuer(enqueuer)
	}
	return svc, cat
}

func TestCreateUploadRegistersPendingAlbumAndPresigns(t *testing.T) {
	store := newFakeStore()
	svc, cat := newTestService(t, store, nil)

	result, err := svc.CreateUpload(context.Background(), "holiday.zip", 512)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	if result.AlbumID == "" {
		t.Fatalf("expected an album id")
	}
	if result.Key != pipeline.SourceKey(result.AlbumID) {
		t.Fatalf("key=%q want=%q", result.Key, pipeline.SourceKey(result.AlbumID))
	}
	if result.UploadURL == "" {
		t.Fatalf("expected a presigned url")
	}
	if len(store.presignCalls) != 1 || store.presignCalls[0] != result.Key {
		t.Fatalf("unexpected presign calls: %v", store.presignCalls)
	}

	album, err := cat.GetAlbum(context.Background(), result.AlbumID)
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != catalog.AlbumStatusPending {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusPending)
	}
	if album.OriginalFilename != "holiday.zip" || album.SizeBytes != 512 {
		t.Fatalf("unexpected album: %+v", album)
	}
	if album.SourceKey != result.Key {
		t.Fatalf("source key=%q want=%q", album.SourceKey, result.Key)
	}
}

func TestCreateUploadValidation(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store, nil)

	if _, err := svc.CreateUpload(context.Background(), "   ", 10); err == nil {
		t.Fatalf("expected error for empty filename")
	}
	if _, err := svc.CreateUpload(context.Background(), "a.zip", 0); err == nil {
		t.Fatalf("expected error for non-positive size")
	}
	if _, err := svc.CreateUpload(context.Background(), "a.zip", 2048); err == nil {
		t.Fatalf("expected error for size over MAX_UPLOAD_BYTES")
	}
	if len(store.presignCalls) != 0 {
		t.Fatalf("no presign should happen on validation failure")
	}
}

func TestRequestFinalizeQueuesUploadedAlbum(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, store, enqueuer)

	result, err := svc.CreateUpload(context.Background(), "holiday.zip", 100)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	store.objects[result.Key] = 100

	state, err := svc.RequestFinalize(context.Background(), result.AlbumID)
	if err != nil {
		t.Fatalf("request finalize: %v", err)
	}
	if state.Status != FinalizeStatusQueued {
		t.Fatalf("status=%s want=%s", state.Status, FinalizeStatusQueued)
	}
	if len(enqueuer.enqueued) != 1 || enqueuer.enqueued[0] != result.AlbumID {
		t.Fatalf("enqueued=%v", enqueuer.enqueued)
	}

	album, _ := cat.GetAlbum(context.Background(), result.AlbumID)
	if album.Status != catalog.AlbumStatusQueued {
		t.Fatalf("catalog status=%s want=%s", album.Status, catalog.AlbumStatusQueued)
	}
}

func TestRequestFinalizeRejectsMissingSource(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store, &fakeEnqueuer{})

	result, err := svc.CreateUpload(context.Background(), "holiday.zip", 100)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	if _, err := svc.RequestFinalize(context.Background(), result.AlbumID); !errors.Is(err, ErrAlbumSourceNotFound) {
		t.Fatalf("expected ErrAlbumSourceNotFound, got %v", err)
	}
}

func TestRequestFinalizeUnknownAlbum(t *testing.T) {
	svc, _ := newTestService(t, newFakeStore(), &fakeEnqueuer{})
	if _, err := svc.RequestFinalize(context.Background(), "missing"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
}

func TestRequestFinalizeIsNoopForQueuedAndReadyAlbums(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, store, enqueuer)

	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusQueued, catalog.AlbumStatusProcessing, catalog.AlbumStatusReady} {
		albumID := "album-" + string(status)
		if err := cat.CreateAlbum(context.Background(), catalog.Album{
			ID:        albumID,
			Status:    status,
			SourceKey: pipeline.SourceKey(albumID),
		}); err != nil {
			t.Fatalf("create album: %v", err)
		}

		state, err := svc.RequestFinalize(context.Background(), albumID)
		if err != nil {
			t.Fatalf("request finalize %s: %v", status, err)
		}
		want := finalizeStatusFromCatalog(status)
		if state.Status != want {
			t.Fatalf("status=%s want=%s", state.Status, want)
		}
	}
	if len(enqueuer.enqueued) != 0 {
		t.Fatalf("no re-enqueue expected, got %v", enqueuer.enqueued)
	}
}

func TestRequestFinalizeRetriesFailedAlbum(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, store, enqueuer)

	if err := cat.CreateAlbum(context.Background(), catalog.Album{
		ID:        "album-a",
		Status:    catalog.AlbumStatusFailed,
		Error:     "boom",
		SourceKey: pipeline.SourceKey("album-a"),
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	store.objects[pipeline.SourceKey("album-a")] = 10

	state, err := svc.RequestFinalize(context.Background(), "album-a")
	if err != nil {
		t.Fatalf("request finalize: %v", err)
	}
	if state.Status != FinalizeStatusQueued {
		t.Fatalf("status=%s want=%s", state.Status, FinalizeStatusQueued)
	}
	if len(enqueuer.enqueued) != 1 {
		t.Fatalf("expected re-enqueue, got %v", enqueuer.enqueued)
	}
}

func TestRequestFinalizeMarksFailedWhenQueueRejects(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{err: errors.New("queue is full")}
	svc, cat := newTestService(t, store, enqueuer)

	result, err := svc.CreateUpload(context.Background(), "holiday.zip", 100)
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	store.objects[result.Key] = 100

	if _, err := svc.RequestFinalize(context.Background(), result.AlbumID); err == nil {
		t.Fatalf("expected enqueue error")
	}
	album, _ := cat.GetAlbum(context.Background(), result.AlbumID)
	if album.Status != catalog.AlbumStatusFailed {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusFailed)
	}
}

func TestGetFinalizeStatusMapsCatalogStatuses(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	cases := []struct {
		status catalog.AlbumStatus
		want   FinalizeStatus
	}{
		{catalog.AlbumStatusPending, FinalizeStatusQueued},
		{catalog.AlbumStatusQueued, FinalizeStatusQueued},
		{catalog.AlbumStatusProcessing, FinalizeStatusProcessing},
		{catalog.AlbumStatusReady, FinalizeStatusSucceeded},
		{catalog.AlbumStatusFailed, FinalizeStatusFailed},
	}
	for _, tc := range cases {
		albumID := "album-" + string(tc.status)
		if err := cat.CreateAlbum(context.Background(), catalog.Album{ID: albumID, Status: tc.status}); err != nil {
			t.Fatalf("create album: %v", err)
		}
		state, err := svc.GetFinalizeStatus(context.Background(), albumID)
		if err != nil {
			t.Fatalf("get status: %v", err)
		}
		if state.Status != tc.want {
			t.Fatalf("status=%s want=%s", state.Status, tc.want)
		}
	}

	if _, err := svc.GetFinalizeStatus(context.Background(), "missing"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
}

func TestGetAlbumReturnsPhotosInLegacyShape(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               "album-a",
		OriginalFilename: "holiday.zip",
		Status:           catalog.AlbumStatusReady,
		CreatedAt:        "2026-02-17T10:00:00Z",
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := cat.InsertPhoto(ctx, catalog.Photo{
		AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-a", Width: 4, Height: 2, Ratio: 2,
	}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	idx, err := svc.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if idx.AlbumID != "album-a" || idx.OriginalFilename != "holiday.zip" || idx.PhotoCount != 1 {
		t.Fatalf("unexpected album index: %+v", idx)
	}
	want := []models.PhotoMeta{{I: 0, Name: "a.png", W: 4, H: 2, Ratio: 2}}
	if !reflect.DeepEqual(idx.Photos, want) {
		t.Fatalf("photos=%+v want=%+v", idx.Photos, want)
	}

	if _, err := svc.GetAlbum(ctx, "missing"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
}

func TestAllAlbumsOnlyReturnsReadyAlbums(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	ctx := context.Background()

	seed := []struct {
		id     string
		status catalog.AlbumStatus
	}{
		{id: "ready-b", status: catalog.AlbumStatusReady},
		{id: "ready-a", status: catalog.AlbumStatusReady},
		{id: "pending", status: catalog.AlbumStatusPending},
	}
	for _, item := range seed {
		if err := cat.CreateAlbum(ctx, catalog.Album{
			ID:               item.id,
			OriginalFilename: item.id + ".zip",
			Status:           item.status,
			CreatedAt:        "2026-02-17T10:00:00Z",
		}); err != nil {
			t.Fatalf("create album: %v", err)
		}
		if err := cat.InsertPhoto(ctx, catalog.Photo{
			AlbumID: item.id, Index: 0, Name: "a.png", Hash: "hash-" + item.id, Width: 1, Height: 1, Ratio: 1,
		}); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}

	albumsList := svc.AllAlbums()
	got := make([]string, 0, len(albumsList))
	for _, album := range albumsList {
		got = append(got, album.AlbumID)
	}
	if !reflect.DeepEqual(got, []string{"ready-a", "ready-b"}) {
		t.Fatalf("albums=%v want=[ready-a ready-b]", got)
	}
}

func TestSearchAlbumsByNamePrefixReturnsItems(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "album-a", OriginalFilename: "Holiday Trip.zip", Status: catalog.AlbumStatusReady,
		CreatedAt: "2026-02-17T10:00:00Z", PhotoCount: 2,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	got, err := svc.SearchAlbumsByNamePrefix(ctx, "  HoLiDaY ", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].AlbumID != "album-a" || got[0].PhotoCount != 2 {
		t.Fatalf("unexpected search results: %+v", got)
	}
}

func TestEnqueuePendingOnlyQueuesAlbumsWithStagedZip(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, store, enqueuer)
	ctx := context.Background()

	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "with-zip", Status: catalog.AlbumStatusPending, SourceKey: pipeline.SourceKey("with-zip"),
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "without-zip", Status: catalog.AlbumStatusPending, SourceKey: pipeline.SourceKey("without-zip"),
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	store.objects[pipeline.SourceKey("with-zip")] = 42

	enqueued, err := svc.EnqueuePending(ctx)
	if err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	if enqueued != 1 || len(enqueuer.enqueued) != 1 || enqueuer.enqueued[0] != "with-zip" {
		t.Fatalf("enqueued=%d list=%v", enqueued, enqueuer.enqueued)
	}
	album, _ := cat.GetAlbum(ctx, "with-zip")
	if album.Status != catalog.AlbumStatusQueued {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusQueued)
	}
}

func TestEnqueuePendingWithoutEnqueuerFails(t *testing.T) {
	svc, _ := newTestService(t, newFakeStore(), nil)
	if _, err := svc.EnqueuePending(context.Background()); err == nil {
		t.Fatalf("expected error when no enqueuer is configured")
	}
}

func TestRegisterStagedUploadQueuesAlbum(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, newFakeStore(), enqueuer)
	ctx := context.Background()

	if err := svc.RegisterStagedUpload(ctx, "album-a", "batch.zip", 128, pipeline.SourceKey("album-a")); err != nil {
		t.Fatalf("register staged upload: %v", err)
	}
	album, err := cat.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != catalog.AlbumStatusQueued || album.OriginalFilename != "batch.zip" || album.SizeBytes != 128 {
		t.Fatalf("unexpected album: %+v", album)
	}
	if len(enqueuer.enqueued) != 1 || enqueuer.enqueued[0] != "album-a" {
		t.Fatalf("enqueued=%v", enqueuer.enqueued)
	}

	if err := svc.RegisterStagedUpload(ctx, "  ", "batch.zip", 1, ""); err == nil {
		t.Fatalf("expected error for empty album id")
	}
}

func TestServiceWithNilCatalogDegradesGracefully(t *testing.T) {
	svc := NewService(cfgpkg.Config{}, nil, newFakeStore())
	if svc.AllAlbums() != nil {
		t.Fatalf("expected nil albums for nil catalog")
	}
	if _, err := svc.GetAlbum(context.Background(), "album-a"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
	if _, err := svc.SearchAlbumsByNamePrefix(context.Background(), "", 10); err != nil {
		t.Fatalf("expected empty search, got %v", err)
	}
	if _, err := svc.GetFinalizeStatus(context.Background(), "album-a"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
	if _, err := svc.CreateUpload(context.Background(), "a.zip", 1); err == nil {
		t.Fatalf("expected error for nil catalog")
	}
}
