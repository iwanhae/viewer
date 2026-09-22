package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"viewer/internal/albums"
	"viewer/internal/models"
)

const photoRefsSnapshotTTL = 3 * time.Second

// coversLoadTimeout bounds one cover snapshot load. The covers query is a
// couple of indexed reads over the album table plus one cover per album, so a
// healthy catalog answers in well under a second even on a large library.
const coversLoadTimeout = 10 * time.Second

type Mode string

const (
	ModeRandom Mode = "random"
	ModeLatest Mode = "latest"
)

type albumSource interface {
	// ReadyAlbumCovers loads every ready album with its cover photo (index 0).
	// The latest feed ranks and pages over albums, so one photo per album is
	// all it needs; loading every photo row per snapshot meant hundreds of
	// thousands of rows and seconds of latency on a large library.
	ReadyAlbumCovers(ctx context.Context) ([]models.AlbumCoverEntry, error)
	// ReadyAlbumPhotoCounts feeds the random sampler, which needs the album
	// pool without any photo rows.
	ReadyAlbumPhotoCounts(ctx context.Context) ([]models.AlbumPhotoCount, error)
	PhotoMetaAt(ctx context.Context, albumID string, index int) (*models.PhotoMeta, error)
}

type Service struct {
	albums albumSource

	mu             sync.RWMutex
	coversSnapshot []models.AlbumCoverEntry
	coversAt       time.Time
	photoCounts    []models.AlbumPhotoCount
	photoCountsAt  time.Time
	snapshotTTL    time.Duration
	now            func() time.Time
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

func (s *Service) Build(
	ctx context.Context,
	limit int,
	seedParam string,
	mode Mode,
	afterCursor string,
) (models.FeedResponse, error) {
	if limit <= 0 {
		limit = 80
	}
	if limit > 200 {
		limit = 200
	}

	if mode == "" {
		mode = ModeRandom
	}
	if mode == ModeRandom {
		return s.buildRandomPage(ctx, limit, seedParam)
	}
	if mode != ModeLatest {
		return models.FeedResponse{}, fmt.Errorf("invalid mode")
	}

	albumsList := s.snapshotCovers()
	if len(albumsList) == 0 {
		return models.FeedResponse{Items: []models.FeedItem{}}, nil
	}
	return buildLatestPage(limit, albumsList, afterCursor), nil
}

// buildRandomPage samples limit photos without loading the photo catalog.
// It samples an album and a photo index per position from the (tiny) album
// pool, then fetches only the sampled photos by key.
func (s *Service) buildRandomPage(ctx context.Context, limit int, seedParam string) (models.FeedResponse, error) {
	pool, err := s.readyAlbumPhotoCounts(ctx)
	if err != nil {
		return models.FeedResponse{}, err
	}
	if len(pool) == 0 {
		return models.FeedResponse{Items: []models.FeedItem{}}, nil
	}

	seed := parseSeed(seedParam)
	items := make([]models.FeedItem, 0, limit)
	// Positions beyond limit are only reached when a sampled photo row is
	// missing (photo_count drift), which the extractor's contiguous indexes
	// make a never-event; the cap just keeps a broken row from short pages.
	for position := 0; len(items) < limit && position < 2*limit; position++ {
		item, err := s.sampleFeedItem(ctx, seed, int64(position), pool)
		if err != nil {
			if errors.Is(err, albums.ErrPhotoNotFound) {
				continue
			}
			return models.FeedResponse{}, err
		}
		items = append(items, item)
	}
	return models.FeedResponse{
		Items:   items,
		HasNext: false,
		HasPrev: false,
	}, nil
}

type latestCursor struct {
	CreatedAtUnixNano int64  `json:"t"`
	AlbumID           string `json:"a"`
}

func decodeLatestCursor(raw string) (latestCursor, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return latestCursor{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return latestCursor{}, false
	}
	var cursor latestCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return latestCursor{}, false
	}
	if strings.TrimSpace(cursor.AlbumID) == "" {
		return latestCursor{}, false
	}
	return cursor, true
}

func encodeLatestCursor(item rankedAlbum) string {
	payload, err := json.Marshal(latestCursor{
		CreatedAtUnixNano: item.createdAt.UnixNano(),
		AlbumID:           item.entry.AlbumID,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// seekPastCursor returns the index the page after the cursor starts at. The
// ranked list is sorted newest first with the album id breaking ties, so the
// entry following the cursor is the first one sorting strictly older than the
// cursor's (createdAt, albumID) key. Seeking by key rather than looking the
// cursor album up means a cursor whose album has since left the ready set -
// re-ingest drops an album from the list while it re-extracts - resumes where
// the reader was instead of silently restarting the feed from its first page.
func seekPastCursor(ranked []rankedAlbum, cursor latestCursor) int {
	return sort.Search(len(ranked), func(i int) bool {
		itemNano := ranked[i].createdAt.UnixNano()
		if itemNano != cursor.CreatedAtUnixNano {
			return itemNano < cursor.CreatedAtUnixNano
		}
		return ranked[i].entry.AlbumID > cursor.AlbumID
	})
}

func buildLatestPage(limit int, covers []models.AlbumCoverEntry, afterCursor string) models.FeedResponse {
	ranked := rankAlbumsByCreatedAt(covers)
	if len(ranked) == 0 {
		return models.FeedResponse{
			Items:   []models.FeedItem{},
			HasNext: false,
			HasPrev: false,
		}
	}
	if limit > len(ranked) {
		limit = len(ranked)
	}
	if limit <= 0 {
		limit = 1
	}

	start := 0
	if decoded, ok := decodeLatestCursor(afterCursor); ok {
		start = seekPastCursor(ranked, decoded)
	}
	if start < 0 {
		start = 0
	}
	if start > len(ranked) {
		start = len(ranked)
	}
	end := start + limit
	if end > len(ranked) {
		end = len(ranked)
	}

	items := make([]models.FeedItem, 0, end-start)
	for i := start; i < end; i++ {
		entry := ranked[i].entry
		items = append(items, models.FeedItem{
			AlbumID: entry.AlbumID,
			I:       entry.Cover.I,
			Hash:    entry.Cover.Hash,
			W:       entry.Cover.W,
			H:       entry.Cover.H,
			Ratio:   entry.Cover.Ratio,
		})
	}

	hasPrev := start > 0
	hasNext := end < len(ranked)

	cursor := ""
	if hasPrev {
		cursor = encodeLatestCursor(ranked[start-1])
	}

	nextCursor := ""
	if hasNext && end > 0 {
		nextCursor = encodeLatestCursor(ranked[end-1])
	}

	prevCursor := ""
	if hasPrev {
		prevStart := start - limit
		if prevStart < 0 {
			prevStart = 0
		}
		if prevStart > 0 {
			prevCursor = encodeLatestCursor(ranked[prevStart-1])
		}
	}

	return models.FeedResponse{
		Items:      items,
		Cursor:     cursor,
		NextCursor: nextCursor,
		PrevCursor: prevCursor,
		HasNext:    hasNext,
		HasPrev:    hasPrev,
	}
}

func (s *Service) readyAlbumPhotoCounts(ctx context.Context) ([]models.AlbumPhotoCount, error) {
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
	if s.photoCounts != nil && now.Sub(s.photoCountsAt) < ttl {
		counts := s.photoCounts
		s.mu.RUnlock()
		return counts, nil
	}
	s.mu.RUnlock()

	counts, err := s.albums.ReadyAlbumPhotoCounts(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.photoCounts = counts
	s.photoCountsAt = now
	s.mu.Unlock()
	return counts, nil
}

// snapshotCovers returns the feed's view of the ready albums, cached for the
// snapshot TTL so paging through the feed costs no catalog queries. A failed
// load is logged and answered with an empty snapshot: the next request retries.
func (s *Service) snapshotCovers() []models.AlbumCoverEntry {
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
	if s.coversSnapshot != nil && now.Sub(s.coversAt) < ttl {
		covers := s.coversSnapshot
		s.mu.RUnlock()
		return covers
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), coversLoadTimeout)
	defer cancel()
	covers, err := s.albums.ReadyAlbumCovers(ctx)
	if err != nil {
		log.Printf("feed: loading album covers failed: %v", err)
		return nil
	}

	s.mu.Lock()
	s.coversSnapshot = covers
	s.coversAt = now
	s.mu.Unlock()
	return covers
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

func (s *Service) sampleFeedItem(ctx context.Context, seed int64, position int64, pool []models.AlbumPhotoCount) (models.FeedItem, error) {
	albumIdx := deterministicIndex(seed, position*2, len(pool))
	album := pool[albumIdx]
	photoIdx := deterministicIndex(seed, position*2+1, album.PhotoCount)
	photo, err := s.albums.PhotoMetaAt(ctx, album.AlbumID, photoIdx)
	if err != nil {
		return models.FeedItem{}, err
	}
	return models.FeedItem{
		AlbumID: album.AlbumID,
		I:       photo.I,
		Hash:    photo.Hash,
		W:       photo.W,
		H:       photo.H,
		Ratio:   photo.Ratio,
	}, nil
}

type rankedAlbum struct {
	entry     models.AlbumCoverEntry
	createdAt time.Time
}

func rankAlbumsByCreatedAt(covers []models.AlbumCoverEntry) []rankedAlbum {
	ranked := make([]rankedAlbum, 0, len(covers))
	for _, entry := range covers {
		ranked = append(ranked, rankedAlbum{
			entry:     entry,
			createdAt: parseAlbumCreatedAt(entry.CreatedAt),
		})
	}

	sort.Slice(ranked, func(i, j int) bool {
		if !ranked[i].createdAt.Equal(ranked[j].createdAt) {
			return ranked[i].createdAt.After(ranked[j].createdAt)
		}
		return ranked[i].entry.AlbumID < ranked[j].entry.AlbumID
	})

	return ranked
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
