package feed

import (
	"context"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"viewer/internal/albums"
	"viewer/internal/models"
)

const photoRefsSnapshotTTL = 3 * time.Second

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
	albumID string
	photos  []models.PhotoMeta
}

func NewService(albumsService *albums.Service) *Service {
	return &Service{
		albums:      albumsService,
		snapshotTTL: photoRefsSnapshotTTL,
		now:         time.Now,
	}
}

func (s *Service) Build(ctx context.Context, limit int, seedParam string) (models.FeedResponse, error) {
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

	seed := parseSeed(seedParam)

	items := make([]models.FeedItem, 0, limit)
	for i := 0; i < limit; i++ {
		position := int64(i)
		albumIdx := deterministicIndex(seed, position*2, len(albumsList))
		album := albumsList[albumIdx]
		photoIdx := deterministicIndex(seed, position*2+1, len(album.photos))
		photo := album.photos[photoIdx]
		items = append(items, models.FeedItem{
			AlbumID: album.albumID,
			I:       photo.I,
			W:       photo.W,
			H:       photo.H,
			Ratio:   photo.Ratio,
		})
	}
	return models.FeedResponse{Items: items}, nil
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
			albumID: album.AlbumID,
			photos:  album.Photos,
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
