package albums

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	cfgpkg "viewer/internal/config"
	"viewer/internal/models"
	"viewer/internal/storage"
)

type Service struct {
	cfg     cfgpkg.Config
	store   albumStore
	indexer *Indexer

	mu                     sync.RWMutex
	albumCache             map[string]*models.AlbumIndex
	uploadHints            map[string]string
	multipartSessions      map[string]multipartSession
	albumSummariesSnapshot []models.AlbumSummary
	allAlbumsSnapshot      []*models.AlbumIndex
}

type RefreshProgress struct {
	Done        int
	Total       int
	Discovered  int
	Processed   int
	Succeeded   int
	Failed      int
	ListingDone bool
	AlbumID     string
	Key         string
	Err         error
}

type albumStore interface {
	CreateMultipartUpload(ctx context.Context, key string, contentType string) (string, error)
	PresignUploadPart(ctx context.Context, key string, uploadID string, partNumber int32, ttl time.Duration) (string, map[string]string, error)
	ListMultipartUploadParts(ctx context.Context, key string, uploadID string) ([]s3types.CompletedPart, error)
	CompleteMultipartUpload(ctx context.Context, key string, uploadID string, parts []s3types.CompletedPart) error
	AbortMultipartUpload(ctx context.Context, key string, uploadID string) error
	HeadObject(ctx context.Context, key string) (bool, int64, error)
	GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error)
	PutJSON(ctx context.Context, key string, v any) error
	ReadJSON(ctx context.Context, key string, out any) error
	ForEachAlbumIndexKey(ctx context.Context, fn func(key string) error) error
}

func NewService(cfg cfgpkg.Config, store *storage.S3Store, indexer *Indexer) *Service {
	return &Service{
		cfg:               cfg,
		store:             store,
		indexer:           indexer,
		albumCache:        make(map[string]*models.AlbumIndex),
		uploadHints:       make(map[string]string),
		multipartSessions: make(map[string]multipartSession),
	}
}

type CreateUploadResult struct {
	AlbumID       string
	Key           string
	Strategy      string
	PartSizeBytes int64
	MaxParts      int
}

type InitiateMultipartResult struct {
	UploadID      string
	PartSizeBytes int64
	PartCount     int
}

type PresignPartResult struct {
	URL     string
	Headers map[string]string
}

type CompletePart struct {
	PartNumber int32
	ETag       string
}

type multipartSession struct {
	AlbumID       string
	Key           string
	UploadID      string
	SizeBytes     int64
	PartSizeBytes int64
	PartCount     int
	CreatedAt     time.Time
}

func sourceKey(albumID string) string {
	return fmt.Sprintf("albums/%s/source.zip", albumID)
}

func indexKey(albumID string) string {
	return fmt.Sprintf("albums/%s/index.json", albumID)
}

const (
	multipartUploadStrategy = "s3_multipart"
	multipartMaxParts       = 10_000
	multipartMinPartSize    = int64(16 << 20) // 16 MiB
	multipartSessionTTL     = 24 * time.Hour
)

func (s *Service) CreateUpload(ctx context.Context, filename string, sizeBytes int64) (CreateUploadResult, error) {
	_ = ctx
	if strings.TrimSpace(filename) == "" {
		return CreateUploadResult{}, fmt.Errorf("filename is required")
	}
	if sizeBytes <= 0 {
		return CreateUploadResult{}, fmt.Errorf("sizeBytes must be > 0")
	}
	if sizeBytes > s.cfg.MaxUploadBytes {
		return CreateUploadResult{}, fmt.Errorf("sizeBytes exceeds MAX_UPLOAD_BYTES")
	}

	albumID := uuid.NewString()
	partSize := computeMultipartPartSize(sizeBytes)

	s.mu.Lock()
	s.uploadHints[albumID] = filename
	s.mu.Unlock()

	return CreateUploadResult{
		AlbumID:       albumID,
		Key:           sourceKey(albumID),
		Strategy:      multipartUploadStrategy,
		PartSizeBytes: partSize,
		MaxParts:      multipartMaxParts,
	}, nil
}

func (s *Service) InitiateMultipartUpload(ctx context.Context, albumID string, sizeBytes int64, contentType string) (InitiateMultipartResult, error) {
	if strings.TrimSpace(albumID) == "" {
		return InitiateMultipartResult{}, fmt.Errorf("albumId is required")
	}
	if sizeBytes <= 0 {
		return InitiateMultipartResult{}, fmt.Errorf("sizeBytes must be > 0")
	}
	if sizeBytes > s.cfg.MaxUploadBytes {
		return InitiateMultipartResult{}, fmt.Errorf("sizeBytes exceeds MAX_UPLOAD_BYTES")
	}

	partSize := computeMultipartPartSize(sizeBytes)
	partCount := int((sizeBytes + partSize - 1) / partSize)
	if partCount <= 0 || partCount > multipartMaxParts {
		return InitiateMultipartResult{}, fmt.Errorf("multipart part count exceeds limit")
	}

	normalizedContentType := strings.TrimSpace(contentType)
	if normalizedContentType == "" {
		normalizedContentType = "application/zip"
	}

	uploadID, err := s.store.CreateMultipartUpload(ctx, sourceKey(albumID), normalizedContentType)
	if err != nil {
		return InitiateMultipartResult{}, err
	}

	session := multipartSession{
		AlbumID:       albumID,
		Key:           sourceKey(albumID),
		UploadID:      uploadID,
		SizeBytes:     sizeBytes,
		PartSizeBytes: partSize,
		PartCount:     partCount,
		CreatedAt:     time.Now().UTC(),
	}

	s.mu.Lock()
	s.cleanupMultipartSessionsLocked(time.Now().UTC())
	s.multipartSessions[multipartSessionKey(albumID, uploadID)] = session
	s.mu.Unlock()

	return InitiateMultipartResult{
		UploadID:      uploadID,
		PartSizeBytes: partSize,
		PartCount:     partCount,
	}, nil
}

func (s *Service) PresignMultipartUploadPart(ctx context.Context, albumID string, uploadID string, partNumber int32) (PresignPartResult, error) {
	session, err := s.getMultipartSession(albumID, uploadID)
	if err != nil {
		return PresignPartResult{}, err
	}
	if partNumber <= 0 {
		return PresignPartResult{}, fmt.Errorf("partNumber must be > 0")
	}
	if int(partNumber) > session.PartCount {
		return PresignPartResult{}, fmt.Errorf("partNumber exceeds expected count")
	}

	url, headers, err := s.store.PresignUploadPart(ctx, session.Key, session.UploadID, partNumber, s.cfg.PresignTTL)
	if err != nil {
		return PresignPartResult{}, err
	}
	return PresignPartResult{URL: url, Headers: headers}, nil
}

func (s *Service) CompleteMultipartUpload(ctx context.Context, albumID string, uploadID string, parts []CompletePart) error {
	session, err := s.getMultipartSession(albumID, uploadID)
	if err != nil {
		return err
	}

	completedParts := make([]s3types.CompletedPart, 0, session.PartCount)
	if len(parts) == 0 {
		completedParts, err = s.store.ListMultipartUploadParts(ctx, session.Key, session.UploadID)
		if err != nil {
			return err
		}
	} else {
		completedParts = toCompletedParts(parts)
	}
	if err := validateCompletedParts(completedParts, session.PartCount); err != nil {
		return err
	}

	if err := s.store.CompleteMultipartUpload(ctx, session.Key, session.UploadID, completedParts); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.multipartSessions, multipartSessionKey(albumID, uploadID))
	s.mu.Unlock()
	return nil
}

func (s *Service) AbortMultipartUpload(ctx context.Context, albumID string, uploadID string) error {
	session, err := s.getMultipartSession(albumID, uploadID)
	if err != nil {
		return err
	}
	if err := s.store.AbortMultipartUpload(ctx, session.Key, session.UploadID); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.multipartSessions, multipartSessionKey(albumID, uploadID))
	s.mu.Unlock()
	return nil
}

func (s *Service) Finalize(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	if strings.TrimSpace(albumID) == "" {
		return nil, fmt.Errorf("albumId is required")
	}

	sourceObjectKey := sourceKey(albumID)
	exists, size, err := s.store.HeadObject(ctx, sourceObjectKey)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}
	if size <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}

	originalFilename := "source.zip"
	s.mu.RLock()
	if hinted, ok := s.uploadHints[albumID]; ok && hinted != "" {
		originalFilename = hinted
	}
	s.mu.RUnlock()

	readerAt := &s3ObjectReaderAt{
		ctx:   ctx,
		store: s.store,
		key:   sourceObjectKey,
		size:  size,
	}
	idx, err := s.indexer.BuildFromZipReaderAt(readerAt, size, albumID, originalFilename)
	if err != nil {
		return nil, err
	}

	if err := s.store.PutJSON(ctx, indexKey(albumID), idx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.albumCache[albumID] = idx
	s.invalidateSnapshotsLocked()
	delete(s.uploadHints, albumID)
	s.mu.Unlock()

	return idx, nil
}

func computeMultipartPartSize(sizeBytes int64) int64 {
	partSize := multipartMinPartSize
	minimumByPartCount := (sizeBytes + multipartMaxParts - 1) / multipartMaxParts
	if minimumByPartCount > partSize {
		partSize = minimumByPartCount
	}

	const miB = int64(1 << 20)
	if remainder := partSize % miB; remainder != 0 {
		partSize += miB - remainder
	}
	return partSize
}

func multipartSessionKey(albumID string, uploadID string) string {
	return albumID + ":" + uploadID
}

func (s *Service) cleanupMultipartSessionsLocked(now time.Time) {
	for key, session := range s.multipartSessions {
		if now.Sub(session.CreatedAt) > multipartSessionTTL {
			delete(s.multipartSessions, key)
		}
	}
}

func (s *Service) getMultipartSession(albumID string, uploadID string) (multipartSession, error) {
	if strings.TrimSpace(albumID) == "" {
		return multipartSession{}, fmt.Errorf("albumId is required")
	}
	if strings.TrimSpace(uploadID) == "" {
		return multipartSession{}, fmt.Errorf("uploadId is required")
	}

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupMultipartSessionsLocked(now)

	session, ok := s.multipartSessions[multipartSessionKey(albumID, uploadID)]
	if !ok {
		return multipartSession{}, fmt.Errorf("%w: %s", ErrMultipartNotFound, uploadID)
	}
	return session, nil
}

func toCompletedParts(parts []CompletePart) []s3types.CompletedPart {
	completed := make([]s3types.CompletedPart, 0, len(parts))
	for _, part := range parts {
		etag := strings.TrimSpace(part.ETag)
		completed = append(completed, s3types.CompletedPart{
			PartNumber: int32Ptr(part.PartNumber),
			ETag:       stringPtr(etag),
		})
	}
	return completed
}

func validateCompletedParts(parts []s3types.CompletedPart, expectedPartCount int) error {
	if len(parts) == 0 {
		return fmt.Errorf("at least one completed part is required")
	}
	if expectedPartCount > 0 && len(parts) != expectedPartCount {
		return fmt.Errorf("expected %d parts, got %d", expectedPartCount, len(parts))
	}

	sort.Slice(parts, func(i, j int) bool {
		return derefInt32(parts[i].PartNumber) < derefInt32(parts[j].PartNumber)
	})

	expectedPartNumber := int32(1)
	for i := range parts {
		part := parts[i]
		if part.PartNumber == nil || *part.PartNumber <= 0 {
			return fmt.Errorf("invalid part number at index %d", i)
		}
		if part.ETag == nil || strings.TrimSpace(*part.ETag) == "" {
			return fmt.Errorf("missing ETag for part %d", *part.PartNumber)
		}
		if *part.PartNumber != expectedPartNumber {
			return fmt.Errorf("missing or duplicate part number %d", expectedPartNumber)
		}
		expectedPartNumber++
	}
	return nil
}

func int32Ptr(v int32) *int32 {
	return &v
}

func stringPtr(v string) *string {
	return &v
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

type s3ObjectReaderAt struct {
	ctx   context.Context
	store albumStore
	key   string
	size  int64
}

func (r *s3ObjectReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r == nil || r.store == nil {
		return 0, fmt.Errorf("range reader is not initialized")
	}
	if off < 0 {
		return 0, fmt.Errorf("negative offset: %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if max := r.size - off; toRead > max {
		toRead = max
	}
	start := off
	end := off + toRead - 1
	body, _, err := r.store.GetObjectRange(r.ctx, r.key, start, end)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	n, readErr := io.ReadFull(body, p[:int(toRead)])
	if readErr != nil {
		return n, fmt.Errorf("read object range %s %d-%d: %w", r.key, start, end, readErr)
	}
	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (s *Service) RefreshFromStorage(ctx context.Context) error {
	return s.refreshFromStorage(ctx, nil, nil)
}

func (s *Service) RefreshFromStorageWithProgress(ctx context.Context, onProgress func(RefreshProgress)) error {
	return s.refreshFromStorage(ctx, onProgress, nil)
}

func (s *Service) RefreshFromStorageWithProgressAndAlbum(ctx context.Context, onProgress func(RefreshProgress), onAlbum func(models.AlbumIndex)) error {
	return s.refreshFromStorage(ctx, onProgress, onAlbum)
}

func (s *Service) refreshFromStorage(ctx context.Context, onProgress func(RefreshProgress), onAlbum func(models.AlbumIndex)) error {
	workerCount := warmupWorkerCount(s.cfg.WarmupFetchConcurrency)
	keyCh := make(chan string, workerCount*2)
	resultCh := make(chan RefreshProgress, workerCount*2)

	var discovered atomic.Int64
	var processed atomic.Int64
	var succeeded atomic.Int64
	var failed atomic.Int64
	var listingDone atomic.Bool

	producerDone := make(chan error, 1)
	go func() {
		defer close(keyCh)
		err := s.store.ForEachAlbumIndexKey(ctx, func(key string) error {
			select {
			case keyCh <- key:
				discovered.Add(1)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		listingDone.Store(true)
		producerDone <- err
	}()

	var workers sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range keyCh {
				progress := RefreshProgress{Key: key}
				var idx models.AlbumIndex
				if err := s.store.ReadJSON(ctx, key, &idx); err != nil {
					progress.Err = err
					resultCh <- progress
					continue
				}

				if idx.AlbumID == "" {
					parts := strings.Split(filepath.Dir(key), "/")
					if len(parts) > 0 {
						idx.AlbumID = parts[len(parts)-1]
					}
				}
				if idx.AlbumID == "" {
					progress.Err = fmt.Errorf("missing album id")
					resultCh <- progress
					continue
				}

				cached := new(models.AlbumIndex)
				*cached = idx
				s.mu.Lock()
				s.albumCache[idx.AlbumID] = cached
				s.invalidateSnapshotsLocked()
				s.mu.Unlock()
				if onAlbum != nil {
					onAlbum(idx)
				}

				progress.AlbumID = idx.AlbumID
				resultCh <- progress
			}
		}()
	}

	go func() {
		workers.Wait()
		close(resultCh)
	}()

	for progress := range resultCh {
		done := processed.Add(1)
		if progress.Err != nil {
			failed.Add(1)
		} else {
			succeeded.Add(1)
		}
		progress.Done = int(done)
		progress.Total = int(discovered.Load())
		progress.Discovered = int(discovered.Load())
		progress.Processed = int(done)
		progress.Succeeded = int(succeeded.Load())
		progress.Failed = int(failed.Load())
		progress.ListingDone = listingDone.Load()
		if onProgress != nil {
			onProgress(progress)
		}
	}

	if err := <-producerDone; err != nil {
		return err
	}
	return nil
}

func warmupWorkerCount(configured int) int {
	if configured > 0 {
		return configured
	}
	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 2 {
		return 2
	}
	if workers > 32 {
		return 32
	}
	return workers
}

func mergeAlbumCaches(existing map[string]*models.AlbumIndex, scanned map[string]*models.AlbumIndex) map[string]*models.AlbumIndex {
	merged := make(map[string]*models.AlbumIndex, len(existing)+len(scanned))
	for albumID, idx := range existing {
		merged[albumID] = idx
	}
	for albumID, idx := range scanned {
		merged[albumID] = idx
	}
	return merged
}

func (s *Service) GetAlbum(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	s.mu.RLock()
	if idx, ok := s.albumCache[albumID]; ok {
		dup := *idx
		s.mu.RUnlock()
		return &dup, nil
	}
	s.mu.RUnlock()

	var idx models.AlbumIndex
	if err := s.store.ReadJSON(ctx, indexKey(albumID), &idx); err != nil {
		if storage.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
		}
		return nil, err
	}
	if idx.AlbumID == "" {
		idx.AlbumID = albumID
	}

	s.mu.Lock()
	s.albumCache[albumID] = &idx
	s.invalidateSnapshotsLocked()
	s.mu.Unlock()

	return &idx, nil
}

func (s *Service) ListAlbums(ctx context.Context) ([]models.AlbumSummary, error) {
	_ = ctx
	s.mu.RLock()
	if s.albumSummariesSnapshot != nil {
		out := cloneAlbumSummarySlice(s.albumSummariesSnapshot)
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	if s.albumSummariesSnapshot == nil || s.allAlbumsSnapshot == nil {
		s.rebuildSnapshotsLocked()
	}
	out := cloneAlbumSummarySlice(s.albumSummariesSnapshot)
	s.mu.Unlock()
	return out, nil
}

func (s *Service) SearchAlbumsByNamePrefix(ctx context.Context, q string, limit int) ([]models.AlbumSearchItem, error) {
	_ = ctx
	if limit <= 0 {
		limit = 20
	}

	normalizedQuery := strings.ToLower(strings.TrimSpace(q))

	s.mu.RLock()
	items := make([]models.AlbumSearchItem, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		if idx == nil {
			continue
		}
		if normalizedQuery != "" && !strings.HasPrefix(strings.ToLower(idx.OriginalFilename), normalizedQuery) {
			continue
		}
		items = append(items, albumSearchItemFromIndex(idx))
	}
	s.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		if items[i].OriginalFilename != items[j].OriginalFilename {
			return items[i].OriginalFilename < items[j].OriginalFilename
		}
		return items[i].AlbumID < items[j].AlbumID
	})

	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *Service) AllAlbums() []*models.AlbumIndex {
	s.mu.RLock()
	if s.allAlbumsSnapshot != nil {
		out := cloneAlbumIndexSlice(s.allAlbumsSnapshot)
		s.mu.RUnlock()
		return out
	}
	s.mu.RUnlock()

	s.mu.Lock()
	if s.albumSummariesSnapshot == nil || s.allAlbumsSnapshot == nil {
		s.rebuildSnapshotsLocked()
	}
	out := cloneAlbumIndexSlice(s.allAlbumsSnapshot)
	s.mu.Unlock()
	return out
}

func (s *Service) DumpAlbumJSON(albumID string) ([]byte, error) {
	s.mu.RLock()
	idx, ok := s.albumCache[albumID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	return json.Marshal(idx)
}

func (s *Service) invalidateSnapshotsLocked() {
	s.albumSummariesSnapshot = nil
	s.allAlbumsSnapshot = nil
}

func (s *Service) rebuildSnapshotsLocked() {
	summaries := make([]models.AlbumSummary, 0, len(s.albumCache))
	albumsList := make([]*models.AlbumIndex, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		summaries = append(summaries, models.AlbumSummary{
			AlbumID:          idx.AlbumID,
			OriginalFilename: idx.OriginalFilename,
			PhotoCount:       idx.PhotoCount,
			CreatedAt:        idx.CreatedAt,
		})
		albumsList = append(albumsList, cloneAlbumIndex(idx))
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt > summaries[j].CreatedAt
	})
	sort.Slice(albumsList, func(i, j int) bool {
		if albumsList[i].AlbumID != albumsList[j].AlbumID {
			return albumsList[i].AlbumID < albumsList[j].AlbumID
		}
		if albumsList[i].CreatedAt != albumsList[j].CreatedAt {
			return albumsList[i].CreatedAt < albumsList[j].CreatedAt
		}
		return albumsList[i].OriginalFilename < albumsList[j].OriginalFilename
	})

	s.albumSummariesSnapshot = summaries
	s.allAlbumsSnapshot = albumsList
}

func cloneAlbumSummarySlice(in []models.AlbumSummary) []models.AlbumSummary {
	return append([]models.AlbumSummary(nil), in...)
}

func cloneAlbumIndexSlice(in []*models.AlbumIndex) []*models.AlbumIndex {
	out := make([]*models.AlbumIndex, 0, len(in))
	for _, idx := range in {
		out = append(out, cloneAlbumIndex(idx))
	}
	return out
}

func cloneAlbumIndex(idx *models.AlbumIndex) *models.AlbumIndex {
	if idx == nil {
		return nil
	}
	dup := *idx
	dup.Photos = append([]models.PhotoMeta(nil), idx.Photos...)
	if idx.Embeddings != nil {
		dup.Embeddings = make(map[string]models.PhotoEmbedding, len(idx.Embeddings))
		for key, value := range idx.Embeddings {
			embeddingCopy := value
			if value.Vector != nil {
				embeddingCopy.Vector = append([]float32(nil), value.Vector...)
			}
			dup.Embeddings[key] = embeddingCopy
		}
	}
	return &dup
}

func albumSearchItemFromIndex(idx *models.AlbumIndex) models.AlbumSearchItem {
	indexStatus, indexedCount, failedCount, totalCount := albumIndexStatusCounts(idx)
	return models.AlbumSearchItem{
		AlbumID:          idx.AlbumID,
		OriginalFilename: idx.OriginalFilename,
		PhotoCount:       idx.PhotoCount,
		CreatedAt:        idx.CreatedAt,
		IndexStatus:      indexStatus,
		IndexedCount:     indexedCount,
		FailedCount:      failedCount,
		TotalCount:       totalCount,
	}
}

func albumIndexStatusCounts(idx *models.AlbumIndex) (models.AlbumIndexStatus, int, int, int) {
	if idx == nil {
		return models.AlbumIndexStatusPending, 0, 0, 0
	}

	readyCount := 0
	failedCount := 0
	totalCount := len(idx.Photos)
	if totalCount == 0 && idx.PhotoCount > 0 {
		totalCount = idx.PhotoCount
	}

	for _, photo := range idx.Photos {
		embedding, ok := idx.Embeddings[strconv.Itoa(photo.I)]
		if !ok {
			continue
		}
		switch embedding.Status {
		case "ready":
			if len(embedding.Vector) > 0 {
				readyCount++
			}
		case "failed":
			failedCount++
		}
	}

	if totalCount == 0 {
		return models.AlbumIndexStatusPending, readyCount, failedCount, totalCount
	}
	if readyCount == totalCount {
		return models.AlbumIndexStatusReady, readyCount, failedCount, totalCount
	}
	if readyCount > 0 {
		return models.AlbumIndexStatusPartial, readyCount, failedCount, totalCount
	}
	if failedCount > 0 {
		return models.AlbumIndexStatusFailed, readyCount, failedCount, totalCount
	}
	return models.AlbumIndexStatusPending, readyCount, failedCount, totalCount
}
