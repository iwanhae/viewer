package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strconv"
	"time"

	"viewer/internal/albums"
	"viewer/internal/models"
)

type Service struct {
	albums *albums.Service
}

type cursorState struct {
	Seed   int64 `json:"seed"`
	Offset int   `json:"offset"`
}

type photoRef struct {
	albumID string
	photo   models.PhotoMeta
}

func NewService(albumsService *albums.Service) *Service {
	return &Service{albums: albumsService}
}

func (s *Service) Build(ctx context.Context, limit int, cursor string, seedParam string) (models.FeedResponse, error) {
	_ = ctx
	if limit <= 0 {
		limit = 80
	}
	if limit > 200 {
		limit = 200
	}

	albumsList := s.albums.AllAlbums()
	refs := make([]photoRef, 0)
	for _, a := range albumsList {
		for _, p := range a.Photos {
			refs = append(refs, photoRef{albumID: a.AlbumID, photo: p})
		}
	}
	if len(refs) == 0 {
		return models.FeedResponse{Items: []models.FeedItem{}}, nil
	}

	state, err := parseCursor(cursor)
	if err != nil {
		return models.FeedResponse{}, err
	}
	if state == nil {
		seed := parseSeed(seedParam)
		state = &cursorState{Seed: seed, Offset: 0}
	}

	r := rand.New(rand.NewSource(state.Seed))
	for i := 0; i < state.Offset; i++ {
		_ = r.Intn(len(refs))
	}

	items := make([]models.FeedItem, 0, limit)
	for i := 0; i < limit; i++ {
		idx := r.Intn(len(refs))
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

	next := &cursorState{Seed: state.Seed, Offset: state.Offset + len(items)}
	nextCursor, err := encodeCursor(next)
	if err != nil {
		return models.FeedResponse{}, err
	}

	return models.FeedResponse{Items: items, NextCursor: nextCursor}, nil
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
	if err := json.Unmarshal(decoded, &st); err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	if st.Offset < 0 {
		st.Offset = 0
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
