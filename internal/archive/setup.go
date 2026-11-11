package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Admin is every S3 call setup and verify make; *s3.Client satisfies it.
// Tests use a fake, never real AWS.
type S3Admin interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutPublicAccessBlock(ctx context.Context, in *s3.PutPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
	PutBucketEncryption(ctx context.Context, in *s3.PutBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error)
	PutBucketPolicy(ctx context.Context, in *s3.PutBucketPolicyInput, opts ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
	PutBucketLifecycleConfiguration(ctx context.Context, in *s3.PutBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	GetPublicAccessBlock(ctx context.Context, in *s3.GetPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.GetPublicAccessBlockOutput, error)
	GetBucketEncryption(ctx context.Context, in *s3.GetBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error)
	GetBucketPolicy(ctx context.Context, in *s3.GetBucketPolicyInput, opts ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
	GetBucketLifecycleConfiguration(ctx context.Context, in *s3.GetBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	GetBucketOwnershipControls(ctx context.Context, in *s3.GetBucketOwnershipControlsInput, opts ...func(*s3.Options)) (*s3.GetBucketOwnershipControlsOutput, error)
}

// Setup creates and hardens the bucket, then verifies it; idempotent.
type Setup struct {
	API        aws3 // S3Admin
	Bucket     string
	Region     string
	Namespaced bool
	Retention  int
	Out        io.Writer
}

type aws3 = S3Admin

// tlsOnlyPolicy denies any request not made over TLS; its only
// Principal "*" is a Deny.
func tlsOnlyPolicy(bucket string) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Sid":       "DenyInsecureTransport",
			"Effect":    "Deny",
			"Principal": "*",
			"Action":    "s3:*",
			"Resource":  []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"},
			"Condition": map[string]any{"Bool": map[string]any{"aws:SecureTransport": "false"}},
		}},
	}
}

func (s *Setup) Run(ctx context.Context, dryRun bool) (int, error) {
	b := s.Bucket
	_, headErr := s.API.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b)})
	exists := headErr == nil

	type step struct {
		name string
		call func() error
	}
	var steps []step
	if !exists {
		steps = append(steps, step{"create-bucket", func() error {
			in := &s3.CreateBucketInput{
				Bucket:          aws.String(b),
				ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced,
			}
			if s.Namespaced {
				in.BucketNamespace = types.BucketNamespaceAccountRegional
			}
			if s.Region != "us-east-1" {
				in.CreateBucketConfiguration = &types.CreateBucketConfiguration{
					LocationConstraint: types.BucketLocationConstraint(s.Region),
				}
			}
			_, err := s.API.CreateBucket(ctx, in)
			return err
		}})
	}
	steps = append(steps,
		step{"put-public-access-block", func() error {
			_, err := s.API.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
				Bucket:                         aws.String(b),
				PublicAccessBlockConfiguration: wantPAB(),
			})
			return err
		}},
		step{"put-bucket-encryption", func() error {
			_, err := s.API.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
				Bucket: aws.String(b),
				ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
					Rules: []types.ServerSideEncryptionRule{{
						ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
							SSEAlgorithm: types.ServerSideEncryptionAes256,
						},
					}},
				},
			})
			return err
		}},
		step{"put-bucket-policy", func() error {
			policy, _ := json.Marshal(tlsOnlyPolicy(b))
			_, err := s.API.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
				Bucket: aws.String(b),
				Policy: aws.String(string(policy)),
			})
			return err
		}},
		step{"put-bucket-lifecycle-configuration", func() error {
			_, err := s.API.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
				Bucket:                 aws.String(b),
				LifecycleConfiguration: wantLifecycle(s.Retention),
			})
			return err
		}},
	)
	fmt.Fprintf(s.Out, "bucket: s3://%s  region: %s\n", b, s.Region)
	for _, st := range steps {
		if dryRun {
			fmt.Fprintf(s.Out, "would run: %s\n", st.name)
			continue
		}
		if err := st.call(); err != nil {
			return 2, fmt.Errorf("%s: %w", st.name, err)
		}
		fmt.Fprintf(s.Out, "ok: %s\n", st.name)
	}
	if dryRun {
		return 0, nil
	}
	if exists {
		fmt.Fprintln(s.Out, "the bucket already existed; its settings were re-applied")
	}
	return s.verify(ctx)
}

func wantPAB() *types.PublicAccessBlockConfiguration {
	t := true
	return &types.PublicAccessBlockConfiguration{
		BlockPublicAcls: &t, IgnorePublicAcls: &t, BlockPublicPolicy: &t, RestrictPublicBuckets: &t,
	}
}

func wantLifecycle(retention int) *types.BucketLifecycleConfiguration {
	days := int32(retention)
	seven := int32(7)
	return &types.BucketLifecycleConfiguration{
		Rules: []types.LifecycleRule{{
			ID:         aws.String("expire"),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("")},
			Expiration: &types.LifecycleExpiration{Days: &days},
			AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: &seven,
			},
		}},
	}
}

// verify reads every setting back; setup isn't done until they match.
func (s *Setup) verify(ctx context.Context) (int, error) {
	b := aws.String(s.Bucket)
	bad := 0
	report := func(name string, ok bool, got any) {
		status := "ok"
		if !ok {
			status = "FAIL"
			bad++
		}
		shown := got
		if ok {
			if _, isStr := got.(string); !isStr {
				shown = "matches"
			}
		}
		fmt.Fprintf(s.Out, "%-5s verify %s: %v\n", status, name, shown)
	}

	pab, err := s.API.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b})
	pabOK := err == nil && pabMatches(pab.PublicAccessBlockConfiguration)
	report("public access block", pabOK, errOr(err, matchWord(pabOK)))

	enc, err := s.API.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b})
	algo := ""
	if err == nil && len(enc.ServerSideEncryptionConfiguration.Rules) > 0 {
		algo = string(enc.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm)
	}
	report("encryption", algo == "AES256", errOr(err, algo))

	lc, err := s.API.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: b})
	days := 0
	if err == nil {
		for _, r := range lc.Rules {
			if aws.ToString(r.ID) == "expire" && r.Expiration != nil && r.Expiration.Days != nil {
				days = int(*r.Expiration.Days)
			}
		}
	}
	report("lifecycle expiration", days == s.Retention, errOr(err, fmt.Sprint(days)))

	pol, err := s.API.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	polOK := false
	if err == nil {
		var got map[string]any
		want, _ := json.Marshal(tlsOnlyPolicy(s.Bucket))
		var wantTree map[string]any
		json.Unmarshal(want, &wantTree)
		if json.Unmarshal([]byte(aws.ToString(pol.Policy)), &got) == nil {
			polOK = reflect.DeepEqual(got, wantTree)
		}
	}
	report("bucket policy (TLS only)", polOK, errOr(err, matchWord(polOK)))

	oc, err := s.API.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: b})
	ownership := ""
	if err == nil && len(oc.OwnershipControls.Rules) > 0 {
		ownership = string(oc.OwnershipControls.Rules[0].ObjectOwnership)
	}
	report("object ownership", ownership == "BucketOwnerEnforced", errOr(err, ownership))

	if bad > 0 {
		return 1, nil
	}
	return 0, nil
}

func pabMatches(c *types.PublicAccessBlockConfiguration) bool {
	if c == nil {
		return false
	}
	for _, v := range []*bool{c.BlockPublicAcls, c.IgnorePublicAcls, c.BlockPublicPolicy, c.RestrictPublicBuckets} {
		if !aws.ToBool(v) {
			return false
		}
	}
	return true
}

func matchWord(ok bool) string {
	if ok {
		return "matches"
	}
	return "differs from what setup writes"
}

func errOr(err error, ok string) any {
	if err != nil {
		return strings.TrimSpace(err.Error())
	}
	return ok
}
