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
	return s.PutObject(ctx, key, bytes.NewReader(b), "application/json")
}

func (s *S3Store) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, "", fmt.Errorf("get object %s: %w", key, err)
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
		var noSuch *types.NotFound
		if errors.As(err, &noSuch) || strings.Contains(strings.ToLower(err.Error()), "not found") {
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
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String("albums/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range out.Contents {
			if obj.Key == nil {
				continue
			}
			if strings.HasSuffix(*obj.Key, "/index.json") {
				keys = append(keys, *obj.Key)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}
