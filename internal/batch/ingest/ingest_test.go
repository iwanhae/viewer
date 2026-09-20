package ingest

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

// fakeStore is a hand-rolled Store whose behavior is injected per test through
// function fields. Every call is recorded so tests can assert on the interact-
// ions with the S3 surface without touching a network or filesystem.
type fakeStore struct {
	listFn   func(ctx context.Context, prefix string) ([]storage.BatchObject, error)
	headFn   func(ctx context.Context, key string) (bool, int64, error)
	copyFn   func(ctx context.Context, srcKey, dstKey string) error
	deleteFn func(ctx context.Context, key string) error

	listedPrefixes []string
	headCalls      []string
	copied         []srcDst
	deleted        []string
}

type srcDst struct {
	src string
	dst string
}

func (f *fakeStore) ListBatchObjects(ctx context.Context, prefix string) ([]storage.BatchObject, error) {
	f.listedPrefixes = append(f.listedPrefixes, prefix)
	if f.listFn == nil {
		return nil, nil
	}
	return f.listFn(ctx, prefix)
}

func (f *fakeStore) HeadObject(ctx context.Context, key string) (bool, int64, error) {
	f.headCalls = append(f.headCalls, key)
	if f.headFn == nil {
		return false, 0, nil
	}
	return f.headFn(ctx, key)
}

func (f *fakeStore) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	f.copied = append(f.copied, srcDst{src: srcKey, dst: dstKey})
	if f.copyFn == nil {
		return nil
	}
	return f.copyFn(ctx, srcKey, dstKey)
}

func (f *fakeStore) DeleteObject(ctx context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	if f.deleteFn == nil {
		return nil
	}
	return f.deleteFn(ctx, key)
}

// fakeSink records every staged-upload registration.
type fakeSink struct {
	fn    func(ctx context.Context, albumID, filename string, sizeBytes int64, sourceKey string) error
	calls []registerCall
}

type registerCall struct {
	albumID   string
	filename  string
	sizeBytes int64
	sourceKey string
}

func (s *fakeSink) RegisterStagedUpload(ctx context.Context, albumID, filename string, sizeBytes int64, sourceKey string) error {
	s.calls = append(s.calls, registerCall{
		albumID:   albumID,
		filename:  filename,
		sizeBytes: sizeBytes,
		sourceKey: sourceKey,
	})
	if s.fn == nil {
		return nil
	}
	return s.fn(ctx, albumID, filename, sizeBytes, sourceKey)
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

func TestFilterTopLevelZips(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		objects []storage.BatchObject
		want    []string
	}{
		{
			name:   "keeps only top-level zips with case-insensitive extension",
			prefix: "batch/",
			objects: []storage.BatchObject{
				{Key: "batch/a.zip", Size: 10},
				{Key: "batch/b.ZIP", Size: 10},
				{Key: "batch/c.Zip", Size: 10},
				{Key: "batch/sub/d.zip", Size: 10},
				{Key: "batch/deep/nested/e.zip", Size: 10},
				{Key: "batch/notazip.txt", Size: 10},
				{Key: "batch/noext", Size: 10},
				{Key: "batch/empty.zip", Size: 0},
				{Key: "batch/negative.zip", Size: -5},
			},
			want: []string{"batch/a.zip", "batch/b.ZIP", "batch/c.Zip"},
		},
		{
			name:   "custom prefix bounds the top level",
			prefix: "inbox/",
			objects: []storage.BatchObject{
				{Key: "inbox/x.zip", Size: 1},
				{Key: "inbox/x.zip.bak", Size: 1},
				{Key: "inbox/sub/y.zip", Size: 1},
				{Key: "batch/z.zip", Size: 1},
			},
			want: []string{"inbox/x.zip"},
		},
		{
			name:   "bare prefix key is not a candidate",
			prefix: "batch/",
			objects: []storage.BatchObject{
				{Key: "batch/", Size: 5},
			},
			want: []string{},
		},
		{
			name:    "empty input yields no candidates",
			prefix:  "batch/",
			objects: nil,
			want:    []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterTopLevelZips(tc.objects, tc.prefix)
			keys := make([]string, 0, len(got))
			for _, obj := range got {
				keys = append(keys, obj.Key)
			}
			if !reflect.DeepEqual(keys, tc.want) {
				t.Fatalf("filterTopLevelZips keys = %v, want %v", keys, tc.want)
			}
		})
	}
}

func TestRunStagesNewZip(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/Holiday.zip", Size: 4096, ETag: `"etag-holiday"`}
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Moved: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []string{"batch/"}; !reflect.DeepEqual(store.listedPrefixes, want) {
		t.Fatalf("listed prefixes = %v, want %v", store.listedPrefixes, want)
	}
	if want := []string{dstKey}; !reflect.DeepEqual(store.headCalls, want) {
		t.Fatalf("head calls = %v, want %v", store.headCalls, want)
	}
	if want := []srcDst{{src: obj.Key, dst: dstKey}}; !reflect.DeepEqual(store.copied, want) {
		t.Fatalf("copied = %+v, want %+v", store.copied, want)
	}
	if want := []string{obj.Key}; !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("deleted = %v, want %v (source should be removed after staging)", store.deleted, want)
	}
	if want := []registerCall{{
		albumID:   albumID,
		filename:  "Holiday.zip",
		sizeBytes: obj.Size,
		sourceKey: dstKey,
	}}; !reflect.DeepEqual(sink.calls, want) {
		t.Fatalf("sink calls = %+v, want %+v", sink.calls, want)
	}
}

func TestRunDedupesWhenDestinationExists(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/dup.zip", Size: 2048, ETag: `"etag-dup"`}
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
		headFn: func(_ context.Context, key string) (bool, int64, error) {
			if key != dstKey {
				t.Fatalf("HeadObject key = %q, want %q", key, dstKey)
			}
			return true, obj.Size, nil
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Deduped: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(store.copied) != 0 {
		t.Fatalf("no copy expected on dedupe, got %+v", store.copied)
	}
	if want := []string{obj.Key}; !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("deleted = %v, want %v (duplicate source should still be removed)", store.deleted, want)
	}
	if want := []registerCall{{
		albumID:   albumID,
		filename:  "dup.zip",
		sizeBytes: obj.Size,
		sourceKey: dstKey,
	}}; !reflect.DeepEqual(sink.calls, want) {
		t.Fatalf("sink calls = %+v, want %+v (sink must still be invoked on dedupe)", sink.calls, want)
	}
}

func TestRunHeadObjectError(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/head.zip", Size: 128, ETag: `"etag-head"`}
	headErr := errors.New("head boom")

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
		headFn: func(context.Context, string) (bool, int64, error) {
			return false, 0, headErr
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Errors: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(store.copied) != 0 {
		t.Fatalf("no copy expected when head fails, got %+v", store.copied)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("no delete expected when head fails, got %v", store.deleted)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("sink should not be invoked when head fails, got %+v", sink.calls)
	}
}

func TestRunCopyObjectErrorKeepsOriginal(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/copy.zip", Size: 256, ETag: `"etag-copy"`}
	copyErr := errors.New("copy boom")

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
		copyFn: func(context.Context, string, string) error {
			return copyErr
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Errors: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("source must be kept for retry on copy failure, got deleted=%v", store.deleted)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("sink should not be invoked on copy failure, got %+v", sink.calls)
	}
}

func TestRunSinkErrorKeepsSource(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/sink.zip", Size: 512, ETag: `"etag-sink"`}
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)
	sinkErr := errors.New("register boom")

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
	}
	sink := &fakeSink{
		fn: func(context.Context, string, string, int64, string) error {
			return sinkErr
		},
	}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Moved: 1, Errors: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []srcDst{{src: obj.Key, dst: dstKey}}; !reflect.DeepEqual(store.copied, want) {
		t.Fatalf("copied = %+v, want %+v", store.copied, want)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("source must NOT be deleted when sink registration fails, got %v", store.deleted)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink calls = %+v, want exactly 1", sink.calls)
	}
	if sink.calls[0].sourceKey != dstKey {
		t.Fatalf("sink sourceKey = %q, want %q", sink.calls[0].sourceKey, dstKey)
	}
}

func TestRunDeleteObjectError(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/del.zip", Size: 64, ETag: `"etag-del"`}
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)
	deleteErr := errors.New("delete boom")

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
		deleteFn: func(context.Context, string) error {
			return deleteErr
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Moved: 1, Errors: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []srcDst{{src: obj.Key, dst: dstKey}}; !reflect.DeepEqual(store.copied, want) {
		t.Fatalf("copied = %+v, want %+v", store.copied, want)
	}
	if want := []string{obj.Key}; !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("deleted = %v, want %v (delete attempt should be recorded)", store.deleted, want)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("sink calls = %+v, want exactly 1", sink.calls)
	}
}

func TestRunCustomBatchPrefix(t *testing.T) {
	obj := storage.BatchObject{Key: "inbox/trip.zip", Size: 1024, ETag: `"etag-trip"`}
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{BatchPrefix: "  inbox/  "})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Moved: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []string{"inbox/"}; !reflect.DeepEqual(store.listedPrefixes, want) {
		t.Fatalf("listed prefixes = %v, want %v (prefix should be trimmed)", store.listedPrefixes, want)
	}
	if want := []registerCall{{
		albumID:   albumID,
		filename:  "trip.zip",
		sizeBytes: obj.Size,
		sourceKey: dstKey,
	}}; !reflect.DeepEqual(sink.calls, want) {
		t.Fatalf("sink calls = %+v, want %+v", sink.calls, want)
	}
	if want := []srcDst{{src: obj.Key, dst: dstKey}}; !reflect.DeepEqual(store.copied, want) {
		t.Fatalf("copied = %+v, want %+v", store.copied, want)
	}
}

func TestRunListObjectsError(t *testing.T) {
	listErr := errors.New("list boom")
	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return nil, listErr
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err == nil {
		t.Fatalf("Run returned nil error, want wrapped list error")
	}
	if !errors.Is(err, listErr) {
		t.Fatalf("Run error = %v, want it to wrap %v", err, listErr)
	}
	if summary != (Summary{}) {
		t.Fatalf("summary = %+v, want zero value", summary)
	}
	if len(store.headCalls) != 0 || len(store.copied) != 0 || len(store.deleted) != 0 {
		t.Fatalf("no store mutations expected after list error: head=%v copy=%v delete=%v",
			store.headCalls, store.copied, store.deleted)
	}
}

func TestRunNilSinkStillStages(t *testing.T) {
	obj := storage.BatchObject{Key: "batch/nosink.zip", Size: 32, ETag: `"etag-nosink"`}
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{obj}, nil
		},
	}

	summary, err := Run(context.Background(), store, nil, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 1, Moved: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []srcDst{{src: obj.Key, dst: dstKey}}; !reflect.DeepEqual(store.copied, want) {
		t.Fatalf("copied = %+v, want %+v", store.copied, want)
	}
	if want := []string{obj.Key}; !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("deleted = %v, want %v", store.deleted, want)
	}
}

func TestRunMixedBatchSummary(t *testing.T) {
	newObj := storage.BatchObject{Key: "batch/new.zip", Size: 100, ETag: `"etag-new"`}
	dupObj := storage.BatchObject{Key: "batch/dup.zip", Size: 200, ETag: `"etag-dup"`}
	dupDst := pipeline.SourceKey(albumIDFromContent(dupObj.ETag, dupObj.Size))

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{
				newObj,
				dupObj,
				{Key: "batch/sub/nested.zip", Size: 300, ETag: `"etag-nested"`},
				{Key: "batch/readme.txt", Size: 10},
				{Key: "batch/empty.zip", Size: 0},
			}, nil
		},
		headFn: func(_ context.Context, key string) (bool, int64, error) {
			return key == dupDst, dupObj.Size, nil
		},
	}
	sink := &fakeSink{}

	summary, err := Run(context.Background(), store, sink, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := (Summary{Discovered: 2, Moved: 1, Deduped: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if want := []string{newObj.Key, dupObj.Key}; !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("deleted = %v, want %v", store.deleted, want)
	}
	if len(store.copied) != 1 || store.copied[0].src != newObj.Key {
		t.Fatalf("copied = %+v, want only %s", store.copied, newObj.Key)
	}
	if len(sink.calls) != 2 {
		t.Fatalf("sink calls = %+v, want 2", sink.calls)
	}
	for _, call := range sink.calls {
		if call.sourceKey == "" || call.albumID == "" || call.filename == "" || call.sizeBytes <= 0 {
			t.Fatalf("incomplete sink registration: %+v", call)
		}
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &fakeStore{
		listFn: func(context.Context, string) ([]storage.BatchObject, error) {
			return []storage.BatchObject{{Key: "batch/a.zip", Size: 10, ETag: `"etag-a"`}}, nil
		},
	}
	sink := &fakeSink{}

	summary, err := Run(ctx, store, sink, RunOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if want := (Summary{Discovered: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if len(store.headCalls) != 0 || len(store.copied) != 0 || len(store.deleted) != 0 {
		t.Fatalf("cancelled run must not touch objects: head=%v copy=%v delete=%v",
			store.headCalls, store.copied, store.deleted)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("cancelled run must not register uploads, got %+v", sink.calls)
	}
}
