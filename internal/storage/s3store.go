package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type S3Store struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
}

func NewS3Store(ctx context.Context, cfg cfgpkg.Config) (*S3Store, error) {
	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(cfg.S3Region),
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
		o.UsePathStyle = cfg.S3UsePathStyle
	})

	return &S3Store{
		bucket:    cfg.S3Bucket,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (s *S3Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error) {
	out, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, fmt.Errorf("presign put: %w", err)
	}
	return out.URL, map[string]string{}, nil
}

func (s *S3Store) PutObject(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) PutJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(b),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("put json %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
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

func (s *S3Store) GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error) {
	if start < 0 {
		return nil, "", fmt.Errorf("invalid range start: %d", start)
	}
	if end < start {
		return nil, "", fmt.Errorf("invalid range end: %d", end)
	}

	rangeValue := fmt.Sprintf("bytes=%d-%d", start, end)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeValue),
	})
	if err != nil {
		return nil, "", wrapObjectError("get object range", key, err)
	}

	if out.ContentRange == nil || !strings.HasPrefix(*out.ContentRange, fmt.Sprintf("bytes %d-%d/", start, end)) {
		out.Body.Close()
		return nil, "", fmt.Errorf("range response mismatch for %s: expected bytes %d-%d, got %q", key, start, end, aws.ToString(out.ContentRange))
	}
	if out.ContentLength == nil || *out.ContentLength != (end-start+1) {
		out.Body.Close()
		return nil, "", fmt.Errorf("range response length mismatch for %s: expected %d, got %d", key, end-start+1, aws.ToInt64(out.ContentLength))
	}
	if out.AcceptRanges != nil {
		accept := strings.ToLower(strings.TrimSpace(*out.AcceptRanges))
		if accept != "" && accept != "bytes" {
			out.Body.Close()
			return nil, "", fmt.Errorf("range response does not advertise byte ranges for %s: %q", key, *out.AcceptRanges)
		}
	}
	if out.ContentType == nil {
		out.ContentType = aws.String("application/octet-stream")
	}
	if out.ContentLength != nil && *out.ContentLength <= 0 {
		out.Body.Close()
		return nil, "", fmt.Errorf("range response empty for %s", key)
	}
	if out.DeleteMarker != nil && *out.DeleteMarker {
		out.Body.Close()
		return nil, "", fmt.Errorf("range response points to delete marker for %s", key)
	}
	ct := "application/octet-stream"
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return out.Body, ct, nil
}

func (s *S3Store) ReadJSON(ctx context.Context, key string, out any) error {
	getOut, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return wrapObjectError("get object", key, err)
	}
	defer getOut.Body.Close()

	if err := json.NewDecoder(getOut.Body).Decode(out); err != nil {
		return fmt.Errorf("decode json %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) HeadObject(ctx context.Context, key string) (bool, int64, error) {
	o, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
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

func (s *S3Store) ForEachAlbumIndexKey(ctx context.Context, fn func(key string) error) error {
	if fn == nil {
		return nil
	}
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String("albums/"),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range out.Contents {
			if obj.Key == nil {
				continue
			}
			if !strings.HasSuffix(*obj.Key, "/index.json") {
				continue
			}
			if err := fn(*obj.Key); err != nil {
				return err
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return nil
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
