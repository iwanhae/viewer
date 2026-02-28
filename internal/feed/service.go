package feed

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"viewer/internal/albums"
	"viewer/internal/models"
)

const (
	photoRefsSnapshotTTL       = 3 * time.Second
	recentAlbumsWindow         = 100
	recentAlbumSharePercent    = 20
	recentStreamOffset         = int64(1_000_000)
	recentFallbackStreamOffset = int64(2_000_000)
)

type Mode string

const (
	ModeRandom Mode = "random"
	ModeLatest Mode = "latest"
)

type albumSource interface {
	AllAlbums() []*models.AlbumIndex
}

type Service struct {
	albums albumSource

	mu             sync.RWMutex
	albumsSnapshot []albumRef
	albumsAt       time.Time
	snapshotTTL    time.Duration
	now            func() time.Time
}

type albumRef struct {
	albumID   string
	photos    []models.PhotoMeta
	createdAt string
}

func NewService(albumsService *albums.Service) *Service {
	return &Service{
		albums:      albumsService,
		snapshotTTL: photoRefsSnapshotTTL,
		now:         time.Now,
	}
}

func ParseMode(modeParam string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(modeParam)) {
	case "", string(ModeRandom):
		return ModeRandom, nil
	case string(ModeLatest):
		return ModeLatest, nil
	default:
		return "", fmt.Errorf("invalid mode")
	}
}

func (s *Service) Build(ctx context.Context, limit int, seedParam string, mode Mode) (models.FeedResponse, error) {
	_ = ctx
	if limit <= 0 {
		limit = 80
	}
	if limit > 200 {
		limit = 200
	}

	albumsList := s.snapshotAlbums()
	if len(albumsList) == 0 {
		return models.FeedResponse{Items: []models.FeedItem{}}, nil
	}

	if mode == "" {
		mode = ModeRandom
	}
	if mode == ModeLatest {
		return models.FeedResponse{Items: buildLatestItems(limit, albumsList)}, nil
	}
	if mode != ModeRandom {
		return models.FeedResponse{}, fmt.Errorf("invalid mode")
	}

	seed := parseSeed(seedParam)
	recentAlbums, nonRecentAlbums := splitRecentAlbums(albumsList, recentAlbumsWindow)
	recentQuota := calculateRecentQuota(limit, len(recentAlbums))

	items := make([]models.FeedItem, 0, limit)
	for i := 0; i < limit; i++ {
		position := int64(i)
		pool := nonRecentAlbums
		streamOffset := int64(0)
		if isRecentSlot(i, limit, recentQuota) {
			pool = recentAlbums
			streamOffset = recentStreamOffset
		} else if len(pool) == 0 {
			pool = recentAlbums
			streamOffset = recentFallbackStreamOffset
		}
		if len(pool) == 0 {
			continue
		}

		items = append(items, sampleFeedItem(seed, position, streamOffset, pool))
	}
	return models.FeedResponse{Items: items}, nil
}

func buildLatestItems(limit int, albumsList []albumRef) []models.FeedItem {
	ranked := rankAlbumsByCreatedAt(albumsList)
	if limit > len(ranked) {
		limit = len(ranked)
	}

	items := make([]models.FeedItem, 0, limit)
	for i := 0; i < limit; i++ {
		album := ranked[i].album
		photo := album.photos[0]
		items = append(items, models.FeedItem{
			AlbumID: album.albumID,
			I:       photo.I,
			W:       photo.W,
			H:       photo.H,
			Ratio:   photo.Ratio,
		})
	}
	return items
}

func (s *Service) snapshotAlbums() []albumRef {
	nowFn := s.now
	if nowFn == nil {
		nowFn = time.Now
	}
	ttl := s.snapshotTTL
	if ttl <= 0 {
		ttl = photoRefsSnapshotTTL
	}
	now := nowFn()

	s.mu.RLock()
	if s.albumsSnapshot != nil && now.Sub(s.albumsAt) < ttl {
		albumsList := s.albumsSnapshot
		s.mu.RUnlock()
		return albumsList
	}
	s.mu.RUnlock()

	if s.albums == nil {
		return nil
	}
	albumsIndex := s.albums.AllAlbums()
	albumsList := make([]albumRef, 0, len(albumsIndex))
	for _, album := range albumsIndex {
		if album == nil || len(album.Photos) == 0 {
			continue
		}
		albumsList = append(albumsList, albumRef{
			albumID:   album.AlbumID,
			photos:    album.Photos,
			createdAt: album.CreatedAt,
		})
	}

	s.mu.Lock()
	s.albumsSnapshot = albumsList
	s.albumsAt = now
	s.mu.Unlock()
	return albumsList
}

func parseSeed(seed string) int64 {
	if seed == "" {
		return time.Now().UnixNano()
	}
	if n, err := strconv.ParseInt(seed, 10, 64); err == nil {
		return n
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return int64(h.Sum64())
}

func deterministicIndex(seed int64, position int64, size int) int {
	if size <= 0 {
		return 0
	}
	x := uint64(seed) + 0x9e3779b97f4a7c15*uint64(position+1)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return int(x % uint64(size))
}

func sampleFeedItem(seed int64, position int64, streamOffset int64, pool []albumRef) models.FeedItem {
	albumIdx := deterministicIndex(seed, streamOffset+position*2, len(pool))
	album := pool[albumIdx]
	photoIdx := deterministicIndex(seed, streamOffset+position*2+1, len(album.photos))
	photo := album.photos[photoIdx]
	return models.FeedItem{
		AlbumID: album.albumID,
		I:       photo.I,
		W:       photo.W,
		H:       photo.H,
		Ratio:   photo.Ratio,
	}
}

type rankedAlbum struct {
	album     albumRef
	createdAt time.Time
}

func rankAlbumsByCreatedAt(albumsList []albumRef) []rankedAlbum {
	ranked := make([]rankedAlbum, 0, len(albumsList))
	for _, album := range albumsList {
		ranked = append(ranked, rankedAlbum{
			album:     album,
			createdAt: parseAlbumCreatedAt(album.createdAt),
		})
	}

	sort.Slice(ranked, func(i, j int) bool {
		if !ranked[i].createdAt.Equal(ranked[j].createdAt) {
			return ranked[i].createdAt.After(ranked[j].createdAt)
		}
		return ranked[i].album.albumID < ranked[j].album.albumID
	})

	return ranked
}

func splitRecentAlbums(albumsList []albumRef, recentLimit int) ([]albumRef, []albumRef) {
	if len(albumsList) == 0 {
		return nil, nil
	}
	if recentLimit <= 0 {
		return nil, append([]albumRef(nil), albumsList...)
	}

	ranked := rankAlbumsByCreatedAt(albumsList)

	if recentLimit > len(ranked) {
		recentLimit = len(ranked)
	}

	recent := make([]albumRef, 0, recentLimit)
	recentSet := make(map[string]struct{}, recentLimit)
	for i := 0; i < recentLimit; i++ {
		recent = append(recent, ranked[i].album)
		recentSet[ranked[i].album.albumID] = struct{}{}
	}

	nonRecent := make([]albumRef, 0, len(albumsList)-recentLimit)
	for _, album := range albumsList {
		if _, ok := recentSet[album.albumID]; ok {
			continue
		}
		nonRecent = append(nonRecent, album)
	}

	return recent, nonRecent
}

func calculateRecentQuota(limit int, recentPoolSize int) int {
	if limit <= 0 || recentPoolSize == 0 {
		return 0
	}
	quota := (limit*recentAlbumSharePercent + 99) / 100
	if quota > limit {
		quota = limit
	}
	return quota
}

func isRecentSlot(position int, limit int, recentQuota int) bool {
	if position < 0 || limit <= 0 || recentQuota <= 0 {
		return false
	}
	return ((position+1)*recentQuota)/limit > (position*recentQuota)/limit
}

func parseAlbumCreatedAt(createdAt string) time.Time {
	createdAt = strings.TrimSpace(createdAt)
	if createdAt == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, createdAt); err == nil {
		return ts
	}
	return time.Time{}
}
