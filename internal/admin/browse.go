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
	"strconv"
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

type HistoryOptions struct {
	Stage  string
	Limit  int
	Cursor string
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

type extractionRunsCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Binding string `json:"binding"`
	RunID   int64  `json:"runId"`
}

type extractionEntriesCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Binding string `json:"binding"`
	Ordinal int    `json:"ordinal"`
}

type ExtractionRunPage struct {
	Runs       []catalog.ExtractionRunSummary `json:"runs"`
	NextCursor *string                        `json:"nextCursor"`
}

type ExtractionEntriesOptions struct {
	RunID   int64
	Outcome string
	Limit   int
	Cursor  string
}

type ExtractionEntriesPage struct {
	Run          catalog.ExtractionRunSummary     `json:"run"`
	RunSelection string                           `json:"runSelection"`
	Entries      []catalog.ExtractionEntryOutcome `json:"entries"`
	NextCursor   *string                          `json:"nextCursor"`
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

// BlobHistory returns a bounded, monotonic event page for one blob. The cursor
// is the last event ID returned, so newly appended events can be read without
// an unbounded offset scan.
func (s *Service) BlobHistory(ctx context.Context, hash string, options HistoryOptions) (catalog.PipelineHistoryPage, error) {
	if s == nil || s.cat == nil {
		return catalog.PipelineHistoryPage{}, fmt.Errorf("catalog is not available")
	}
	if err := validateLimit(options.Limit); err != nil {
		return catalog.PipelineHistoryPage{}, err
	}
	if options.Limit > MaxBrowseLimit {
		return catalog.PipelineHistoryPage{}, fmt.Errorf("%w: history limit must be at most %d", ErrInvalidBrowseRequest, MaxBrowseLimit)
	}
	if options.Stage != "" && options.Stage != "encoding" && options.Stage != "embedding" {
		return catalog.PipelineHistoryPage{}, fmt.Errorf("%w: stage must be encoding or embedding", ErrInvalidBrowseRequest)
	}
	var after int64
	if options.Cursor != "" {
		parsed, err := strconv.ParseInt(options.Cursor, 10, 64)
		if err != nil || parsed < 0 {
			return catalog.PipelineHistoryPage{}, fmt.Errorf("%w: invalid history cursor", ErrInvalidBrowseRequest)
		}
		after = parsed
	}
	return s.cat.PipelineHistory(ctx, hash, options.Stage, after, options.Limit)
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

// ExtractionRuns returns the album's append-only extraction history in
// descending run-id order. Cursors are bound to the album and page size.
func (s *Service) ExtractionRuns(ctx context.Context, albumID string, options BrowsePageOptions) (ExtractionRunPage, error) {
	if s == nil || s.cat == nil {
		return ExtractionRunPage{}, fmt.Errorf("catalog is not available")
	}
	if err := validateLimit(options.Limit); err != nil {
		return ExtractionRunPage{}, err
	}
	binding := browseBinding("extractionRuns", struct {
		AlbumID string `json:"albumId"`
	}{albumID}, options.Limit)
	afterID, err := decodeExtractionRunsCursor(options.Cursor, binding)
	if err != nil {
		return ExtractionRunPage{}, err
	}
	rows, err := s.cat.ListExtractionRuns(ctx, albumID, afterID, options.Limit+1)
	if err != nil {
		return ExtractionRunPage{}, err
	}
	page := ExtractionRunPage{Runs: rows}
	if len(rows) > options.Limit {
		page.Runs = rows[:options.Limit]
		last := page.Runs[len(page.Runs)-1]
		cursor := encodeBrowseCursor(extractionRunsCursor{Version: 1, Kind: "extractionRuns", Binding: binding, RunID: last.RunID})
		page.NextCursor = &cursor
	}
	return page, nil
}

// ExtractionEntries defaults to the latest recorded run. The resolved run id
// is included in the cursor binding so a cursor cannot drift to a newer run.
func (s *Service) ExtractionEntries(ctx context.Context, albumID string, options ExtractionEntriesOptions) (ExtractionEntriesPage, error) {
	if s == nil || s.cat == nil {
		return ExtractionEntriesPage{}, fmt.Errorf("catalog is not available")
	}
	if err := validateLimit(options.Limit); err != nil {
		return ExtractionEntriesPage{}, err
	}
	if options.RunID < 0 {
		return ExtractionEntriesPage{}, fmt.Errorf("%w: runId must be positive", ErrInvalidBrowseRequest)
	}
	if options.Outcome != "" && !oneOf(options.Outcome, "registered", "skipped", "failed") {
		return ExtractionEntriesPage{}, fmt.Errorf("%w: outcome must be registered, skipped, or failed", ErrInvalidBrowseRequest)
	}
	selection := "explicit"
	var run catalog.ExtractionRunSummary
	var err error
	if options.RunID == 0 {
		selection = "latest"
		var found bool
		run, found, err = s.cat.LatestExtractionRun(ctx, albumID)
		if err != nil {
			return ExtractionEntriesPage{}, err
		}
		if !found {
			return ExtractionEntriesPage{}, fmt.Errorf("%w: no recorded extraction run for album %s", catalog.ErrExtractionRunNotFound, albumID)
		}
	} else {
		run, err = s.cat.ExtractionRunByID(ctx, albumID, options.RunID)
		if err != nil {
			return ExtractionEntriesPage{}, err
		}
	}
	if run.Status == "running" {
		return ExtractionEntriesPage{}, fmt.Errorf("%w: %d", catalog.ErrExtractionRunInProgress, run.RunID)
	}
	binding := browseBinding("extractionEntries", struct {
		AlbumID string `json:"albumId"`
		RunID   int64  `json:"runId"`
		Outcome string `json:"outcome"`
	}{albumID, run.RunID, options.Outcome}, options.Limit)
	afterOrdinal, err := decodeExtractionEntriesCursor(options.Cursor, binding)
	if err != nil {
		return ExtractionEntriesPage{}, err
	}
	rows, err := s.cat.ListExtractionEntries(ctx, albumID, run.RunID, options.Outcome, afterOrdinal, options.Limit+1)
	if err != nil {
		return ExtractionEntriesPage{}, err
	}
	page := ExtractionEntriesPage{Run: run, RunSelection: selection, Entries: rows}
	if len(rows) > options.Limit {
		page.Entries = rows[:options.Limit]
		last := page.Entries[len(page.Entries)-1]
		cursor := encodeBrowseCursor(extractionEntriesCursor{Version: 1, Kind: "extractionEntries", Binding: binding, Ordinal: last.Ordinal})
		page.NextCursor = &cursor
	}
	return page, nil
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

func decodeExtractionRunsCursor(token, binding string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	var cursor extractionRunsCursor
	if err := decodeBrowseCursor(token, &cursor); err != nil || cursor.Version != 1 || cursor.Kind != "extractionRuns" || cursor.Binding != binding || cursor.RunID <= 0 {
		return 0, invalidCursor()
	}
	return cursor.RunID, nil
}

func decodeExtractionEntriesCursor(token, binding string) (int, error) {
	if token == "" {
		return -1, nil
	}
	var cursor extractionEntriesCursor
	if err := decodeBrowseCursor(token, &cursor); err != nil || cursor.Version != 1 || cursor.Kind != "extractionEntries" || cursor.Binding != binding || cursor.Ordinal < 0 {
		return 0, invalidCursor()
	}
	return cursor.Ordinal, nil
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
