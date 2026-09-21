// Package ingest adopts the zips that appear in the upload prefix.
//
// A client normally uploads through POST /api/albums, which creates the album
// row before the bytes arrive and queues it when the transfer finishes.
// Anything else that lands under "uploads/" - a zip copied in by an operator or
// an external tool - has no album row, so this scan creates one and queues it.
// Both paths use the same directory, so the bucket has exactly one staging
// prefix; the backup package empties it in batches once a drain has backed the
// catalog up.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"path"
	"strings"

	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

// albumIDHashLen sizes the album id derived from an object's content. Sixteen
// hex characters is far beyond collision range for a photo library and keeps the
// id short in URLs.
const albumIDHashLen = 16

// Store is the object-storage surface the scan needs.
type Store interface {
	ListObjects(ctx context.Context, prefix string) ([]storage.Object, error)
}

// Sink is the catalog surface the scan needs.
type Sink interface {
	// AlbumForStagedObject returns the album that already owns a staged object,
	// if there is one.
	AlbumForStagedObject(ctx context.Context, sourceKey string) (*catalog.Album, bool, error)
	// RegisterStagedUpload records an album for a staged zip and queues it.
	RegisterStagedUpload(ctx context.Context, albumID, filename string, sizeBytes int64, sourceKey string) error
	// EnqueueStaged queues an album whose staged zip is still waiting.
	EnqueueStaged(ctx context.Context, albumID string) error
}

// Summary counts what one scan did.
type Summary struct {
	Discovered int
	Registered int
	Requeued   int
	Skipped    int
	Errors     int
}

// Run lists the upload prefix and makes sure every zip under it is queued for
// extraction, registering an album for the ones that have none. Nothing is
// copied, moved or deleted: the backup finalizer removes a staged zip after a
// drain, so a scan is safe to repeat.
func Run(ctx context.Context, store Store, sink Sink) (Summary, error) {
	var summary Summary

	objects, err := store.ListObjects(ctx, pipeline.UploadPrefix)
	if err != nil {
		return summary, fmt.Errorf("list uploads: %w", err)
	}

	candidates := stagedZips(objects)
	summary.Discovered = len(candidates)
	log.Printf(
		"upload ingest: prefix=%s listed=%d candidates=%d",
		pipeline.UploadPrefix, len(objects), len(candidates),
	)

	for _, obj := range candidates {
		if err := ctx.Err(); err != nil {
			log.Printf("upload ingest: cancelled at key=%s", obj.Key)
			return summary, err
		}
		adopt(ctx, sink, obj, &summary)
	}

	return summary, nil
}

// stagedZips keeps the objects that could hold an album: a zip with content.
func stagedZips(objects []storage.Object) []storage.Object {
	out := make([]storage.Object, 0, len(objects))
	for _, obj := range objects {
		if !strings.HasSuffix(strings.ToLower(obj.Key), ".zip") {
			continue
		}
		if obj.Size <= 0 {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// adopt decides what one listed object needs. An object an album already owns is
// either left alone - its album is done, failed, or being extracted right now -
// or queued again, because a status of QUEUED or PROCESSING with the zip still
// in the bucket means no worker holds it. An object nobody owns becomes an album
// of its own, keyed by its content so the same zip dropped twice is one album.
func adopt(ctx context.Context, sink Sink, obj storage.Object, summary *Summary) {
	album, found, err := sink.AlbumForStagedObject(ctx, obj.Key)
	if err != nil {
		summary.Errors++
		log.Printf("upload ingest: ERROR lookup key=%s size=%d err=%v", obj.Key, obj.Size, err)
		return
	}
	if found {
		switch album.Status {
		case catalog.AlbumStatusQueued, catalog.AlbumStatusProcessing:
			if err := sink.EnqueueStaged(ctx, album.ID); err != nil {
				summary.Errors++
				log.Printf("upload ingest: ERROR enqueue key=%s album_id=%s err=%v", obj.Key, album.ID, err)
				return
			}
			summary.Requeued++
			log.Printf("upload ingest: requeued key=%s album_id=%s status=%s", obj.Key, album.ID, album.Status)
		default:
			// A terminal status: the zip waits for the next drain-time batch
			// delete, which only ever runs between scans on the pipeline
			// worker, so nothing removes it mid-extraction.
			summary.Skipped++
		}
		return
	}

	albumID := albumIDFromContent(obj.ETag, obj.Size)
	if err := sink.RegisterStagedUpload(ctx, albumID, path.Base(obj.Key), obj.Size, obj.Key); err != nil {
		summary.Errors++
		log.Printf("upload ingest: ERROR register key=%s album_id=%s size=%d err=%v", obj.Key, albumID, obj.Size, err)
		return
	}
	summary.Registered++
	log.Printf("upload ingest: registered key=%s album_id=%s size=%d", obj.Key, albumID, obj.Size)
}

// albumIDFromContent derives an album id from the object's ETag and size, which
// together identify the bytes without downloading them. Dropping identical bytes
// again therefore resolves to the same album instead of creating a second one.
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
