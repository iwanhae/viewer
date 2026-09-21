package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	cfgpkg "viewer/internal/config"
)

// S3Store is the object-storage boundary. Every key that crosses it is logical:
// callers pass and receive keys like "blobs/<hash>" while the store itself adds
// the configured key prefix. Keeping the prefix out of the catalog means moving
// a deployment's objects within the bucket never invalidates stored metadata,
// and it keeps listing results in the same namespace callers pass back in.
type S3Store struct {
	bucket    string
	prefix    string
	client    *s3.Client
	presigner *s3.PresignClient
}

// Object is one entry of a bucket listing.
type Object struct {
	Key          string
	LastModified time.Time
	Size         int64
	ETag         string
}

func NewS3Store(ctx context.Context, cfg cfgpkg.Config) (*S3Store, error) {
	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(cfgpkg.S3Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	// BaseEndpoint keeps the operator's endpoint verbatim, including any path
	// prefix it carries, and leaves UsePathStyle to place the bucket: the first
	// path segment when true, a subdomain of the endpoint when false.
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.S3UsePathStyle
		o.BaseEndpoint = aws.String(cfg.S3Endpoint)
	})

	return &S3Store{
		bucket:    cfg.S3Bucket,
		prefix:    cfg.S3Prefix,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

// physicalKey maps a logical key to the key stored in the bucket.
func (s *S3Store) physicalKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + key
}

// logicalKey is the inverse of physicalKey. Callers only ever see logical keys,
// so a listed object can be fed straight back into CopyObject or DeleteObject.
func (s *S3Store) logicalKey(key string) string {
	return strings.TrimPrefix(key, s.prefix)
}

func (s *S3Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error) {
	out, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.physicalKey(key)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, fmt.Errorf("presign put: %w", err)
	}
	return out.URL, map[string]string{}, nil
}

func (s *S3Store) PutObject(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.physicalKey(key)),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.physicalKey(key)),
	})
	if err != nil {
		return nil, "", wrapObjectError("get object", key, err)
	}
	ct := "application/octet-stream"
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return out.Body, ct, nil
}

func (s *S3Store) HeadObject(ctx context.Context, key string) (bool, int64, error) {
	o, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.physicalKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("head object %s: %w", key, err)
	}
	var size int64
	if o.ContentLength != nil {
		size = *o.ContentLength
	}
	return true, size, nil
}

// StatObject reports one object's listing metadata, the way ListObjects
// reports it. A missing object is (Object{}, false, nil), like HeadObject.
func (s *S3Store) StatObject(ctx context.Context, key string) (Object, bool, error) {
	o, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.physicalKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return Object{}, false, nil
		}
		return Object{}, false, fmt.Errorf("stat object %s: %w", key, err)
	}
	return Object{
		Key:          key,
		LastModified: aws.ToTime(o.LastModified).UTC(),
		Size:         aws.ToInt64(o.ContentLength),
		ETag:         aws.ToString(o.ETag),
	}, true, nil
}

func (s *S3Store) DeleteObject(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("key is required")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.physicalKey(key)),
	})
	if err != nil {
		return fmt.Errorf("delete object %s: %w", key, err)
	}
	return nil
}

// maxDeleteObjectsPerRequest is the DeleteObjects API's per-request key limit.
const maxDeleteObjectsPerRequest = 1000

// DeleteObjects deletes logical keys in batches of 1000. Deleting a key that
// does not exist is not an error. Per-key failures are collected and reported
// with their logical keys; the batches that succeeded stay deleted.
func (s *S3Store) DeleteObjects(ctx context.Context, keys []string) error {
	requested := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key != "" {
			requested = append(requested, key)
		}
	}
	if len(requested) == 0 {
		return nil
	}

	var failures []string
	for start := 0; start < len(requested); start += maxDeleteObjectsPerRequest {
		end := start + maxDeleteObjectsPerRequest
		if end > len(requested) {
			end = len(requested)
		}
		chunk := requested[start:end]
		ids := make([]types.ObjectIdentifier, 0, len(chunk))
		for _, key := range chunk {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(s.physicalKey(key))})
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{
				Objects: ids,
				// Quiet keeps the response to failures only, so anything that
				// comes back in Errors is a real problem.
				Quiet: aws.Bool(true),
			},
		})
		if err != nil {
			return fmt.Errorf("delete objects: %w", err)
		}
		for _, delErr := range out.Errors {
			failures = append(failures, fmt.Sprintf(
				"%s: %s",
				s.logicalKey(aws.ToString(delErr.Key)),
				strings.TrimSpace(aws.ToString(delErr.Message)+" ("+aws.ToString(delErr.Code)+")"),
			))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("delete objects failed for %d key(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// ListObjects lists every object under a logical prefix. It is the one method
// that reads keys out of the bucket, so it strips the store's key prefix again:
// callers get a listing in the same logical namespace they pass to the other
// methods.
func (s *S3Store) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, fmt.Errorf("prefix is required")
	}
	listPrefix := s.physicalKey(prefix)

	objects := make([]Object, 0)
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(listPrefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range out.Contents {
			key := s.logicalKey(aws.ToString(obj.Key))
			if key == "" {
				continue
			}
			objects = append(objects, Object{
				Key:          key,
				LastModified: aws.ToTime(obj.LastModified).UTC(),
				Size:         aws.ToInt64(obj.Size),
				ETag:         aws.ToString(obj.ETag),
			})
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})
	return objects, nil
}

func wrapObjectError(action string, key string, err error) error {
	if isS3NotFound(err) {
		return fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	return fmt.Errorf("%s %s: %w", action, key, err)
}

func isS3NotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}
