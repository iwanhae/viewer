package storage

import (
	"bytes"
	"context"
	"encoding/json"
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

func (s *S3Store) CreateMultipartUpload(ctx context.Context, key string, contentType string) (string, error) {
	input := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if strings.TrimSpace(contentType) != "" {
		input.ContentType = aws.String(contentType)
	}

	out, err := s.client.CreateMultipartUpload(ctx, input)
	if err != nil {
		return "", fmt.Errorf("create multipart upload %s: %w", key, err)
	}
	if out.UploadId == nil || strings.TrimSpace(*out.UploadId) == "" {
		return "", fmt.Errorf("create multipart upload %s: missing upload id", key)
	}
	return *out.UploadId, nil
}

func (s *S3Store) PresignUploadPart(ctx context.Context, key string, uploadID string, partNumber int32, ttl time.Duration) (string, map[string]string, error) {
	if strings.TrimSpace(uploadID) == "" {
		return "", nil, fmt.Errorf("upload id is required")
	}
	if partNumber <= 0 {
		return "", nil, fmt.Errorf("part number must be > 0")
	}

	out, err := s.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, fmt.Errorf("presign upload part %s part=%d: %w", key, partNumber, err)
	}
	return out.URL, map[string]string{}, nil
}

func (s *S3Store) ListMultipartUploadParts(ctx context.Context, key string, uploadID string) ([]types.CompletedPart, error) {
	if strings.TrimSpace(uploadID) == "" {
		return nil, fmt.Errorf("upload id is required")
	}

	parts := make([]types.CompletedPart, 0, 64)
	var marker *string
	for {
		out, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(s.bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, fmt.Errorf("list multipart parts %s: %w", key, err)
		}
		for _, part := range out.Parts {
			if part.PartNumber == nil || part.ETag == nil {
				continue
			}
			parts = append(parts, types.CompletedPart{
				ETag:       aws.String(*part.ETag),
				PartNumber: aws.Int32(*part.PartNumber),
			})
		}

		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		marker = out.NextPartNumberMarker
	}

	sort.Slice(parts, func(i, j int) bool {
		return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber)
	})
	return parts, nil
}

func (s *S3Store) CompleteMultipartUpload(ctx context.Context, key string, uploadID string, parts []types.CompletedPart) error {
	if strings.TrimSpace(uploadID) == "" {
		return fmt.Errorf("upload id is required")
	}
	if len(parts) == 0 {
		return fmt.Errorf("at least one uploaded part is required")
	}

	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return fmt.Errorf("complete multipart upload %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) AbortMultipartUpload(ctx context.Context, key string, uploadID string) error {
	if strings.TrimSpace(uploadID) == "" {
		return fmt.Errorf("upload id is required")
	}

	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("abort multipart upload %s: %w", key, err)
	}
	return nil
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
	return s.PutObject(ctx, key, bytes.NewReader(b), "application/json")
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
	body, _, err := s.GetObject(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()

	if err := json.NewDecoder(body).Decode(out); err != nil {
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

func (s *S3Store) ListAlbumIndexKeys(ctx context.Context) ([]string, error) {
	keys := make([]string, 0, 128)
	if err := s.ForEachAlbumIndexKey(ctx, func(key string) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return nil, err
	}
	return keys, nil
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
