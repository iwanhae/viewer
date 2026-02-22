package dedupe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"viewer/internal/storage"
)

type fakeStore struct {
	sources     []storage.AlbumSourceObject
	prefixKeys  map[string][]string
	objectBytes map[string][]byte
	deleteErr   map[string]error
	deleted     []string
}

func (f *fakeStore) ListAlbumSources(ctx context.Context) ([]storage.AlbumSourceObject, error) {
	out := make([]storage.AlbumSourceObject, len(f.sources))
	copy(out, f.sources)
	return out, nil
}

func (f *fakeStore) ListObjectsByPrefix(ctx context.Context, prefix string) ([]string, error) {
	keys := f.prefixKeys[prefix]
	out := make([]string, len(keys))
	copy(out, keys)
	return out, nil
}

func (f *fakeStore) DeleteObject(ctx context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	if err, ok := f.deleteErr[key]; ok {
		return err
	}
	return nil
}

func (f *fakeStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	body, ok := f.objectBytes[key]
	if !ok {
		return nil, "", errors.New("missing object body")
	}
	return io.NopCloser(bytes.NewReader(body)), "application/octet-stream", nil
}

func TestBuildPlanKeepsNewestAndExpandsDeleteKeys(t *testing.T) {
	t0 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t1.Add(1 * time.Hour)

	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{AlbumID: "album-a", Key: "albums/album-a/source.zip", LastModified: t1, Size: 10, ETag: "\"0123456789abcdef0123456789abcdef\""},
			{AlbumID: "album-b", Key: "albums/album-b/source.zip", LastModified: t2, Size: 10, ETag: "\"0123456789abcdef0123456789abcdef\""},
			{AlbumID: "album-c", Key: "albums/album-c/source.zip", LastModified: t2, Size: 12, ETag: "\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\""},
			{AlbumID: "album-d", Key: "albums/album-d/source.zip", LastModified: t0, Size: 10, ETag: "\"0123456789abcdef0123456789abcdef\""},
		},
		prefixKeys: map[string][]string{
			"albums/album-a/": {"albums/album-a/index.json", "albums/album-a/source.zip"},
			"albums/album-d/": {"albums/album-d/index.json", "albums/album-d/source.zip"},
		},
		objectBytes: map[string][]byte{},
	}

	plan, summary, err := NewPlanner(store, "viewer").BuildPlan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if summary.ScannedSources != 4 || summary.DuplicateGroups != 1 || summary.KeptAlbums != 1 || summary.DeletedAlbums != 2 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("groups=%d want=1", len(plan.Groups))
	}
	if plan.Groups[0].KeepAlbumID != "album-b" {
		t.Fatalf("keep album=%q want=album-b", plan.Groups[0].KeepAlbumID)
	}

	if !reflect.DeepEqual(plan.DeleteAlbums, []string{"album-a", "album-d"}) {
		t.Fatalf("DeleteAlbums=%v", plan.DeleteAlbums)
	}

	wantKeys := []string{
		"albums/album-a/index.json",
		"albums/album-a/source.zip",
		"albums/album-d/index.json",
		"albums/album-d/source.zip",
	}
	if !reflect.DeepEqual(plan.DeleteKeys, wantKeys) {
		t.Fatalf("DeleteKeys=%v want=%v", plan.DeleteKeys, wantKeys)
	}
	if len(plan.SourceSnapshot) != 3 {
		t.Fatalf("SourceSnapshot=%d want=3", len(plan.SourceSnapshot))
	}
	if err := plan.ValidateFingerprint(); err != nil {
		t.Fatalf("ValidateFingerprint: %v", err)
	}
}

func TestBuildPlanUsesSHA256ForMultipartETag(t *testing.T) {
	now := time.Date(2026, 2, 2, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{AlbumID: "album-a", Key: "albums/album-a/source.zip", LastModified: now, Size: 3, ETag: "\"abc-2\""},
			{AlbumID: "album-b", Key: "albums/album-b/source.zip", LastModified: now.Add(time.Minute), Size: 3, ETag: ""},
		},
		prefixKeys: map[string][]string{
			"albums/album-a/": {"albums/album-a/source.zip"},
		},
		objectBytes: map[string][]byte{
			"albums/album-a/source.zip": {1, 2, 3},
			"albums/album-b/source.zip": {1, 2, 3},
		},
	}

	plan, _, err := NewPlanner(store, "viewer").BuildPlan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("groups=%d want=1", len(plan.Groups))
	}
	if !strings.HasPrefix(plan.Groups[0].ContentHash, "sha256:") {
		t.Fatalf("content hash=%q want sha256 prefix", plan.Groups[0].ContentHash)
	}
}

func TestValidateFingerprintDetectsMutation(t *testing.T) {
	plan := Plan{
		Version:        PlanVersion,
		GeneratedAt:    "2026-02-22T00:00:00Z",
		Bucket:         "viewer",
		HashPolicy:     HashPolicy,
		Groups:         []DuplicateGroup{},
		KeepAlbums:     []string{},
		DeleteAlbums:   []string{},
		DeleteKeys:     []string{"albums/a/source.zip"},
		SourceSnapshot: []SourceSnapshot{},
	}
	if err := plan.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := plan.ValidateFingerprint(); err != nil {
		t.Fatalf("ValidateFingerprint: %v", err)
	}

	plan.DeleteKeys = append(plan.DeleteKeys, "albums/b/source.zip")
	if err := plan.ValidateFingerprint(); err == nil {
		t.Fatalf("expected fingerprint mismatch")
	}
}

func TestApplyPlanFailsWhenSnapshotChanges(t *testing.T) {
	now := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)
	plan := Plan{
		Version:      PlanVersion,
		GeneratedAt:  "2026-02-22T00:00:00Z",
		Bucket:       "viewer",
		HashPolicy:   HashPolicy,
		DeleteKeys:   []string{"albums/album-a/source.zip"},
		DeleteAlbums: []string{"album-a"},
		SourceSnapshot: []SourceSnapshot{
			{
				AlbumID:              "album-a",
				Key:                  "albums/album-a/source.zip",
				Size:                 10,
				ETag:                 "0123456789abcdef0123456789abcdef",
				LastModifiedUnixNano: now.UnixNano(),
			},
		},
	}
	if err := plan.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{
				AlbumID:      "album-a",
				Key:          "albums/album-a/source.zip",
				Size:         10,
				ETag:         "\"ffffffffffffffffffffffffffffffff\"",
				LastModified: now,
			},
		},
	}

	_, err := ApplyPlan(context.Background(), store, plan)
	if err == nil || !strings.Contains(err.Error(), "snapshot mismatch") {
		t.Fatalf("ApplyPlan err=%v, want snapshot mismatch", err)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("expected no deletes on snapshot mismatch")
	}
}

func TestApplyPlanReportsPartialDeleteFailures(t *testing.T) {
	now := time.Date(2026, 2, 4, 10, 0, 0, 0, time.UTC)
	plan := Plan{
		Version:      PlanVersion,
		GeneratedAt:  "2026-02-22T00:00:00Z",
		Bucket:       "viewer",
		HashPolicy:   HashPolicy,
		DeleteKeys:   []string{"albums/album-a/source.zip", "albums/album-a/index.json"},
		DeleteAlbums: []string{"album-a"},
		SourceSnapshot: []SourceSnapshot{
			{
				AlbumID:              "album-a",
				Key:                  "albums/album-a/source.zip",
				Size:                 10,
				ETag:                 "0123456789abcdef0123456789abcdef",
				LastModifiedUnixNano: now.UnixNano(),
			},
		},
	}
	if err := plan.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{
				AlbumID:      "album-a",
				Key:          "albums/album-a/source.zip",
				Size:         10,
				ETag:         "\"0123456789abcdef0123456789abcdef\"",
				LastModified: now,
			},
		},
		deleteErr: map[string]error{
			"albums/album-a/index.json": errors.New("boom"),
		},
	}

	result, err := ApplyPlan(context.Background(), store, plan)
	if err == nil {
		t.Fatalf("expected delete failure")
	}
	if result.Attempted != 2 || result.Deleted != 1 || len(result.Failed) != 1 {
		t.Fatalf("unexpected apply result: %+v", result)
	}
	if !reflect.DeepEqual(store.deleted, []string{"albums/album-a/source.zip", "albums/album-a/index.json"}) {
		t.Fatalf("deleted keys order mismatch: %v", store.deleted)
	}
}
