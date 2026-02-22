package feed

import (
	"context"
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

	if _, err := svc.Build(context.Background(), 2, "1"); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if _, err := svc.Build(context.Background(), 2, "1"); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("expected one albums scan within ttl, got %d", source.calls)
	}

	now = now.Add(4 * time.Second)
	if _, err := svc.Build(context.Background(), 2, "1"); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if source.calls != 2 {
		t.Fatalf("expected snapshot refresh after ttl, got %d scans", source.calls)
	}
}

func TestBuildDeterministicForSeed(t *testing.T) {
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

	first, err := svc.Build(context.Background(), 4, "42")
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}
	again, err := svc.Build(context.Background(), 4, "42")
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}
	if !reflect.DeepEqual(first.Items, again.Items) {
		t.Fatalf("expected deterministic items for same seed, got first=%+v again=%+v", first.Items, again.Items)
	}
}

func TestBuildVariesForDifferentSeeds(t *testing.T) {
	photos := make([]models.PhotoMeta, 0, 24)
	for i := 0; i < 24; i++ {
		photos = append(photos, models.PhotoMeta{I: i, W: 100, H: 80, Ratio: 1.25})
	}
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID: "album-a",
				Photos:  photos,
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(300, 0) }

	first, err := svc.Build(context.Background(), 24, "7")
	if err != nil {
		t.Fatalf("build first seed failed: %v", err)
	}
	second, err := svc.Build(context.Background(), 24, "11")
	if err != nil {
		t.Fatalf("build second seed failed: %v", err)
	}
	if reflect.DeepEqual(first.Items, second.Items) {
		t.Fatalf("expected different seeds to produce different ordering, got same items=%+v", first.Items)
	}
}

func TestBuildSkipsAlbumsWithoutPhotos(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID: "empty-album",
				Photos:  []models.PhotoMeta{},
			},
			{
				AlbumID: "album-a",
				Photos: []models.PhotoMeta{
					{I: 0, W: 100, H: 80, Ratio: 1.25},
					{I: 1, W: 101, H: 80, Ratio: 1.26},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(400, 0) }

	resp, err := svc.Build(context.Background(), 20, "123")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != 20 {
		t.Fatalf("items=%d want=20", len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.AlbumID != "album-a" {
			t.Fatalf("unexpected album %q; expected only non-empty album", item.AlbumID)
		}
	}
}

func TestBuildSelectsAlbumBeforePhoto(t *testing.T) {
	bigPhotos := make([]models.PhotoMeta, 0, 100)
	for i := 0; i < 100; i++ {
		bigPhotos = append(bigPhotos, models.PhotoMeta{
			I:     i,
			W:     1000 + i,
			H:     500,
			Ratio: float64(1000+i) / 500.0,
		})
	}
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID: "album-big",
				Photos:  bigPhotos,
			},
			{
				AlbumID: "album-small",
				Photos: []models.PhotoMeta{
					{I: 0, W: 33, H: 22, Ratio: 1.5},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(500, 0) }

	resp, err := svc.Build(context.Background(), 200, "2026")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != 200 {
		t.Fatalf("items=%d want=200", len(resp.Items))
	}

	countByAlbum := map[string]int{}
	for _, item := range resp.Items {
		countByAlbum[item.AlbumID]++
		if item.AlbumID == "album-small" && item.I != 0 {
			t.Fatalf("album-small returned unexpected photo index %d", item.I)
		}
	}
	if countByAlbum["album-small"] < 50 {
		t.Fatalf("album-small selected too rarely for album-first sampling: %d", countByAlbum["album-small"])
	}
	if countByAlbum["album-big"] < 50 {
		t.Fatalf("album-big selected too rarely for album-first sampling: %d", countByAlbum["album-big"])
	}
}
