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
	refsSnapshot   []photoRef
	refsSnapshotAt time.Time
	snapshotTTL    time.Duration
	now            func() time.Time
}

type photoRef struct {
	albumID string
	photo   models.PhotoMeta
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

	refs := s.snapshotPhotoRefs()
	if len(refs) == 0 {
		return models.FeedResponse{Items: []models.FeedItem{}}, nil
	}

	seed := parseSeed(seedParam)

	items := make([]models.FeedItem, 0, limit)
	for i := 0; i < limit; i++ {
		position := int64(i)
		idx := deterministicIndex(seed, position, len(refs))
		ref := refs[idx]
		items = append(items, models.FeedItem{
			AlbumID: ref.albumID,
			I:       ref.photo.I,
			W:       ref.photo.W,
			H:       ref.photo.H,
			Ratio:   ref.photo.Ratio,
		})
	}
	return models.FeedResponse{Items: items}, nil
}

func (s *Service) snapshotPhotoRefs() []photoRef {
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
	if s.refsSnapshot != nil && now.Sub(s.refsSnapshotAt) < ttl {
		refs := s.refsSnapshot
		s.mu.RUnlock()
		return refs
	}
	s.mu.RUnlock()

	if s.albums == nil {
		return nil
	}
	albumsList := s.albums.AllAlbums()
	refs := make([]photoRef, 0)
	for _, album := range albumsList {
		for _, photo := range album.Photos {
			refs = append(refs, photoRef{albumID: album.AlbumID, photo: photo})
		}
	}

	s.mu.Lock()
	s.refsSnapshot = refs
	s.refsSnapshotAt = now
	s.mu.Unlock()
	return refs
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
