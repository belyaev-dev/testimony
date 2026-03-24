package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"github.com/testimony-dev/testimony/internal/config"
)

type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

type DownloadResult struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
}

var ErrObjectNotFound = errors.New("object not found")

type Store interface {
	EnsureBucket(ctx context.Context) error
	Ready(ctx context.Context) error
	Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	Download(ctx context.Context, key string) (DownloadResult, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

type S3Store struct {
	client *s3.Client
	bucket string
	region string
	logger *slog.Logger
}

func NewS3Store(ctx context.Context, cfg config.S3Config, logger *slog.Logger) (*S3Store, error) {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("new s3 store: empty endpoint")
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("new s3 store: parse endpoint %q: %w", endpoint, err)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(strings.TrimSpace(cfg.Region)),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			strings.TrimSpace(cfg.AccessKeyID),
			strings.TrimSpace(cfg.SecretAccessKey),
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.UsePathStyle
		options.BaseEndpoint = aws.String(endpoint)
	})

	store := &S3Store{
		client: client,
		bucket: strings.TrimSpace(cfg.Bucket),
		region: strings.TrimSpace(cfg.Region),
		logger: logger,
	}
	if store.bucket == "" {
		return nil, fmt.Errorf("new s3 store: empty bucket")
	}
	if store.region == "" {
		return nil, fmt.Errorf("new s3 store: empty region")
	}

	if err := store.EnsureBucket(ctx); err != nil {
		logger.Error("s3 bucket check failed",
			"bucket", store.bucket,
			"error", err,
		)
		return nil, err
	}

	logger.Info("s3 store ready",
		"bucket", store.bucket,
		"endpoint", endpoint,
		"use_path_style", cfg.UsePathStyle,
	)

	return store, nil
}

func (s *S3Store) EnsureBucket(ctx context.Context) error {
	if err := s.Ready(ctx); err == nil {
		return nil
	} else {
		createErr := s.createBucket(ctx)
		if createErr == nil || isBucketExistsError(createErr) {
			return nil
		}
		return fmt.Errorf("ensure bucket %q: %v; create bucket: %w", s.bucket, err, createErr)
	}
}

func (s *S3Store) Ready(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("head bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *S3Store) Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	trimmedKey := strings.TrimSpace(key)
	if trimmedKey == "" {
		return fmt.Errorf("upload object: empty key")
	}
	if body == nil {
		return fmt.Errorf("upload object %q: nil body", trimmedKey)
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(trimmedKey),
		Body:   body,
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	if trimmedType := strings.TrimSpace(contentType); trimmedType != "" {
		input.ContentType = aws.String(trimmedType)
	}

	if _, err := s.client.PutObject(ctx, input); err != nil {
		s.logger.Error("s3 upload failed",
			"bucket", s.bucket,
			"object_key", trimmedKey,
			"error", err,
		)
		return fmt.Errorf("upload object %q: %w", trimmedKey, err)
	}

	return nil
}

func (s *S3Store) Download(ctx context.Context, key string) (DownloadResult, error) {
	trimmedKey := strings.TrimSpace(key)
	if trimmedKey == "" {
		return DownloadResult{}, fmt.Errorf("download object: empty key")
	}

	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(trimmedKey),
	})
	if err != nil {
		if isObjectNotFoundError(err) {
			return DownloadResult{}, fmt.Errorf("download object %q: %w", trimmedKey, ErrObjectNotFound)
		}
		return DownloadResult{}, fmt.Errorf("download object %q: %w", trimmedKey, err)
	}

	return DownloadResult{
		Body:          result.Body,
		ContentType:   strings.TrimSpace(aws.ToString(result.ContentType)),
		ContentLength: aws.ToInt64(result.ContentLength),
	}, nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	}
	if trimmedPrefix := strings.TrimSpace(prefix); trimmedPrefix != "" {
		input.Prefix = aws.String(trimmedPrefix)
	}

	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	objects := make([]ObjectInfo, 0)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects with prefix %q: %w", prefix, err)
		}
		for _, object := range page.Contents {
			objects = append(objects, ObjectInfo{
				Key:          aws.ToString(object.Key),
				Size:         aws.ToInt64(object.Size),
				ETag:         aws.ToString(object.ETag),
				LastModified: aws.ToTime(object.LastModified).UTC(),
			})
		}
	}

	return objects, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	trimmedKey := strings.TrimSpace(key)
	if trimmedKey == "" {
		return fmt.Errorf("delete object: empty key")
	}

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(trimmedKey),
	}); err != nil {
		return fmt.Errorf("delete object %q: %w", trimmedKey, err)
	}

	return nil
}

func (s *S3Store) createBucket(ctx context.Context) error {
	input := &s3.CreateBucketInput{Bucket: aws.String(s.bucket)}
	if s.region != "" && s.region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(s.region),
		}
	}

	if _, err := s.client.CreateBucket(ctx, input); err != nil {
		return fmt.Errorf("create bucket %q: %w", s.bucket, err)
	}
	return nil
}

func isObjectNotFoundError(err error) bool {
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}

	return false
}

func isBucketExistsError(err error) bool {
	var ownedByYou *s3types.BucketAlreadyOwnedByYou
	if errors.As(err, &ownedByYou) {
		return true
	}

	var alreadyExists *s3types.BucketAlreadyExists
	if errors.As(err, &alreadyExists) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}

	return false
}
