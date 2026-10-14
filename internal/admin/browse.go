package admin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"viewer/internal/catalog"
)

var ErrInvalidBrowseRequest = errors.New("invalid admin browse request")

const (
	// DefaultBrowseLimit and MaxBrowseLimit are the shared HTTP/service page bounds.
	DefaultBrowseLimit = 50
	MaxBrowseLimit     = 100
	maxBrowseCursor    = 4096
	maxBrowseQuery     = 256
	maxRawStateFilter  = 128
)

// PhotoFilters are the explicitly supported GET /admin/api/photos filters.
// Query is a literal, case-insensitive SQLite substring over album and photo
// names; %, _ and backslash are not wildcard syntax.
type PhotoFilters struct {
	Query         string
	AlbumID       string
	Hash          string
	Encoding      string
	Embedding     string
	EncodingRaw   string
	EmbeddingRaw  string
	SkippedReason string
}

// BlobFilters are the explicitly supported GET /admin/api/blobs filters.
type BlobFilters struct {
	AlbumID       string
	Hash          string
	Encoding      string
	Embedding     string
	EncodingRaw   string
	EmbeddingRaw  string
	SkippedReason string
	Unreferenced  *bool
}

// AlbumFilters are the explicitly supported GET /admin/api/albums filters.
type AlbumFilters struct {
	Query  string
	Status string
}

type BrowsePageOptions struct {
	Limit  int
	Cursor string
}

type PhotoPage struct {
	Photos     []catalog.AdminPhoto `json:"photos"`
	NextCursor *string              `json:"nextCursor"`
}

type BlobPage struct {
	Blobs      []catalog.AdminBlob `json:"blobs"`
	NextCursor *string             `json:"nextCursor"`
}

type AlbumPage struct {
	Albums     []catalog.AdminAlbum `json:"albums"`
	NextCursor *string              `json:"nextCursor"`
}

type photoCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Binding string `json:"binding"`
	AlbumID string `json:"albumId"`
	Index   int    `json:"idx"`
}

type blobCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Binding string `json:"binding"`
	Hash    string `json:"hash"`
}

type albumCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Binding string `json:"binding"`
	ID      string `json:"id"`
}

// ListPhotos returns a page ordered by the stable (album_id, idx) key. Each
// request is an independent read; cursors do not promise a snapshot while
// rows are concurrently inserted, deleted, or reclassified.
func (s *Service) ListPhotos(ctx context.Context, filters PhotoFilters, options BrowsePageOptions) (PhotoPage, error) {
	if s == nil || s.cat == nil {
		return PhotoPage{}, fmt.Errorf("catalog is not available")
	}
	if err := validateLimit(options.Limit); err != nil {
		return PhotoPage{}, err
	}
	if err := validatePhotoFilters(filters); err != nil {
		return PhotoPage{}, err
	}
	binding := browseBinding("photos", filters, options.Limit)
	afterAlbum, afterIndex, err := decodePhotoCursor(options.Cursor, binding)
	if err != nil {
		return PhotoPage{}, err
	}
	rows, err := s.cat.AdminPhotos(ctx, catalog.AdminPhotoFilters{
		Query: filters.Query, AlbumID: filters.AlbumID, Hash: filters.Hash,
		Encoding: filters.Encoding, Embedding: filters.Embedding,
		EncodingRaw: filters.EncodingRaw, EmbeddingRaw: filters.EmbeddingRaw,
		SkippedReason: filters.SkippedReason,
	}, afterAlbum, afterIndex, options.Limit+1)
	if err != nil {
		return PhotoPage{}, err
	}
	page := PhotoPage{Photos: rows}
	if len(rows) > options.Limit {
		page.Photos = rows[:options.Limit]
		last := page.Photos[len(page.Photos)-1]
		cursor := encodeBrowseCursor(photoCursor{Version: 1, Kind: "photos", Binding: binding, AlbumID: last.AlbumID, Index: last.Index})
		page.NextCursor = &cursor
	}
	return page, nil
}

func (s *Service) PhotoAt(ctx context.Context, albumID string, index int) (catalog.AdminPhoto, error) {
	if s == nil || s.cat == nil {
		return catalog.AdminPhoto{}, fmt.Errorf("catalog is not available")
	}
	return s.cat.AdminPhotoAt(ctx, albumID, index)
}

// ListBlobs returns a page ordered by hash. Relation counts are included, but
// references themselves are available via ListPhotos(hash=...) so a shared
// blob never yields an unbounded response.
func (s *Service) ListBlobs(ctx context.Context, filters BlobFilters, options BrowsePageOptions) (BlobPage, error) {
	if s == nil || s.cat == nil {
		return BlobPage{}, fmt.Errorf("catalog is not available")
	}
	if err := validateLimit(options.Limit); err != nil {
		return BlobPage{}, err
	}
	if err := validateBlobFilters(filters); err != nil {
		return BlobPage{}, err
	}
	binding := browseBinding("blobs", filters, options.Limit)
	afterHash, err := decodeBlobCursor(options.Cursor, binding)
	if err != nil {
		return BlobPage{}, err
	}
	rows, err := s.cat.AdminBlobs(ctx, catalog.AdminBlobFilters{
		AlbumID: filters.AlbumID, Hash: filters.Hash,
		Encoding: filters.Encoding, Embedding: filters.Embedding,
		EncodingRaw: filters.EncodingRaw, EmbeddingRaw: filters.EmbeddingRaw,
		SkippedReason: filters.SkippedReason, Unreferenced: filters.Unreferenced,
	}, afterHash, options.Limit+1)
	if err != nil {
		return BlobPage{}, err
	}
	page := BlobPage{Blobs: rows}
	if len(rows) > options.Limit {
		page.Blobs = rows[:options.Limit]
		cursor := encodeBrowseCursor(blobCursor{Version: 1, Kind: "blobs", Binding: binding, Hash: page.Blobs[len(page.Blobs)-1].Hash})
		page.NextCursor = &cursor
	}
	return page, nil
}

func (s *Service) BlobByHash(ctx context.Context, hash string) (catalog.AdminBlob, error) {
	if s == nil || s.cat == nil {
		return catalog.AdminBlob{}, fmt.Errorf("catalog is not available")
	}
	return s.cat.AdminBlobByHash(ctx, hash)
}

// ListAlbums returns a page ordered by album id. Per-row photo counts are
// computed in the catalog query using the photos primary-key index, not N+1
// application/database round trips.
func (s *Service) ListAlbums(ctx context.Context, filters AlbumFilters, options BrowsePageOptions) (AlbumPage, error) {
	if s == nil || s.cat == nil {
		return AlbumPage{}, fmt.Errorf("catalog is not available")
	}
	if err := validateLimit(options.Limit); err != nil {
		return AlbumPage{}, err
	}
	if err := validateAlbumFilters(filters); err != nil {
		return AlbumPage{}, err
	}
	binding := browseBinding("albums", filters, options.Limit)
	afterID, err := decodeAlbumCursor(options.Cursor, binding)
	if err != nil {
		return AlbumPage{}, err
	}
	rows, err := s.cat.AdminAlbums(ctx, catalog.AdminAlbumFilters{Query: filters.Query, Status: filters.Status}, afterID, options.Limit+1)
	if err != nil {
		return AlbumPage{}, err
	}
	page := AlbumPage{Albums: rows}
	if len(rows) > options.Limit {
		page.Albums = rows[:options.Limit]
		cursor := encodeBrowseCursor(albumCursor{Version: 1, Kind: "albums", Binding: binding, ID: page.Albums[len(page.Albums)-1].ID})
		page.NextCursor = &cursor
	}
	return page, nil
}

func (s *Service) AlbumByID(ctx context.Context, albumID string) (catalog.AdminAlbumDetail, error) {
	if s == nil || s.cat == nil {
		return catalog.AdminAlbumDetail{}, fmt.Errorf("catalog is not available")
	}
	return s.cat.AdminAlbumByID(ctx, albumID)
}

func validateLimit(limit int) error {
	if limit < 1 || limit > MaxBrowseLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidBrowseRequest, MaxBrowseLimit)
	}
	return nil
}

func validatePhotoFilters(filters PhotoFilters) error {
	if err := validateQuery(filters.Query); err != nil {
		return err
	}
	if err := validateStateFilters(filters.Encoding, filters.Embedding, filters.EncodingRaw, filters.EmbeddingRaw, filters.SkippedReason); err != nil {
		return err
	}
	return nil
}

func validateBlobFilters(filters BlobFilters) error {
	return validateStateFilters(filters.Encoding, filters.Embedding, filters.EncodingRaw, filters.EmbeddingRaw, filters.SkippedReason)
}

func validateAlbumFilters(filters AlbumFilters) error {
	if err := validateQuery(filters.Query); err != nil {
		return err
	}
	if filters.Status != "" && !oneOf(filters.Status, "QUEUED", "PROCESSING", "SUCCEEDED", "FAILED") {
		return fmt.Errorf("%w: unsupported album status %q", ErrInvalidBrowseRequest, filters.Status)
	}
	return nil
}

func validateQuery(query string) error {
	if len(query) > maxBrowseQuery {
		return fmt.Errorf("%w: q must be at most %d bytes", ErrInvalidBrowseRequest, maxBrowseQuery)
	}
	return nil
}

func validateStateFilters(encoding, embedding, encodingRaw, embeddingRaw, skippedReason string) error {
	if encoding != "" && !oneOf(encoding, "queued", "processing", "validating", "committing", "completed", "skipped", "failed", "unknown") {
		return fmt.Errorf("%w: unsupported encoding category %q", ErrInvalidBrowseRequest, encoding)
	}
	if embedding != "" && !oneOf(embedding, "queued", "blocked", "processing", "completed", "failed", "unknown") {
		return fmt.Errorf("%w: unsupported embedding category %q", ErrInvalidBrowseRequest, embedding)
	}
	if skippedReason != "" && !oneOf(skippedReason, "webp", "nonWebp") {
		return fmt.Errorf("%w: unsupported skipped reason %q", ErrInvalidBrowseRequest, skippedReason)
	}
	for _, item := range []struct{ name, value string }{
		{name: "encodingRaw", value: encodingRaw},
		{name: "embeddingRaw", value: embeddingRaw},
	} {
		if len(item.value) > maxRawStateFilter {
			return fmt.Errorf("%w: %s must be at most %d bytes", ErrInvalidBrowseRequest, item.name, maxRawStateFilter)
		}
	}
	return nil
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func browseBinding(kind string, filters any, limit int) string {
	canonical, _ := json.Marshal(struct {
		Kind    string `json:"kind"`
		Limit   int    `json:"limit"`
		Filters any    `json:"filters"`
	}{Kind: kind, Limit: limit, Filters: filters})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func encodeBrowseCursor(value any) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodePhotoCursor(token, binding string) (string, int, error) {
	if token == "" {
		return "", 0, nil
	}
	var cursor photoCursor
	if err := decodeBrowseCursor(token, &cursor); err != nil || cursor.Version != 1 || cursor.Kind != "photos" || cursor.Binding != binding || cursor.AlbumID == "" || cursor.Index < 0 {
		return "", 0, invalidCursor()
	}
	return cursor.AlbumID, cursor.Index, nil
}

func decodeBlobCursor(token, binding string) (string, error) {
	if token == "" {
		return "", nil
	}
	var cursor blobCursor
	if err := decodeBrowseCursor(token, &cursor); err != nil || cursor.Version != 1 || cursor.Kind != "blobs" || cursor.Binding != binding || cursor.Hash == "" {
		return "", invalidCursor()
	}
	return cursor.Hash, nil
}

func decodeAlbumCursor(token, binding string) (string, error) {
	if token == "" {
		return "", nil
	}
	var cursor albumCursor
	if err := decodeBrowseCursor(token, &cursor); err != nil || cursor.Version != 1 || cursor.Kind != "albums" || cursor.Binding != binding || cursor.ID == "" {
		return "", invalidCursor()
	}
	return cursor.ID, nil
}

func decodeBrowseCursor(token string, out any) error {
	if len(token) > maxBrowseCursor {
		return invalidCursor()
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return invalidCursor()
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return invalidCursor()
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return invalidCursor()
	}
	return nil
}

func invalidCursor() error {
	return fmt.Errorf("%w: invalid or mismatched cursor", ErrInvalidBrowseRequest)
}
