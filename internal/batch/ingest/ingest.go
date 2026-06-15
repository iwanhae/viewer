package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"viewer/internal/albums"
	"viewer/internal/storage"
)

const (
	defaultBatchPrefix = "batch/"
	albumIDHashLen     = 16
)

type Store interface {
	ListBatchObjects(ctx context.Context, prefix string) ([]storage.BatchObject, error)
	HeadObject(ctx context.Context, key string) (bool, int64, error)
	GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error)
	CopyObject(ctx context.Context, srcKey, dstKey string) error
	DeleteObject(ctx context.Context, key string) error
}

type Summary struct {
	Discovered    int
	Moved         int
	Deduped       int
	DeletedFailed int
	Errors        int
}

type RunOptions struct {
	BatchPrefix string
}

func Run(ctx context.Context, store Store, indexer *albums.Indexer, opts RunOptions) (Summary, error) {
	summary := Summary{}

	prefix := strings.TrimSpace(opts.BatchPrefix)
	if prefix == "" {
		prefix = defaultBatchPrefix
	}

	objects, err := store.ListBatchObjects(ctx, prefix)
	if err != nil {
		return summary, fmt.Errorf("list batch objects: %w", err)
	}

	candidates := filterTopLevelZips(objects, prefix)
	summary.Discovered = len(candidates)

	for _, obj := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		processOne(ctx, store, indexer, obj, prefix, &summary)
	}

	return summary, nil
}

func filterTopLevelZips(objects []storage.BatchObject, prefix string) []storage.BatchObject {
	out := make([]storage.BatchObject, 0, len(objects))
	for _, obj := range objects {
		if !strings.HasSuffix(strings.ToLower(obj.Key), ".zip") {
			continue
		}
		rel := strings.TrimPrefix(obj.Key, prefix)
		if rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if obj.Size <= 0 {
			continue
		}
		out = append(out, obj)
	}
	return out
}

func processOne(ctx context.Context, store Store, indexer *albums.Indexer, obj storage.BatchObject, prefix string, summary *Summary) {
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := fmt.Sprintf("albums/%s/source.zip", albumID)
	originalFilename := strings.TrimPrefix(obj.Key, prefix)

	exists, _, err := store.HeadObject(ctx, dstKey)
	if err != nil {
		summary.Errors++
		return
	}
	if exists {
		if err := store.DeleteObject(ctx, obj.Key); err != nil {
			summary.Errors++
			return
		}
		summary.Deduped++
		return
	}

	valid, validateErr := validateZip(ctx, store, indexer, obj, albumID, originalFilename)
	if validateErr != nil {
		summary.Errors++
		return
	}
	if !valid {
		_ = store.DeleteObject(ctx, obj.Key)
		summary.DeletedFailed++
		return
	}

	if err := store.CopyObject(ctx, obj.Key, dstKey); err != nil {
		summary.Errors++
		return
	}
	if err := store.DeleteObject(ctx, obj.Key); err != nil {
		summary.Errors++
		return
	}
	summary.Moved++
}

func validateZip(ctx context.Context, store Store, indexer *albums.Indexer, obj storage.BatchObject, albumID, originalFilename string) (bool, error) {
	readerAt := &storeReaderAt{ctx: ctx, store: store, key: obj.Key, size: obj.Size}
	if _, err := indexer.BuildFromZipReaderAt(readerAt, obj.Size, albumID, originalFilename); err != nil {
		if err == albums.ErrNoValidImages {
			return false, nil
		}
		if isZipOpenErr(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isZipOpenErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "open zip:") || strings.Contains(msg, "zip:")
}

func albumIDFromContent(etag string, size int64) string {
	etag = strings.Trim(etag, "\"")
	raw := fmt.Sprintf("%s:%d", etag, size)
	sum := sha256.Sum256([]byte(raw))
	hexStr := hex.EncodeToString(sum[:])
	if len(hexStr) > albumIDHashLen {
		hexStr = hexStr[:albumIDHashLen]
	}
	return hexStr
}

type storeReaderAt struct {
	ctx   context.Context
	store Store
	key   string
	size  int64
}

func (r *storeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
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
