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
// and it keeps the batch scanner's keys in the same namespace it lists.
type S3Store struct {
	bucket    string
	prefix    string
	client    *s3.Client
	presigner *s3.PresignClient
}

type BatchObject struct {
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

	awsCfg.EndpointResolverWithOptions = aws.EndpointResolverWithOptionsFunc(
		func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			if service == s3.ServiceID {
				return aws.Endpoint{URL: cfg.S3Endpoint, HostnameImmutable: true}, nil
			}
			return aws.Endpoint{}, &aws.EndpointNotFoundError{}
		},
	)

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfgpkg.S3UsePathStyle
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

func (s *S3Store) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	srcKey = strings.TrimSpace(srcKey)
	dstKey = strings.TrimSpace(dstKey)
	if srcKey == "" || dstKey == "" {
		return fmt.Errorf("srcKey and dstKey are required")
	}
	if srcKey == dstKey {
		return fmt.Errorf("srcKey and dstKey must differ")
	}
	copySource := fmt.Sprintf("%s/%s", s.bucket, s.physicalKey(srcKey))
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		CopySource: aws.String(copySource),
		Key:        aws.String(s.physicalKey(dstKey)),
	})
	if err != nil {
		return fmt.Errorf("copy object %s -> %s: %w", srcKey, dstKey, err)
	}
	return nil
}

// ListBatchObjects lists the batch prefix. It is the one method that reads keys
// out of the bucket, so it strips the store's key prefix again: callers get a
// listing in the same logical namespace they pass to the other methods.
func (s *S3Store) ListBatchObjects(ctx context.Context, prefix string) ([]BatchObject, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, fmt.Errorf("prefix is required")
	}
	listPrefix := s.physicalKey(prefix)

	objects := make([]BatchObject, 0)
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(listPrefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list batch objects: %w", err)
		}
		for _, obj := range out.Contents {
			key := s.logicalKey(aws.ToString(obj.Key))
			if key == "" {
				continue
			}
			objects = append(objects, BatchObject{
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
