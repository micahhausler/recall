package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/version"
)

func init() {
	Register("s3", func(ctx context.Context, s config.StoreSettings) (Store, error) {
		client, bucket, err := S3Client(ctx, s.S3)
		if err != nil {
			return nil, err
		}
		return NewS3(client, bucket), nil
	})
}

// NamespacedBucket is the account regional namespace name used when
// store.s3.bucket is empty: only this account can create names with the -an
// suffix, and the account ID is derived at run time, never written to config.
func NamespacedBucket(prefix, account, region string) string {
	return fmt.Sprintf("%s-%s-%s-an", prefix, account, region)
}

// STSAPI is the one STS call bucket resolution needs; sts.Client satisfies it.
type STSAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// ResolveBucket returns the configured bucket, or the namespaced name derived
// from the caller's account.
func ResolveBucket(ctx context.Context, c config.S3Settings, api STSAPI) (string, error) {
	if c.Bucket != "" {
		return c.Bucket, nil
	}
	if c.Region == "" {
		return "", errors.New("store.s3.region is unset and store.s3.bucket is empty; set one with recall config set")
	}
	out, err := api.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts get-caller-identity: %w", err)
	}
	return NamespacedBucket(c.BucketPrefix, aws.ToString(out.Account), c.Region), nil
}

// AWSConfig loads the default credential chain with the configured profile
// and region. Every call identifies itself: the SDK appends app/<id> to the
// User-Agent, so access logs and CloudTrail say recall made the request.
func AWSConfig(ctx context.Context, c config.S3Settings) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithAppID(version.UserAgentAppID()),
	}
	if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// S3Client builds the S3 client and resolves the bucket name.
func S3Client(ctx context.Context, c config.S3Settings) (*s3.Client, string, error) {
	cfg, err := AWSConfig(ctx, c)
	if err != nil {
		return nil, "", err
	}
	bucket, err := ResolveBucket(ctx, c, sts.NewFromConfig(cfg))
	if err != nil {
		return nil, "", err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
			o.UsePathStyle = true
		}
	}), bucket, nil
}

// S3Store stores objects in one bucket; uploads use If-None-Match, so an
// existing object is never overwritten.
type S3Store struct {
	client *s3.Client
	bucket string
}

// NewS3 wraps a client and bucket as a Store.
func NewS3(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

// Bucket returns the resolved bucket name.
func (s *S3Store) Bucket() string { return s.bucket }

func (s *S3Store) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	// Buffered so the SDK can sign and retry; weekly archives are small.
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		IfNoneMatch: aws.String("*"),
		ContentType: aws.String("application/zstd"),
	})
	if isCode(err, "PreconditionFailed") {
		return fmt.Errorf("%s: %w", key, ErrExists)
	}
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

func (s *S3Store) Head(ctx context.Context, key string) (Info, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return Info{}, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: aws.ToInt64(out.ContentLength)}, nil
}

func isCode(err error, code string) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == code
}
