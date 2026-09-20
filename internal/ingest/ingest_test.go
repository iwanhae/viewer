package ingest

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

// fakeStore serves a fixed listing and records the prefixes it was asked for.
type fakeStore struct {
	objects []storage.Object
	listErr error

	listedPrefixes []string
}

func (f *fakeStore) ListObjects(_ context.Context, prefix string) ([]storage.Object, error) {
	f.listedPrefixes = append(f.listedPrefixes, prefix)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objects, nil
}

// fakeSink records every catalog interaction and returns a canned answer for
// the lookup.
type fakeSink struct {
	album  *catalog.Album
	found  bool
	lookup error

	registered []registerCall
	enqueued   []string
	enqueueErr error
}

type registerCall struct {
	albumID   string
	filename  string
	sizeBytes int64
	sourceKey string
}

func (s *fakeSink) AlbumForStagedObject(context.Context, string) (*catalog.Album, bool, error) {
	if s.lookup != nil {
		return nil, false, s.lookup
	}
	return s.album, s.found, nil
}

func (s *fakeSink) RegisterStagedUpload(_ context.Context, albumID, filename string, sizeBytes int64, sourceKey string) error {
	s.registered = append(s.registered, registerCall{
		albumID: albumID, filename: filename, sizeBytes: sizeBytes, sourceKey: sourceKey,
	})
	return nil
}

func (s *fakeSink) EnqueueStaged(_ context.Context, albumID string) error {
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	s.enqueued = append(s.enqueued, albumID)
	return nil
}

func TestAlbumIDFromContentDeterministicAndFormatted(t *testing.T) {
	base := albumIDFromContent(`"etag-1"`, 123)

	if got := albumIDFromContent(`"etag-1"`, 123); got != base {
		t.Fatalf("album id is not deterministic: %q vs %q", base, got)
	}
	if got := albumIDFromContent("etag-1", 123); got != base {
		t.Fatalf("surrounding etag quotes must be stripped: %q vs %q", got, base)
	}
	if len(base) != albumIDHashLen {
		t.Fatalf("album id length = %d, want %d", len(base), albumIDHashLen)
	}
	if _, err := hex.DecodeString(base); err != nil {
		t.Fatalf("album id %q is not valid hex: %v", base, err)
	}
	if base != strings.ToLower(base) {
		t.Fatalf("album id %q should be lowercase hex", base)
	}
	if got := albumIDFromContent(`"etag-1"`, 124); got == base {
		t.Fatalf("different size must yield a different album id (both %q)", got)
	}
	if got := albumIDFromContent(`"etag-2"`, 123); got == base {
		t.Fatalf("different etag must yield a different album id (both %q)", got)
	}
}

func TestStagedZipsKeepsZipsWithContent(t *testing.T) {
	objects := []storage.Object{
		{Key: "uploads/a.zip", Size: 10},
		{Key: "uploads/b.ZIP", Size: 10},
		{Key: "uploads/c.Zip", Size: 10},
		{Key: "uploads/nested/d.zip", Size: 10},
		{Key: "uploads/notazip.txt", Size: 10},
		{Key: "uploads/noext", Size: 10},
		{Key: "uploads/empty.zip", Size: 0},
		{Key: "uploads/negative.zip", Size: -5},
	}

	got := stagedZips(objects)
	keys := make([]string, 0, len(got))
	for _, obj := range got {
		keys = append(keys, obj.Key)
	}
	want := []string{"uploads/a.zip", "uploads/b.ZIP", "uploads/c.Zip", "uploads/nested/d.zip"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("stagedZips keys = %v, want %v", keys, want)
	}
}

// TestRunAdoptsUnknownZip is the feature: a zip nobody owns becomes an album,
// keeps its own key, and is neither copied nor deleted.
func TestRunAdoptsUnknownZip(t *testing.T) {
	obj := storage.Object{Key: "uploads/Holiday.zip", Size: 4096, ETag: `"etag-holiday"`}
	store := &fakeStore{objects: []storage.Object{obj}}
	sink := &fakeSink{found: false}

	summary, err := Run(context.Background(), store, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Registered: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []string{pipeline.UploadPrefix}; !reflect.DeepEqual(store.listedPrefixes, want) {
		t.Fatalf("listed prefixes = %v, want %v", store.listedPrefixes, want)
	}
	if want := []registerCall{{
		albumID:   albumIDFromContent(obj.ETag, obj.Size),
		filename:  "Holiday.zip",
		sizeBytes: obj.Size,
		sourceKey: obj.Key,
	}}; !reflect.DeepEqual(sink.registered, want) {
		t.Fatalf("registered = %+v, want %+v", sink.registered, want)
	}
	if len(sink.enqueued) != 0 {
		t.Fatalf("registration queues the album itself, got enqueued=%v", sink.enqueued)
	}
}

// TestRunAdoptsNestedZipUsesItsBaseName keeps a nested drop usable as a filing
// convention instead of an error: the album is named after the file.
func TestRunAdoptsNestedZipUsesItsBaseName(t *testing.T) {
	obj := storage.Object{Key: "uploads/2026/June/trip.zip", Size: 10, ETag: `"etag-trip"`}
	sink := &fakeSink{found: false}

	if _, err := Run(context.Background(), &fakeStore{objects: []storage.Object{obj}}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := "trip.zip"; len(sink.registered) != 1 || sink.registered[0].filename != want {
		t.Fatalf("registered = %+v, want filename %q", sink.registered, want)
	}
	if sink.registered[0].sourceKey != obj.Key {
		t.Fatalf("sourceKey = %q want %q (the object stays where it is)", sink.registered[0].sourceKey, obj.Key)
	}
}

// TestRunRequeuesWaitingAlbums covers the startup recovery the scan took over
// from the separate pending-upload pass: an album the catalog says is still
// waiting is queued again, because the zip being present means no worker holds
// it.
func TestRunRequeuesWaitingAlbums(t *testing.T) {
	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusQueued, catalog.AlbumStatusProcessing} {
		t.Run(string(status), func(t *testing.T) {
			obj := storage.Object{Key: pipeline.SourceKey("album-a"), Size: 10, ETag: `"etag-a"`}
			sink := &fakeSink{found: true, album: &catalog.Album{ID: "album-a", Status: status}}

			summary, err := Run(context.Background(), &fakeStore{objects: []storage.Object{obj}}, sink)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if want := (Summary{Discovered: 1, Requeued: 1}); summary != want {
				t.Fatalf("summary = %+v, want %+v", summary, want)
			}
			if want := []string{"album-a"}; !reflect.DeepEqual(sink.enqueued, want) {
				t.Fatalf("enqueued = %v, want %v", sink.enqueued, want)
			}
			if len(sink.registered) != 0 {
				t.Fatalf("an owned object must not be registered again, got %+v", sink.registered)
			}
		})
	}
}

// TestRunLeavesFinishedAlbumsAlone keeps a leftover zip of a SUCCEEDED album
// (whose delete failed) and a FAILED album that is waiting for a deliberate
// retry out of the queue.
func TestRunLeavesFinishedAlbumsAlone(t *testing.T) {
	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusReady, catalog.AlbumStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			obj := storage.Object{Key: pipeline.SourceKey("album-a"), Size: 10, ETag: `"etag-a"`}
			sink := &fakeSink{found: true, album: &catalog.Album{ID: "album-a", Status: status}}

			summary, err := Run(context.Background(), &fakeStore{objects: []storage.Object{obj}}, sink)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if want := (Summary{Discovered: 1, Skipped: 1}); summary != want {
				t.Fatalf("summary = %+v, want %+v", summary, want)
			}
			if len(sink.enqueued) != 0 || len(sink.registered) != 0 {
				t.Fatalf("nothing should be queued: enqueued=%v registered=%+v", sink.enqueued, sink.registered)
			}
		})
	}
}

// TestRunMixedSummary pins the shape of one scan over a realistic prefix.
func TestRunMixedSummary(t *testing.T) {
	adopted := storage.Object{Key: "uploads/new.zip", Size: 100, ETag: `"etag-new"`}
	waiting := storage.Object{Key: pipeline.SourceKey("waiting"), Size: 200, ETag: `"etag-waiting"`}
	store := &fakeStore{objects: []storage.Object{
		adopted,
		waiting,
		{Key: pipeline.SourceKey("done"), Size: 300, ETag: `"etag-done"`},
		{Key: "uploads/readme.txt", Size: 10},
		{Key: "uploads/empty.zip", Size: 0},
	}}
	sink := &lookupSink{statuses: map[string]catalog.AlbumStatus{
		"waiting": catalog.AlbumStatusQueued,
		"done":    catalog.AlbumStatusReady,
	}}

	summary, err := Run(context.Background(), store, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 3, Registered: 1, Requeued: 1, Skipped: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []string{"waiting"}; !reflect.DeepEqual(sink.enqueued, want) {
		t.Fatalf("enqueued = %v, want %v", sink.enqueued, want)
	}
	if len(sink.registered) != 1 || sink.registered[0].sourceKey != adopted.Key {
		t.Fatalf("registered = %+v, want only %s", sink.registered, adopted.Key)
	}
}

// lookupSink answers the album lookup from a map of album id to status, with
// every staged key resolved to the album id SourceKey implies.
type lookupSink struct {
	statuses   map[string]catalog.AlbumStatus
	enqueued   []string
	registered []registerCall
}

func (s *lookupSink) AlbumForStagedObject(_ context.Context, sourceKey string) (*catalog.Album, bool, error) {
	id, ok := pipeline.StagedAlbumID(sourceKey)
	if !ok {
		return nil, false, nil
	}
	status, ok := s.statuses[id]
	if !ok {
		return nil, false, nil
	}
	return &catalog.Album{ID: id, Status: status}, true, nil
}

func (s *lookupSink) RegisterStagedUpload(_ context.Context, albumID, filename string, sizeBytes int64, sourceKey string) error {
	s.registered = append(s.registered, registerCall{
		albumID: albumID, filename: filename, sizeBytes: sizeBytes, sourceKey: sourceKey,
	})
	return nil
}

func (s *lookupSink) EnqueueStaged(_ context.Context, albumID string) error {
	s.enqueued = append(s.enqueued, albumID)
	return nil
}

func TestRunLookupErrorIsCountedAndSkipped(t *testing.T) {
	lookupErr := errors.New("lookup boom")
	store := &fakeStore{objects: []storage.Object{
		{Key: "uploads/broken.zip", Size: 10, ETag: `"etag-broken"`},
		{Key: "uploads/good.zip", Size: 10, ETag: `"etag-good"`},
	}}
	sink := &fakeSink{lookup: lookupErr}

	summary, err := Run(context.Background(), store, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 2, Errors: 2}); summary != want {
		t.Fatalf("summary = %+v, want %+v (one failure must not abort the scan)", summary, want)
	}
	if len(sink.registered) != 0 {
		t.Fatalf("registered = %+v, want none", sink.registered)
	}
}

func TestRunEnqueueErrorIsCounted(t *testing.T) {
	obj := storage.Object{Key: pipeline.SourceKey("album-a"), Size: 10, ETag: `"etag-a"`}
	sink := &fakeSink{
		found:      true,
		album:      &catalog.Album{ID: "album-a", Status: catalog.AlbumStatusQueued},
		enqueueErr: errors.New("queue boom"),
	}

	summary, err := Run(context.Background(), &fakeStore{objects: []storage.Object{obj}}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Errors: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
}

func TestRunListObjectsError(t *testing.T) {
	listErr := errors.New("list boom")
	store := &fakeStore{listErr: listErr}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink)
	if err == nil {
		t.Fatalf("Run returned nil error, want wrapped list error")
	}
	if !errors.Is(err, listErr) {
		t.Fatalf("Run error = %v, want it to wrap %v", err, listErr)
	}
	if summary != (Summary{}) {
		t.Fatalf("summary = %+v, want zero value", summary)
	}
	if len(sink.registered) != 0 || len(sink.enqueued) != 0 {
		t.Fatalf("nothing should be touched after a list error: %+v %v", sink.registered, sink.enqueued)
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &fakeStore{objects: []storage.Object{{Key: "uploads/a.zip", Size: 10, ETag: `"etag-a"`}}}
	sink := &fakeSink{}

	summary, err := Run(ctx, store, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if want := (Summary{Discovered: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(sink.registered) != 0 || len(sink.enqueued) != 0 {
		t.Fatalf("cancelled run must not touch anything: %+v %v", sink.registered, sink.enqueued)
	}
}
