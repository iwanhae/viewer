package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

const (
	defaultBatchPrefix = "batch/"
	albumIDHashLen     = 16
)

// Store is the S3 surface the batch scanner needs.
type Store interface {
	ListBatchObjects(ctx context.Context, prefix string) ([]storage.BatchObject, error)
	HeadObject(ctx context.Context, key string) (bool, int64, error)
	CopyObject(ctx context.Context, srcKey, dstKey string) error
	DeleteObject(ctx context.Context, key string) error
}

// Sink registers a staged zip so the ingest pipeline picks it up.
type Sink interface {
	RegisterStagedUpload(ctx context.Context, albumID string, filename string, sizeBytes int64, sourceKey string) error
}

type Summary struct {
	Discovered int
	Moved      int
	Deduped    int
	Errors     int
}

type RunOptions struct {
	BatchPrefix string
}

// Run moves top-level "batch/*.zip" objects into the staged-upload location and
// queues them for extraction. Album ids are derived from the zip content so the
// same zip uploaded twice maps to the same album.
func Run(ctx context.Context, store Store, sink Sink, opts RunOptions) (Summary, error) {
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
	log.Printf("batch ingest: prefix=%s listed=%d candidates=%d", prefix, len(objects), len(candidates))

	for _, obj := range candidates {
		if err := ctx.Err(); err != nil {
			log.Printf("batch ingest: cancelled at key=%s", obj.Key)
			return summary, err
		}
		processOne(ctx, store, sink, obj, prefix, &summary)
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

func processOne(ctx context.Context, store Store, sink Sink, obj storage.BatchObject, prefix string, summary *Summary) {
	albumID := albumIDFromContent(obj.ETag, obj.Size)
	dstKey := pipeline.SourceKey(albumID)
	originalFilename := strings.TrimPrefix(obj.Key, prefix)

	exists, _, err := store.HeadObject(ctx, dstKey)
	if err != nil {
		summary.Errors++
		log.Printf("batch ingest: ERROR head key=%s album_id=%s size=%d err=%v", obj.Key, albumID, obj.Size, err)
		return
	}

	if !exists {
		if err := store.CopyObject(ctx, obj.Key, dstKey); err != nil {
			summary.Errors++
			log.Printf("batch ingest: ERROR copy key=%s dst=%s album_id=%s size=%d err=%v (original kept for retry)", obj.Key, dstKey, albumID, obj.Size, err)
			return
		}
		summary.Moved++
	} else {
		summary.Deduped++
	}

	if sink != nil {
		if err := sink.RegisterStagedUpload(ctx, albumID, originalFilename, obj.Size, dstKey); err != nil {
			summary.Errors++
			log.Printf("batch ingest: ERROR register key=%s dst=%s album_id=%s size=%d err=%v (staged zip kept)", obj.Key, dstKey, albumID, obj.Size, err)
			return
		}
	}

	if err := store.DeleteObject(ctx, obj.Key); err != nil {
		summary.Errors++
		log.Printf("batch ingest: ERROR delete-src key=%s dst=%s album_id=%s size=%d err=%v (copied but original left behind)", obj.Key, dstKey, albumID, obj.Size, err)
		return
	}

	log.Printf("batch ingest: STAGED key=%s dst=%s album_id=%s size=%d original_filename=%s", obj.Key, dstKey, albumID, obj.Size, originalFilename)
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
