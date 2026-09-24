// Package encoding coordinates external WebP workers and commits their staged
// output to the original blob key only after server-side validation.
package encoding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
	"viewer/internal/catalog"
	"viewer/internal/config"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

const LeaseTTL = 10 * time.Minute
const DefaultClaim = 8
const MaxClaim = 64

var ErrLostLease = errors.New("encoding lease is no longer current")
var ErrInvalidOutput = errors.New("invalid encoded output")
var ErrInvalidOutcome = errors.New("invalid encoding outcome")

type Store interface {
	GetObject(context.Context, string) (io.ReadCloser, string, error)
	PresignGet(context.Context, string, time.Duration) (string, error)
	PresignPut(context.Context, string, time.Duration) (string, error)
	StatObject(context.Context, string) (storage.Object, bool, error)
	CopyObjectIfMatch(context.Context, string, string, string, string) error
	DeleteObjects(context.Context, []string) error
	ListObjects(context.Context, string) ([]storage.Object, error)
}

type Service struct {
	cat         *catalog.Store
	store       Store
	commitSlots chan struct{}
}

func New(cat *catalog.Store, store Store) *Service {
	return &Service{cat: cat, store: store, commitSlots: make(chan struct{}, 2)}
}

type Claimed struct {
	Hash        string    `json:"hash"`
	SizeBytes   int64     `json:"sizeBytes"`
	ContentType string    `json:"contentType"`
	Token       string    `json:"token"`
	GetURL      string    `json:"getUrl"`
	PutURL      string    `json:"putUrl"`
	LeaseUntil  time.Time `json:"leaseUntil"`
}

func (s *Service) Claim(ctx context.Context, limit int) ([]Claimed, error) {
	if limit <= 0 {
		limit = DefaultClaim
	} else if limit > MaxClaim {
		limit = MaxClaim
	}
	jobs, err := s.cat.ClaimEncoding(ctx, limit, LeaseTTL)
	if err != nil {
		return nil, err
	}
	claimed := make([]Claimed, 0, len(jobs))
	for _, job := range jobs {
		getURL, getErr := s.store.PresignGet(ctx, pipeline.BlobKey(job.Hash), config.PresignTTL)
		putURL, putErr := s.store.PresignPut(ctx, job.StageKey, config.PresignTTL)
		if getErr != nil || putErr != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for _, release := range jobs {
				_ = s.cat.ReleaseEncoding(releaseCtx, release.Hash, release.Token)
			}
			cancel()
			return nil, fmt.Errorf("presign encoding job: get=%v put=%v", getErr, putErr)
		}
		claimed = append(claimed, Claimed{job.Hash, job.SizeBytes, job.Type, job.Token, getURL, putURL, job.LeaseUntil})
	}
	return claimed, nil
}

func (s *Service) Renew(ctx context.Context, hash, token string) (time.Time, string, error) {
	until, ok, err := s.cat.RenewEncoding(ctx, hash, token, LeaseTTL)
	if err != nil {
		return time.Time{}, "", err
	}
	if !ok {
		return time.Time{}, "", ErrLostLease
	}
	job, err := s.cat.GetEncodingJob(ctx, hash)
	if err != nil {
		return time.Time{}, "", err
	}
	url, err := s.store.PresignPut(ctx, job.StageKey, config.PresignTTL)
	return until, url, err
}

func readObject(ctx context.Context, store Store, key string) ([]byte, string, error) {
	reader, contentType, err := store.GetObject(ctx, key)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	return data, contentType, err
}

func isWebP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

// Complete validates a worker result. "uploaded" is the only result that
// changes the object; the others terminate the work without storage writes.
func (s *Service) Complete(ctx context.Context, hash, token, outcome, detail string) (string, error) {
	job, err := s.cat.GetEncodingJob(ctx, hash)
	if err != nil {
		return "", err
	}
	if token == "" || job.Token != token {
		return "", ErrLostLease
	}
	if job.Status == "done" || job.Status == "skipped" || job.Status == "failed" {
		return job.Status, nil
	}
	if job.Status != "leased" || time.Now().After(job.LeaseUntil) {
		return "", ErrLostLease
	}
	key := pipeline.BlobKey(hash)
	if outcome != "uploaded" {
		if outcome != "already_webp" && outcome != "not_smaller" && outcome != "failed" {
			return "", fmt.Errorf("%w: %q", ErrInvalidOutcome, outcome)
		}
		data, _, err := readObject(ctx, s.store, key)
		if err != nil {
			return "", err
		}
		status, contentType := "skipped", job.Type
		if isWebP(data) {
			contentType = "image/webp"
		}
		if outcome == "already_webp" && !isWebP(data) {
			return "", ErrInvalidOutput
		}
		if outcome == "failed" {
			status = "failed"
		}
		if len(detail) > 512 {
			detail = detail[:512]
		}
		ok, err := s.cat.SkipEncoding(ctx, hash, token, status, contentType, detail, int64(len(data)))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrLostLease
		}
		return status, nil
	}
	// Verification reads and decodes two full images. Bound concurrent commit
	// memory even when many external workers finish at the same time.
	select {
	case s.commitSlots <- struct{}{}:
		defer func() { <-s.commitSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	originalInfo, exists, err := s.store.StatObject(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("original blob missing: %s", hash)
	}
	original, _, err := readObject(ctx, s.store, key)
	if err != nil {
		return "", err
	}
	if isWebP(original) {
		ok, err := s.cat.SkipEncoding(ctx, hash, token, "done", "image/webp", "", int64(len(original)))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrLostLease
		}
		return "done", nil
	}
	sum := sha256.Sum256(original)
	if hex.EncodeToString(sum[:]) != hash {
		return "", fmt.Errorf("original bytes no longer match source hash %s", hash)
	}
	originalConfig, _, err := image.DecodeConfig(bytes.NewReader(original))
	if err != nil {
		return "", fmt.Errorf("decode original: %w", err)
	}
	stagedInfo, exists, err := s.store.StatObject(ctx, job.StageKey)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("staged output missing: %s", job.StageKey)
	}
	if stagedInfo.Size >= int64(len(original)) || stagedInfo.Size <= 0 {
		return "", ErrInvalidOutput
	}
	encoded, _, err := readObject(ctx, s.store, job.StageKey)
	if err != nil {
		return "", err
	}
	if !isWebP(encoded) || int64(len(encoded)) != stagedInfo.Size {
		return "", ErrInvalidOutput
	}
	encodedConfig, _, err := image.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || encodedConfig.Width != originalConfig.Width || encodedConfig.Height != originalConfig.Height {
		return "", ErrInvalidOutput
	}
	// A full decode catches truncated output whose header alone looks valid.
	if _, _, err := image.Decode(bytes.NewReader(encoded)); err != nil {
		return "", fmt.Errorf("decode WebP output: %w", err)
	}
	currentInfo, exists, err := s.store.StatObject(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists || currentInfo.ETag != originalInfo.ETag || currentInfo.Size != originalInfo.Size {
		return "", ErrLostLease
	}
	ok, err := s.cat.BeginEncodingCommit(ctx, hash, token)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrLostLease
	}
	if err := s.store.CopyObjectIfMatch(ctx, job.StageKey, key, stagedInfo.ETag, "image/webp"); err != nil {
		_ = s.cat.ResetCommittingEncoding(context.Background(), hash, token)
		return "", err
	}
	if _, err := s.cat.FinishEncoding(ctx, hash, token, "done", "image/webp", "", int64(len(encoded))); err != nil {
		return "", err
	}
	if err := s.store.DeleteObjects(ctx, []string{job.StageKey}); err != nil {
		log.Printf("encoding: delete staged %s: %v", job.StageKey, err)
	}
	return "done", nil
}

// Recover reconciles a crash between the copy and the catalog update. The
// object store decides the outcome; a source still in its old format retries.
func (s *Service) Recover(ctx context.Context) error {
	jobs, err := s.cat.CommittingEncodings(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		data, _, err := readObject(ctx, s.store, pipeline.BlobKey(job.Hash))
		if err != nil {
			return err
		}
		if isWebP(data) {
			if _, err := s.cat.FinishEncoding(ctx, job.Hash, job.Token, "done", "image/webp", "", int64(len(data))); err != nil {
				return err
			}
		} else if err := s.cat.ResetCommittingEncoding(ctx, job.Hash, job.Token); err != nil {
			return err
		}
	}
	return s.Cleanup(ctx)
}

// Cleanup removes abandoned staged outputs after their signed URLs and leases
// have expired. It is safe to run periodically while workers are active.
func (s *Service) Cleanup(ctx context.Context) error {
	objects, err := s.store.ListObjects(ctx, "encoding/")
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	keys := make([]string, 0)
	for _, object := range objects {
		if strings.HasPrefix(object.Key, "encoding/") && object.LastModified.Before(cutoff) {
			keys = append(keys, object.Key)
		}
	}
	return s.store.DeleteObjects(ctx, keys)
}
