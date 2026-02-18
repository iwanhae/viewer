package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"viewer/internal/models"
)

type stubAlbumSource struct {
	albums []*models.AlbumIndex
	calls  int
}

func (s *stubAlbumSource) AllAlbums() []*models.AlbumIndex {
	s.calls++
	return s.albums
}

func TestBuildUsesSnapshotWithinTTL(t *testing.T) {
	now := time.Unix(100, 0)
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{{
			AlbumID: "album-a",
			Photos:  []models.PhotoMeta{{I: 0, W: 100, H: 100, Ratio: 1}},
		}},
	}
	svc := &Service{
		albums:      source,
		snapshotTTL: 3 * time.Second,
		now: func() time.Time {
			return now
		},
	}

	if _, err := svc.Build(context.Background(), 2, "", "1"); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if _, err := svc.Build(context.Background(), 2, "", "1"); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("expected one albums scan within ttl, got %d", source.calls)
	}

	now = now.Add(4 * time.Second)
	if _, err := svc.Build(context.Background(), 2, "", "1"); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if source.calls != 2 {
		t.Fatalf("expected snapshot refresh after ttl, got %d scans", source.calls)
	}
}

func TestBuildDeterministicForSeedAndCursor(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID: "album-a",
				Photos: []models.PhotoMeta{
					{I: 0, W: 100, H: 80, Ratio: 1.25},
					{I: 1, W: 100, H: 80, Ratio: 1.25},
				},
			},
			{
				AlbumID: "album-b",
				Photos: []models.PhotoMeta{
					{I: 0, W: 120, H: 90, Ratio: 1.33},
					{I: 1, W: 120, H: 90, Ratio: 1.33},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(200, 0) }

	first, err := svc.Build(context.Background(), 4, "", "42")
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}
	again, err := svc.Build(context.Background(), 4, "", "42")
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}
	if !reflect.DeepEqual(first.Items, again.Items) {
		t.Fatalf("expected deterministic first page items, got first=%+v again=%+v", first.Items, again.Items)
	}

	nextA, err := svc.Build(context.Background(), 4, first.NextCursor, "ignored")
	if err != nil {
		t.Fatalf("cursor build failed: %v", err)
	}
	nextB, err := svc.Build(context.Background(), 4, first.NextCursor, "ignored")
	if err != nil {
		t.Fatalf("cursor build failed: %v", err)
	}
	if !reflect.DeepEqual(nextA.Items, nextB.Items) {
		t.Fatalf("expected deterministic cursor page items, got a=%+v b=%+v", nextA.Items, nextB.Items)
	}
}

func TestBuildSupportsLegacyOffsetCursor(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID: "album-a",
				Photos: []models.PhotoMeta{
					{I: 0, W: 100, H: 80, Ratio: 1.25},
					{I: 1, W: 100, H: 80, Ratio: 1.25},
					{I: 2, W: 100, H: 80, Ratio: 1.25},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(300, 0) }

	legacyCursor := encodeLegacyCursor(t, int64(7), 6)
	legacyResp, err := svc.Build(context.Background(), 3, legacyCursor, "ignored")
	if err != nil {
		t.Fatalf("legacy cursor build failed: %v", err)
	}

	newCursor, err := encodeCursor(&cursorState{Seed: 7, Page: 2})
	if err != nil {
		t.Fatalf("encode cursor failed: %v", err)
	}
	newResp, err := svc.Build(context.Background(), 3, newCursor, "ignored")
	if err != nil {
		t.Fatalf("new cursor build failed: %v", err)
	}

	if !reflect.DeepEqual(legacyResp.Items, newResp.Items) {
		t.Fatalf("expected legacy cursor mapping to page cursor, got legacy=%+v new=%+v", legacyResp.Items, newResp.Items)
	}
}

func TestBuildRejectsInvalidCursor(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{{
			AlbumID: "album-a",
			Photos:  []models.PhotoMeta{{I: 0, W: 100, H: 100, Ratio: 1}},
		}},
	}
	svc := NewService(nil)
	svc.albums = source

	if _, err := svc.Build(context.Background(), 3, "!!!", "42"); err == nil {
		t.Fatalf("expected invalid cursor error")
	}
}

func encodeLegacyCursor(t *testing.T, seed int64, offset int) string {
	t.Helper()
	payload := map[string]any{
		"seed":   seed,
		"offset": offset,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
