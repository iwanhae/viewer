package feed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
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

type cursorState struct {
	Seed int64 `json:"seed"`
	Page int   `json:"page"`
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

func (s *Service) Build(ctx context.Context, limit int, cursor string, seedParam string) (models.FeedResponse, error) {
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

	state, err := parseCursor(cursor)
	if err != nil {
		return models.FeedResponse{}, err
	}
	if state == nil {
		seed := parseSeed(seedParam)
		state = &cursorState{Seed: seed, Page: 0}
	}

	items := make([]models.FeedItem, 0, limit)
	for i := 0; i < limit; i++ {
		position := int64(state.Page)*int64(limit) + int64(i)
		idx := deterministicIndex(state.Seed, position, len(refs))
		ref := refs[idx]
		items = append(items, models.FeedItem{
			AlbumID: ref.albumID,
			I:       ref.photo.I,
			W:       ref.photo.W,
			H:       ref.photo.H,
			Ratio:   ref.photo.Ratio,
			Src:     fmt.Sprintf("/api/image/%s/%d?mode=wall&w=480", ref.albumID, ref.photo.I),
		})
	}

	next := &cursorState{Seed: state.Seed, Page: state.Page + 1}
	nextCursor, err := encodeCursor(next)
	if err != nil {
		return models.FeedResponse{}, err
	}

	return models.FeedResponse{Items: items, NextCursor: nextCursor}, nil
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

func parseCursor(raw string) (*cursorState, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	var st cursorState
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&st); err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("invalid cursor")
	}
	if st.Page < 0 {
		st.Page = 0
	}
	return &st, nil
}

func encodeCursor(st *cursorState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
