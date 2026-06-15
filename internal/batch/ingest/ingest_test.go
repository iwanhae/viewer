package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"reflect"
	"strings"
	"testing"

	"viewer/internal/albums"
	"viewer/internal/storage"
)

type fakeStore struct {
	batchObjects []storage.BatchObject
	existingKeys map[string]bool
	objectBytes  map[string][]byte
	copyErr      map[string]error
	deleteErr    map[string]error
	rangeErr     map[string]error

	copied   []srcDst
	deleted  []string
	headHits map[string]int
}

type srcDst struct{ src, dst string }

func (f *fakeStore) ListBatchObjects(ctx context.Context, prefix string) ([]storage.BatchObject, error) {
	out := make([]storage.BatchObject, len(f.batchObjects))
	copy(out, f.batchObjects)
	return out, nil
}

func (f *fakeStore) HeadObject(ctx context.Context, key string) (bool, int64, error) {
	if f.headHits == nil {
		f.headHits = make(map[string]int)
	}
	f.headHits[key]++
	if f.existingKeys == nil {
		f.existingKeys = make(map[string]bool)
	}
	exists := f.existingKeys[key]
	var size int64
	if exists {
		if b, ok := f.objectBytes[key]; ok {
			size = int64(len(b))
		} else {
			size = 1
		}
	}
	return exists, size, nil
}

func (f *fakeStore) GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error) {
	if err, ok := f.rangeErr[key]; ok {
		return nil, "", err
	}
	body, ok := f.objectBytes[key]
	if !ok {
		return nil, "", errors.New("missing object body")
	}
	if start < 0 || end < start || end >= int64(len(body)) {
		return nil, "", fmt.Errorf("invalid range %d-%d for %s", start, end, key)
	}
	return io.NopCloser(bytes.NewReader(body[start : end+1])), "application/octet-stream", nil
}

func (f *fakeStore) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	if err, ok := f.copyErr[srcKey]; ok {
		return err
	}
	f.copied = append(f.copied, srcDst{src: srcKey, dst: dstKey})
	if f.existingKeys == nil {
		f.existingKeys = make(map[string]bool)
	}
	f.existingKeys[dstKey] = true
	if b, ok := f.objectBytes[srcKey]; ok {
		if f.objectBytes == nil {
			f.objectBytes = make(map[string][]byte)
		}
		f.objectBytes[dstKey] = append([]byte(nil), b...)
	}
	return nil
}

func (f *fakeStore) DeleteObject(ctx context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	if err, ok := f.deleteErr[key]; ok {
		return err
	}
	if f.existingKeys != nil {
		delete(f.existingKeys, key)
	}
	if f.objectBytes != nil {
		delete(f.objectBytes, key)
	}
	return nil
}

func validPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func zipWithFile(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, err := w.Create(name)
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestAlbumIDFromContentStable(t *testing.T) {
	a := albumIDFromContent("\"abc\"", 100)
	b := albumIDFromContent("abc", 100)
	if a != b {
		t.Fatalf("etag quotes should be stripped: %q vs %q", a, b)
	}
	if len(a) != albumIDHashLen {
		t.Fatalf("album id length = %d, want %d", len(a), albumIDHashLen)
	}
	c := albumIDFromContent("abc", 101)
	if a == c {
		t.Fatalf("different size must yield different album id")
	}
	d := albumIDFromContent("abc-5", 100)
	if a == d {
		t.Fatalf("multipart-style etag must not collide with simple etag when derived differently")
	}
}

func TestFilterTopLevelZipsSkipsNestedAndNonZip(t *testing.T) {
	objs := []storage.BatchObject{
		{Key: "batch/a.zip", Size: 10},
		{Key: "batch/b.ZIP", Size: 10},
		{Key: "batch/sub/c.zip", Size: 10},
		{Key: "batch/notazip.txt", Size: 10},
		{Key: "batch/empty.zip", Size: 0},
	}
	got := filterTopLevelZips(objs, "batch/")
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d (%+v)", len(got), got)
	}
	for _, o := range got {
		if o.Key == "batch/sub/c.zip" {
			t.Fatalf("nested zip should be filtered out")
		}
	}
}

func TestRunMovesNewValidZip(t *testing.T) {
	zipBytes := zipWithFile(t, "photo.png", validPNGBytes(t))
	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "batch/Holiday.zip", Size: int64(len(zipBytes)), ETag: "\"etag-holiday\""},
		},
		existingKeys: map[string]bool{},
		objectBytes:  map[string][]byte{"batch/Holiday.zip": zipBytes},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	expectedAlbumID := albumIDFromContent("\"etag-holiday\"", int64(len(zipBytes)))
	expectedDst := "albums/" + expectedAlbumID + "/source.zip"

	if summary.Discovered != 1 {
		t.Fatalf("Discovered = %d, want 1", summary.Discovered)
	}
	if summary.Moved != 1 {
		t.Fatalf("Moved = %d, want 1", summary.Moved)
	}
	if summary.Deduped != 0 || summary.DeletedFailed != 0 || summary.Errors != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	wantCopy := []srcDst{{src: "batch/Holiday.zip", dst: expectedDst}}
	if !reflect.DeepEqual(store.copied, wantCopy) {
		t.Fatalf("copied = %+v, want %+v", store.copied, wantCopy)
	}
	if !reflect.DeepEqual(store.deleted, []string{"batch/Holiday.zip"}) {
		t.Fatalf("deleted = %v, want [batch/Holiday.zip]", store.deleted)
	}
	if !store.existingKeys[expectedDst] {
		t.Fatalf("expected destination %s to exist after move", expectedDst)
	}
}

func TestRunDedupesWhenAlbumAlreadyExists(t *testing.T) {
	zipBytes := zipWithFile(t, "photo.png", validPNGBytes(t))
	albumID := albumIDFromContent("\"etag-dup\"", int64(len(zipBytes)))
	dstKey := "albums/" + albumID + "/source.zip"

	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "batch/dup.zip", Size: int64(len(zipBytes)), ETag: "\"etag-dup\""},
		},
		existingKeys: map[string]bool{dstKey: true},
		objectBytes:  map[string][]byte{"batch/dup.zip": zipBytes},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.Deduped != 1 {
		t.Fatalf("Deduped = %d, want 1", summary.Deduped)
	}
	if summary.Moved != 0 {
		t.Fatalf("Moved should be 0 on dedup, got %d", summary.Moved)
	}
	if len(store.copied) != 0 {
		t.Fatalf("no copy expected on dedup, got %+v", store.copied)
	}
	if !reflect.DeepEqual(store.deleted, []string{"batch/dup.zip"}) {
		t.Fatalf("deleted = %v, want [batch/dup.zip]", store.deleted)
	}
}

func TestRunDeletesCorruptZip(t *testing.T) {
	garbage := []byte("this is not a zip file")
	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "batch/broken.zip", Size: int64(len(garbage)), ETag: "\"etag-broken\""},
		},
		existingKeys: map[string]bool{},
		objectBytes:  map[string][]byte{"batch/broken.zip": garbage},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.DeletedFailed != 1 {
		t.Fatalf("DeletedFailed = %d, want 1", summary.DeletedFailed)
	}
	if summary.Moved != 0 || summary.Errors != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if !reflect.DeepEqual(store.deleted, []string{"batch/broken.zip"}) {
		t.Fatalf("deleted = %v, want [batch/broken.zip]", store.deleted)
	}
	if len(store.copied) != 0 {
		t.Fatalf("no copy expected for corrupt zip, got %+v", store.copied)
	}
}

func TestRunDeletesZipWithNoValidImages(t *testing.T) {
	zipBytes := zipWithFile(t, "notes.txt", []byte("hello"))
	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "batch/noimgs.zip", Size: int64(len(zipBytes)), ETag: "\"etag-noimgs\""},
		},
		existingKeys: map[string]bool{},
		objectBytes:  map[string][]byte{"batch/noimgs.zip": zipBytes},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.DeletedFailed != 1 {
		t.Fatalf("DeletedFailed = %d, want 1", summary.DeletedFailed)
	}
	if summary.Moved != 0 {
		t.Fatalf("Moved should be 0, got %d", summary.Moved)
	}
	if !reflect.DeepEqual(store.deleted, []string{"batch/noimgs.zip"}) {
		t.Fatalf("deleted = %v, want [batch/noimgs.zip]", store.deleted)
	}
}

func TestRunCopyFailureKeepsOriginalAndCountsError(t *testing.T) {
	zipBytes := zipWithFile(t, "photo.png", validPNGBytes(t))
	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "batch/fail.zip", Size: int64(len(zipBytes)), ETag: "\"etag-fail\""},
		},
		existingKeys: map[string]bool{},
		objectBytes:  map[string][]byte{"batch/fail.zip": zipBytes},
		copyErr:      map[string]error{"batch/fail.zip": errors.New("s3 copy failed")},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{})
	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if summary.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", summary.Errors)
	}
	if summary.Moved != 0 {
		t.Fatalf("Moved should be 0 on copy failure, got %d", summary.Moved)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("original should NOT be deleted on copy failure, got %v", store.deleted)
	}
	if _, ok := store.objectBytes["batch/fail.zip"]; !ok {
		t.Fatalf("original batch/fail.zip bytes should still be present after copy failure")
	}
}

func TestRunIgnoresNestedZip(t *testing.T) {
	zipBytes := zipWithFile(t, "photo.png", validPNGBytes(t))
	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "batch/sub/nested.zip", Size: int64(len(zipBytes)), ETag: "\"etag-nested\""},
			{Key: "batch/top.zip", Size: int64(len(zipBytes)), ETag: "\"etag-top\""},
		},
		existingKeys: map[string]bool{},
		objectBytes: map[string][]byte{
			"batch/sub/nested.zip": zipBytes,
			"batch/top.zip":        zipBytes,
		},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.Discovered != 1 {
		t.Fatalf("Discovered = %d, want 1 (nested should be ignored)", summary.Discovered)
	}
	if summary.Moved != 1 {
		t.Fatalf("Moved = %d, want 1", summary.Moved)
	}
	for _, c := range store.copied {
		if strings.HasPrefix(c.src, "batch/sub/") {
			t.Fatalf("nested zip should not be copied: %+v", c)
		}
	}
}

func TestRunCustomBatchPrefix(t *testing.T) {
	zipBytes := zipWithFile(t, "photo.png", validPNGBytes(t))
	store := &fakeStore{
		batchObjects: []storage.BatchObject{
			{Key: "inbox/x.zip", Size: int64(len(zipBytes)), ETag: "\"etag-x\""},
		},
		existingKeys: map[string]bool{},
		objectBytes:  map[string][]byte{"inbox/x.zip": zipBytes},
	}

	summary, err := Run(context.Background(), store, albums.NewIndexer(), RunOptions{BatchPrefix: "inbox/"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Discovered != 1 || summary.Moved != 1 {
		t.Fatalf("summary = %+v, want Discovered=1 Moved=1", summary)
	}
}
