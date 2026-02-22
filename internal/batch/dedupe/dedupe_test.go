package dedupe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"viewer/internal/storage"
)

type fakeStore struct {
	sources        []storage.AlbumSourceObject
	prefixKeys     map[string][]string
	objectBytes    map[string][]byte
	deleteErr      map[string]error
	rangeErr       map[string]error
	deleted        []string
	getObjectCalls map[string]int
	getRangeCalls  map[string]int
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
	if f.getObjectCalls == nil {
		f.getObjectCalls = make(map[string]int)
	}
	f.getObjectCalls[key]++

	body, ok := f.objectBytes[key]
	if !ok {
		return nil, "", errors.New("missing object body")
	}
	return io.NopCloser(bytes.NewReader(body)), "application/octet-stream", nil
}

func (f *fakeStore) GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error) {
	if f.getRangeCalls == nil {
		f.getRangeCalls = make(map[string]int)
	}
	f.getRangeCalls[key]++
	f.getRangeCalls[rangeCallKey(key, start, end)]++

	if err, ok := f.rangeErr[key]; ok {
		return nil, "", err
	}
	if err, ok := f.rangeErr[rangeCallKey(key, start, end)]; ok {
		return nil, "", err
	}

	body, ok := f.objectBytes[key]
	if !ok {
		return nil, "", errors.New("missing object body")
	}
	if start < 0 || end < start || end >= int64(len(body)) {
		return nil, "", fmt.Errorf("invalid range %d-%d for %s", start, end, key)
	}

	window := body[start : end+1]
	return io.NopCloser(bytes.NewReader(window)), "application/octet-stream", nil
}

func rangeCallKey(key string, start int64, end int64) string {
	return fmt.Sprintf("%s:%d-%d", key, start, end)
}

func TestBuildPlanKeepsNewestAndExpandsDeleteKeys(t *testing.T) {
	t0 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t1.Add(1 * time.Hour)

	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{AlbumID: "album-a", Key: "albums/album-a/source.zip", LastModified: t1, Size: 10, ETag: "\"abc-2\""},
			{AlbumID: "album-b", Key: "albums/album-b/source.zip", LastModified: t2, Size: 10, ETag: "\"00000000000000000000000000000000\""},
			{AlbumID: "album-c", Key: "albums/album-c/source.zip", LastModified: t2, Size: 12, ETag: "\"ffffffffffffffffffffffffffffffff\""},
			{AlbumID: "album-d", Key: "albums/album-d/source.zip", LastModified: t0, Size: 10, ETag: "\"n/a\""},
		},
		prefixKeys: map[string][]string{
			"albums/album-a/": {"albums/album-a/index.json", "albums/album-a/source.zip"},
			"albums/album-d/": {"albums/album-d/index.json", "albums/album-d/source.zip"},
		},
		objectBytes: map[string][]byte{
			"albums/album-a/source.zip": []byte("abcdefghij"),
			"albums/album-b/source.zip": []byte("abcdefghij"),
			"albums/album-c/source.zip": []byte("unique-bytes!"),
			"albums/album-d/source.zip": []byte("abcdefghij"),
		},
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
	if !strings.HasPrefix(plan.Groups[0].ContentHash, "sha256:") {
		t.Fatalf("content hash=%q want sha256 prefix", plan.Groups[0].ContentHash)
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
	if got := store.getObjectCalls["albums/album-c/source.zip"]; got != 0 {
		t.Fatalf("album-c should not require full hash, got calls=%d", got)
	}
	if err := plan.ValidateFingerprint(); err != nil {
		t.Fatalf("ValidateFingerprint: %v", err)
	}
}

func TestBuildPlanIgnoresUnreliableETag(t *testing.T) {
	now := time.Date(2026, 2, 2, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{AlbumID: "album-a", Key: "albums/album-a/source.zip", LastModified: now, Size: 3, ETag: "\"abc-2\""},
			{AlbumID: "album-b", Key: "albums/album-b/source.zip", LastModified: now.Add(time.Minute), Size: 3, ETag: "\"ffffffffffffffffffffffffffffffff\""},
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

func TestBuildPlanSampleCollisionRequiresFullSHA256(t *testing.T) {
	now := time.Date(2026, 2, 2, 11, 0, 0, 0, time.UTC)
	size := int(sampleWindowBytes * 5)

	bodyA := bytes.Repeat([]byte{'a'}, size)
	bodyB := bytes.Repeat([]byte{'a'}, size)

	ranges := sampleRanges(int64(size))
	diffIndex := firstUnsampledByteIndex(int64(size), ranges)
	if diffIndex < 0 {
		t.Fatalf("expected at least one unsampled byte")
	}
	bodyB[diffIndex] = 'b'

	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{AlbumID: "album-a", Key: "albums/album-a/source.zip", LastModified: now, Size: int64(size)},
			{AlbumID: "album-b", Key: "albums/album-b/source.zip", LastModified: now.Add(time.Minute), Size: int64(size)},
		},
		objectBytes: map[string][]byte{
			"albums/album-a/source.zip": bodyA,
			"albums/album-b/source.zip": bodyB,
		},
	}

	planner := NewPlanner(store, "viewer")
	sampleA, err := planner.resolveSampleHash(context.Background(), store.sources[0])
	if err != nil {
		t.Fatalf("resolveSampleHash(album-a): %v", err)
	}
	sampleB, err := planner.resolveSampleHash(context.Background(), store.sources[1])
	if err != nil {
		t.Fatalf("resolveSampleHash(album-b): %v", err)
	}
	if sampleA != sampleB {
		t.Fatalf("expected sample collision, got %q vs %q", sampleA, sampleB)
	}

	plan, summary, err := planner.BuildPlan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if summary.DuplicateGroups != 0 || len(plan.Groups) != 0 {
		t.Fatalf("unexpected duplicates from sample collision: summary=%+v groups=%d", summary, len(plan.Groups))
	}
	if store.getObjectCalls["albums/album-a/source.zip"] == 0 || store.getObjectCalls["albums/album-b/source.zip"] == 0 {
		t.Fatalf("expected full hash calls for both sources, calls=%v", store.getObjectCalls)
	}
}

func TestBuildPlanFallsBackToFullSHA256WhenRangeReadsFail(t *testing.T) {
	now := time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		sources: []storage.AlbumSourceObject{
			{AlbumID: "album-a", Key: "albums/album-a/source.zip", LastModified: now, Size: 4},
			{AlbumID: "album-b", Key: "albums/album-b/source.zip", LastModified: now.Add(time.Minute), Size: 4},
		},
		prefixKeys: map[string][]string{
			"albums/album-a/": {"albums/album-a/source.zip"},
		},
		objectBytes: map[string][]byte{
			"albums/album-a/source.zip": {1, 2, 3, 4},
			"albums/album-b/source.zip": {1, 2, 3, 4},
		},
		rangeErr: map[string]error{
			"albums/album-a/source.zip": errors.New("range unavailable"),
		},
	}

	plan, summary, err := NewPlanner(store, "viewer").BuildPlan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if summary.DuplicateGroups != 1 || len(plan.Groups) != 1 {
		t.Fatalf("unexpected summary/groups: summary=%+v groups=%d", summary, len(plan.Groups))
	}
	if store.getObjectCalls["albums/album-a/source.zip"] == 0 || store.getObjectCalls["albums/album-b/source.zip"] == 0 {
		t.Fatalf("expected fallback full hashes for bucket, calls=%v", store.getObjectCalls)
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

func TestValidatePlanRejectsLegacyVersionAndPolicy(t *testing.T) {
	legacyVersion := Plan{
		Version:    PlanVersion - 1,
		Bucket:     "viewer",
		HashPolicy: HashPolicy,
	}
	if err := ValidatePlan(legacyVersion); err == nil || !strings.Contains(err.Error(), "unsupported plan version") {
		t.Fatalf("ValidatePlan legacy version err=%v", err)
	}

	legacyPolicy := Plan{
		Version:    PlanVersion,
		Bucket:     "viewer",
		HashPolicy: "etag_or_sha256",
	}
	if err := ValidatePlan(legacyPolicy); err == nil || !strings.Contains(err.Error(), "unsupported hash policy") {
		t.Fatalf("ValidatePlan legacy policy err=%v", err)
	}
}

func TestApplyPlanFailsWhenSnapshotSizeChanges(t *testing.T) {
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
				AlbumID: "album-a",
				Key:     "albums/album-a/source.zip",
				Size:    10,
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
				Size:         11,
				ETag:         "\"changed\"",
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

func TestApplyPlanAllowsSameSizeSourceChange(t *testing.T) {
	now := time.Date(2026, 2, 3, 11, 0, 0, 0, time.UTC)
	plan := Plan{
		Version:      PlanVersion,
		GeneratedAt:  "2026-02-22T00:00:00Z",
		Bucket:       "viewer",
		HashPolicy:   HashPolicy,
		DeleteKeys:   []string{"albums/album-a/source.zip"},
		DeleteAlbums: []string{"album-a"},
		SourceSnapshot: []SourceSnapshot{
			{
				AlbumID: "album-a",
				Key:     "albums/album-a/source.zip",
				Size:    10,
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
				ETag:         "\"changed\"",
				LastModified: now.Add(5 * time.Minute),
			},
		},
	}

	result, err := ApplyPlan(context.Background(), store, plan)
	if err != nil {
		t.Fatalf("ApplyPlan err=%v, want nil", err)
	}
	if result.Attempted != 1 || result.Deleted != 1 || len(result.Failed) != 0 {
		t.Fatalf("unexpected apply result: %+v", result)
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
				AlbumID: "album-a",
				Key:     "albums/album-a/source.zip",
				Size:    10,
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

func firstUnsampledByteIndex(size int64, ranges []byteRange) int {
	for i := int64(0); i < size; i++ {
		sampled := false
		for _, r := range ranges {
			if i >= r.start && i <= r.end {
				sampled = true
				break
			}
		}
		if !sampled {
			return int(i)
		}
	}
	return -1
}
