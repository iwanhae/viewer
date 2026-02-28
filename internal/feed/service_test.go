package feed

import (
	"context"
	"fmt"
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

	if _, err := svc.Build(context.Background(), 2, "1", ModeRandom); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if _, err := svc.Build(context.Background(), 2, "1", ModeRandom); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("expected one albums scan within ttl, got %d", source.calls)
	}

	now = now.Add(4 * time.Second)
	if _, err := svc.Build(context.Background(), 2, "1", ModeRandom); err != nil {
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

	first, err := svc.Build(context.Background(), 4, "42", ModeRandom)
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}
	again, err := svc.Build(context.Background(), 4, "42", ModeRandom)
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

	first, err := svc.Build(context.Background(), 24, "7", ModeRandom)
	if err != nil {
		t.Fatalf("build first seed failed: %v", err)
	}
	second, err := svc.Build(context.Background(), 24, "11", ModeRandom)
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

	resp, err := svc.Build(context.Background(), 20, "123", ModeRandom)
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

	resp, err := svc.Build(context.Background(), 200, "2026", ModeRandom)
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

func TestBuildReservesRecentAlbumQuota(t *testing.T) {
	albumsList, recentSet := buildAlbumsWithSequentialCreatedAt(120)
	source := &stubAlbumSource{albums: albumsList}

	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(600, 0) }

	const limit = 25
	resp, err := svc.Build(context.Background(), limit, "777", ModeRandom)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != limit {
		t.Fatalf("items=%d want=%d", len(resp.Items), limit)
	}

	recentCount := 0
	for _, item := range resp.Items {
		if _, ok := recentSet[item.AlbumID]; ok {
			recentCount++
		}
	}

	if recentCount != 5 {
		t.Fatalf("recentCount=%d want=5", recentCount)
	}
}

func TestBuildSpreadsRecentAlbumsAcrossFeed(t *testing.T) {
	albumsList, recentSet := buildAlbumsWithSequentialCreatedAt(120)
	source := &stubAlbumSource{albums: albumsList}

	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(700, 0) }

	const limit = 25
	resp, err := svc.Build(context.Background(), limit, "999", ModeRandom)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != limit {
		t.Fatalf("items=%d want=%d", len(resp.Items), limit)
	}

	recentPositions := make([]int, 0, 5)
	for i, item := range resp.Items {
		if _, ok := recentSet[item.AlbumID]; ok {
			recentPositions = append(recentPositions, i)
		}
	}

	want := []int{4, 9, 14, 19, 24}
	if !reflect.DeepEqual(recentPositions, want) {
		t.Fatalf("recent positions mismatch, got=%v want=%v", recentPositions, want)
	}
}

func TestBuildTreatsMalformedCreatedAtAsOldest(t *testing.T) {
	validAlbums, _ := buildAlbumsWithSequentialCreatedAt(recentAlbumsWindow)
	source := &stubAlbumSource{
		albums: append(validAlbums, &models.AlbumIndex{
			AlbumID:   "album-invalid",
			CreatedAt: "not-a-timestamp",
			Photos: []models.PhotoMeta{
				{I: 0, W: 100, H: 80, Ratio: 1.25},
			},
		}),
	}

	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(800, 0) }

	const limit = 25
	resp, err := svc.Build(context.Background(), limit, "12345", ModeRandom)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != limit {
		t.Fatalf("items=%d want=%d", len(resp.Items), limit)
	}

	invalidCount := 0
	for _, item := range resp.Items {
		if item.AlbumID == "album-invalid" {
			invalidCount++
		}
	}

	if invalidCount != 20 {
		t.Fatalf("invalid album count=%d want=20", invalidCount)
	}
}

func TestBuildLatestUsesDescendingCreatedAtAndFirstPhoto(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID:   "album-b",
				CreatedAt: "2026-01-02T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 22, W: 100, H: 80, Ratio: 1.25},
					{I: 23, W: 101, H: 80, Ratio: 1.26},
				},
			},
			{
				AlbumID:   "album-a",
				CreatedAt: "2026-01-03T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 11, W: 120, H: 90, Ratio: 1.33},
					{I: 12, W: 121, H: 90, Ratio: 1.34},
				},
			},
			{
				AlbumID:   "album-c",
				CreatedAt: "2026-01-01T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 33, W: 140, H: 100, Ratio: 1.4},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(900, 0) }

	resp, err := svc.Build(context.Background(), 10, "ignored", ModeLatest)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items=%d want=3", len(resp.Items))
	}

	gotAlbumIDs := []string{resp.Items[0].AlbumID, resp.Items[1].AlbumID, resp.Items[2].AlbumID}
	wantAlbumIDs := []string{"album-a", "album-b", "album-c"}
	if !reflect.DeepEqual(gotAlbumIDs, wantAlbumIDs) {
		t.Fatalf("album order=%v want=%v", gotAlbumIDs, wantAlbumIDs)
	}

	gotIndexes := []int{resp.Items[0].I, resp.Items[1].I, resp.Items[2].I}
	wantIndexes := []int{11, 22, 33}
	if !reflect.DeepEqual(gotIndexes, wantIndexes) {
		t.Fatalf("photo indexes=%v want=%v", gotIndexes, wantIndexes)
	}
}

func TestBuildLatestBreaksCreatedAtTiesByAlbumID(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID:   "album-b",
				CreatedAt: "2026-02-01T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 2, W: 100, H: 80, Ratio: 1.25},
				},
			},
			{
				AlbumID:   "album-a",
				CreatedAt: "2026-02-01T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 1, W: 100, H: 80, Ratio: 1.25},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(901, 0) }

	resp, err := svc.Build(context.Background(), 2, "ignored", ModeLatest)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d want=2", len(resp.Items))
	}
	if resp.Items[0].AlbumID != "album-a" || resp.Items[1].AlbumID != "album-b" {
		t.Fatalf("unexpected order: %+v", resp.Items)
	}
}

func TestBuildLatestTreatsMalformedCreatedAtAsOldest(t *testing.T) {
	source := &stubAlbumSource{
		albums: []*models.AlbumIndex{
			{
				AlbumID:   "album-new",
				CreatedAt: "2026-02-03T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 5, W: 100, H: 80, Ratio: 1.25},
				},
			},
			{
				AlbumID:   "album-invalid",
				CreatedAt: "invalid",
				Photos: []models.PhotoMeta{
					{I: 8, W: 100, H: 80, Ratio: 1.25},
				},
			},
			{
				AlbumID:   "album-old",
				CreatedAt: "2026-01-01T00:00:00Z",
				Photos: []models.PhotoMeta{
					{I: 9, W: 100, H: 80, Ratio: 1.25},
				},
			},
		},
	}
	svc := NewService(nil)
	svc.albums = source
	svc.snapshotTTL = 10 * time.Minute
	svc.now = func() time.Time { return time.Unix(902, 0) }

	resp, err := svc.Build(context.Background(), 3, "ignored", ModeLatest)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items=%d want=3", len(resp.Items))
	}
	if resp.Items[2].AlbumID != "album-invalid" {
		t.Fatalf("expected malformed createdAt album to be last, got order=%+v", resp.Items)
	}
}

func buildAlbumsWithSequentialCreatedAt(total int) ([]*models.AlbumIndex, map[string]struct{}) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	albumsList := make([]*models.AlbumIndex, 0, total)

	recentStart := total - recentAlbumsWindow
	if recentStart < 0 {
		recentStart = 0
	}
	recentSet := make(map[string]struct{}, total-recentStart)

	for i := 0; i < total; i++ {
		albumID := fmt.Sprintf("album-%03d", i)
		albumsList = append(albumsList, &models.AlbumIndex{
			AlbumID:   albumID,
			CreatedAt: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			Photos: []models.PhotoMeta{
				{I: 0, W: 100, H: 80, Ratio: 1.25},
			},
		})
		if i >= recentStart {
			recentSet[albumID] = struct{}{}
		}
	}

	return albumsList, recentSet
}
