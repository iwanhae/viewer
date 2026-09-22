package albums

import (
	"context"
	"encoding/json"
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

func (f *fakeEnqueuer) Enqueue(_ context.Context, albumID string) error {
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
	svc := NewService(cat, store, enqueuer)
	return svc, cat
}

func TestCreateUploadRegistersQueuedAlbumAndPresigns(t *testing.T) {
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
	if album.Status != catalog.AlbumStatusQueued {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusQueued)
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
	if _, err := svc.CreateUpload(context.Background(), "a.zip", cfgpkg.MaxUploadBytes+1); err == nil {
		t.Fatalf("expected error for size over the upload limit")
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
	if state.Status != catalog.AlbumStatusQueued {
		t.Fatalf("status=%s want=%s", state.Status, catalog.AlbumStatusQueued)
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

func TestRequestFinalizeIsNoopForProcessingAndReadyAlbums(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, store, enqueuer)

	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusProcessing, catalog.AlbumStatusReady} {
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
		want := status
		if state.Status != want {
			t.Fatalf("status=%s want=%s", state.Status, want)
		}
	}
	if len(enqueuer.enqueued) != 0 {
		t.Fatalf("no re-enqueue expected, got %v", enqueuer.enqueued)
	}
}

// TestRequestFinalizeReenqueuesQueuedAlbumWithStagedZip covers the collapsed
// QUEUED status: a freshly registered album is already QUEUED, so finalize must
// still schedule it instead of treating it as already in flight.
func TestRequestFinalizeReenqueuesQueuedAlbumWithStagedZip(t *testing.T) {
	store := newFakeStore()
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, store, enqueuer)

	if err := cat.CreateAlbum(context.Background(), catalog.Album{
		ID:        "album-queued",
		Status:    catalog.AlbumStatusQueued,
		SourceKey: pipeline.SourceKey("album-queued"),
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	store.objects[pipeline.SourceKey("album-queued")] = 10

	state, err := svc.RequestFinalize(context.Background(), "album-queued")
	if err != nil {
		t.Fatalf("request finalize: %v", err)
	}
	if state.Status != catalog.AlbumStatusQueued {
		t.Fatalf("status=%s want=%s", state.Status, catalog.AlbumStatusQueued)
	}
	if len(enqueuer.enqueued) != 1 || enqueuer.enqueued[0] != "album-queued" {
		t.Fatalf("enqueued=%v", enqueuer.enqueued)
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
	if state.Status != catalog.AlbumStatusQueued {
		t.Fatalf("status=%s want=%s", state.Status, catalog.AlbumStatusQueued)
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

func TestGetFinalizeStatusPassesCatalogStatusThrough(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	statuses := []catalog.AlbumStatus{
		catalog.AlbumStatusQueued,
		catalog.AlbumStatusProcessing,
		catalog.AlbumStatusReady,
		catalog.AlbumStatusFailed,
	}
	for _, status := range statuses {
		albumID := "album-" + string(status)
		if err := cat.CreateAlbum(context.Background(), catalog.Album{ID: albumID, Status: status}); err != nil {
			t.Fatalf("create album: %v", err)
		}
		state, err := svc.GetFinalizeStatus(context.Background(), albumID)
		if err != nil {
			t.Fatalf("get status: %v", err)
		}
		if state.Status != status {
			t.Fatalf("status=%s want=%s", state.Status, status)
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
	want := []models.PhotoMeta{{I: 0, Name: "a.png", Hash: "hash-a", W: 4, H: 2, Ratio: 2}}
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
		{id: "pending", status: catalog.AlbumStatusQueued},
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

func TestSearchAlbumsByNameReturnsItems(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "album-a", OriginalFilename: "Holiday Trip.zip", Status: catalog.AlbumStatusReady,
		CreatedAt: "2026-02-17T10:00:00Z", PhotoCount: 2, SizeBytes: 2048,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := cat.InsertPhoto(ctx, catalog.Photo{
		AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-a", Width: 4, Height: 2, Ratio: 2,
	}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	got, err := svc.SearchAlbumsByName(ctx, "  HoLiDaY ", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].AlbumID != "album-a" || got[0].PhotoCount != 2 {
		t.Fatalf("unexpected search results: %+v", got)
	}
	if got[0].SizeBytes != 2048 {
		t.Fatalf("size bytes=%d want=2048", got[0].SizeBytes)
	}
	if got[0].Cover == nil || got[0].Cover.I != 0 || got[0].Cover.W != 4 || got[0].Cover.H != 2 || got[0].Cover.Ratio != 2 {
		t.Fatalf("unexpected cover: %+v", got[0].Cover)
	}
	encoded, err := json.Marshal(got[0].Cover)
	if err != nil {
		t.Fatalf("marshal cover: %v", err)
	}
	if string(encoded) != `{"i":0,"hash":"hash-a","w":4,"h":2,"ratio":2}` {
		t.Fatalf("cover json=%s", encoded)
	}
}

// TestAlbumForStagedObjectMatchesRecordedAndImplicitKeys covers the lookup the
// upload scan uses to tell an object it already knows from one to adopt: the
// recorded source_key wins, and a row written before that column was populated
// is still recognised through the key its upload used.
func TestAlbumForStagedObjectMatchesRecordedAndImplicitKeys(t *testing.T) {
	svc, cat := newTestService(t, newFakeStore(), nil)
	ctx := context.Background()

	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "recorded", Status: catalog.AlbumStatusReady, SourceKey: "uploads/custom-name.zip",
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "implicit", Status: catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	cases := []struct {
		name    string
		key     string
		wantID  string
		wantHit bool
	}{
		{name: "recorded key", key: "uploads/custom-name.zip", wantID: "recorded", wantHit: true},
		{name: "implicit key", key: pipeline.SourceKey("implicit"), wantID: "implicit", wantHit: true},
		{name: "unknown album", key: pipeline.SourceKey("nobody"), wantHit: false},
		{name: "unrelated key", key: "blobs/deadbeef", wantHit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			album, found, err := svc.AlbumForStagedObject(ctx, tc.key)
			if err != nil {
				t.Fatalf("AlbumForStagedObject: %v", err)
			}
			if found != tc.wantHit {
				t.Fatalf("found=%v want=%v for key %q", found, tc.wantHit, tc.key)
			}
			if found && album.ID != tc.wantID {
				t.Fatalf("album=%s want=%s", album.ID, tc.wantID)
			}
		})
	}
}

// TestEnqueueStagedSchedulesWithoutTouchingStatus pins the recovery the scan
// relies on: an album whose zip is still in the bucket is queued again, and its
// status is left to the pipeline, so a scan cannot reset an album that is being
// extracted right now back to QUEUED.
func TestEnqueueStagedSchedulesWithoutTouchingStatus(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, newFakeStore(), enqueuer)
	ctx := context.Background()

	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID: "stuck", Status: catalog.AlbumStatusProcessing, SourceKey: pipeline.SourceKey("stuck"),
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	if err := svc.EnqueueStaged(ctx, "stuck"); err != nil {
		t.Fatalf("enqueue staged: %v", err)
	}
	if want := []string{"stuck"}; !reflect.DeepEqual(enqueuer.enqueued, want) {
		t.Fatalf("enqueued=%v want=%v", enqueuer.enqueued, want)
	}
	album, err := cat.GetAlbum(ctx, "stuck")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != catalog.AlbumStatusProcessing {
		t.Fatalf("status=%s want=%s (the pipeline owns the status)", album.Status, catalog.AlbumStatusProcessing)
	}
}

func TestEnqueueStagedWithoutEnqueuerFails(t *testing.T) {
	svc, _ := newTestService(t, newFakeStore(), nil)
	if err := svc.EnqueueStaged(context.Background(), "album-a"); err == nil {
		t.Fatalf("expected error when no enqueuer is configured")
	}
}

func TestRegisterStagedUploadQueuesAlbum(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	svc, cat := newTestService(t, newFakeStore(), enqueuer)
	ctx := context.Background()

	if err := svc.RegisterStagedUpload(ctx, "album-a", "trip.zip", 128, pipeline.SourceKey("album-a")); err != nil {
		t.Fatalf("register staged upload: %v", err)
	}
	album, err := cat.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != catalog.AlbumStatusQueued || album.OriginalFilename != "trip.zip" || album.SizeBytes != 128 {
		t.Fatalf("unexpected album: %+v", album)
	}
	if len(enqueuer.enqueued) != 1 || enqueuer.enqueued[0] != "album-a" {
		t.Fatalf("enqueued=%v", enqueuer.enqueued)
	}

	if err := svc.RegisterStagedUpload(ctx, "  ", "trip.zip", 1, ""); err == nil {
		t.Fatalf("expected error for empty album id")
	}
}

func TestServiceWithNilCatalogDegradesGracefully(t *testing.T) {
	svc := NewService(nil, newFakeStore(), nil)
	if svc.AllAlbums() != nil {
		t.Fatalf("expected nil albums for nil catalog")
	}
	if _, err := svc.GetAlbum(context.Background(), "album-a"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
	if _, err := svc.SearchAlbumsByName(context.Background(), "", 10); err != nil {
		t.Fatalf("expected empty search, got %v", err)
	}
	if _, err := svc.GetFinalizeStatus(context.Background(), "album-a"); !errors.Is(err, ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
	if _, err := svc.CreateUpload(context.Background(), "a.zip", 1); err == nil {
		t.Fatalf("expected error for nil catalog")
	}
}
