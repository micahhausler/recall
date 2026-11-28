package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"

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

// BucketPrefix starts the bucket name in the account regional namespace.
const BucketPrefix = "chat-archive"

// NamespacedBucket is the account regional namespace name for an account and
// region: only that account can create names with the -an suffix.
func NamespacedBucket(account, region string) string {
	return fmt.Sprintf("%s-%s-%s-an", BucketPrefix, account, region)
}

var namespacedName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*-[0-9]{12}-(.+)-an$`)

// InNamespace reports a bucket name in the account regional namespace form
// for region: <prefix>-<account id>-<region>-an.
func InNamespace(bucket, region string) bool {
	m := namespacedName.FindStringSubmatch(bucket)
	return m != nil && region != "" && m[1] == region
}

// STSAPI is the one STS call the archive check makes; sts.Client satisfies it.
type STSAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
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

// S3Client builds the S3 client for the configured bucket; it sends nothing.
func S3Client(ctx context.Context, c config.S3Settings) (*s3.Client, string, error) {
	if c.Bucket == "" {
		return nil, "", errors.New("store.s3.bucket is unset; name the bucket with recall config set store.s3.bucket <name>")
	}
	cfg, err := AWSConfig(ctx, c)
	if err != nil {
		return nil, "", err
	}
	bucket := c.Bucket
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

// Bucket returns the bucket name.
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

// Put replaces the object at key: the same upload without the absence
// condition.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
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

// Delete removes the object at key; S3 reports success for a missing one.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
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
