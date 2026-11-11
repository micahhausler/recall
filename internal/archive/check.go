package archive

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/micahhausler/recall/internal/store"
)

var regionRE = regexp.MustCompile(`^[a-z]{2,4}(-[a-z]+)+-\d+$`)

// CheckS3 is every S3 call check makes; *s3.Client satisfies it.
type CheckS3 interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Check runs before anything that writes, and prints one ok, FAIL, or skip
// line per check with the fix. With Setup, it checks what setup needs;
// without, what uploads need. It writes one empty <harness>/<host>/.write-check
// object to prove write access; a 412 on later runs is decided after
// authorization, so it still proves it.
type Check struct {
	Region    string
	Profile   string
	Bucket    string
	Host      string
	Harnesses []string
	Setup     bool
	STS       store.STSAPI
	S3        CheckS3
	Out       io.Writer
}

func (c *Check) Run(ctx context.Context) int {
	failed := 0
	report := func(status, what, detail string) {
		line := fmt.Sprintf("%-5s %s", status, what)
		if detail != "" {
			line += ": " + detail
		}
		fmt.Fprintln(c.Out, line)
		if status == "FAIL" {
			failed++
		}
	}
	defer func() {
		if failed > 0 {
			kind := "upload"
			if c.Setup {
				kind = "setup"
			}
			fmt.Fprintf(c.Out, "%d check(s) failed. The policies: recall archive-chats policy %s\n", failed, kind)
		}
	}()

	if c.Region == "" {
		report("FAIL", "region", "set one with: recall config set store.s3.region <region>")
		return 1
	}
	if !regionRE.MatchString(c.Region) {
		report("FAIL", "region", fmt.Sprintf("%q isn't a region code", c.Region))
		return 1
	}
	ident, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		msg := err.Error()
		what := "credentials"
		if strings.Contains(strings.ToLower(msg), "no such host") || strings.Contains(strings.ToLower(msg), "could not connect") {
			what = "region"
		}
		report("FAIL", what, fmt.Sprintf("%s with profile %s: %s", c.Region, orDefault(c.Profile), lastLine(msg)))
		return 1
	}
	report("ok", "region and credentials", fmt.Sprintf("%s, %s", c.Region, aws.ToString(ident.Arn)))

	_, headErr := c.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.Bucket)})
	exists := headErr == nil
	switch {
	case exists:
		report("ok", "bucket", fmt.Sprintf("%s exists and you can reach it", c.Bucket))
	case isNotFound(headErr):
		if c.Setup {
			report("ok", "bucket", c.Bucket+" doesn't exist; setup will create it")
		} else {
			report("FAIL", "bucket", c.Bucket+" doesn't exist; run recall archive-chats setup")
		}
	default:
		report("FAIL", "bucket", fmt.Sprintf("%s: %s (taken by another account, or no s3:ListBucket)", c.Bucket, lastLine(headErr.Error())))
	}

	if c.Setup {
		report("skip", "setup permissions", "IAM simulation isn't built in; compare your policy with: recall archive-chats policy setup")
		return boolToRC(failed)
	}
	if !exists {
		return boolToRC(failed)
	}
	prefix := c.Harnesses[0] + "/" + c.Host + "/"
	one := int32(1)
	_, lsErr := c.S3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.Bucket), Prefix: aws.String(prefix), MaxKeys: &one,
	})
	if lsErr != nil {
		report("FAIL", "s3:ListBucket", lastLine(lsErr.Error()))
	} else {
		report("ok", "s3:ListBucket", "")
	}
	_, putErr := c.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.Bucket), Key: aws.String(prefix + ".write-check"),
		Body: strings.NewReader(""), IfNoneMatch: aws.String("*"),
	})
	if putErr == nil || isPrecondition(putErr) {
		report("ok", "s3:PutObject", "")
	} else {
		report("FAIL", "s3:PutObject", lastLine(putErr.Error()))
	}
	return boolToRC(failed)
}

func boolToRC(failed int) int {
	if failed > 0 {
		return 1
	}
	return 0
}

func orDefault(profile string) string {
	if profile == "" {
		return "(default)"
	}
	return profile
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "NotFound") || strings.Contains(msg, "Not Found")
}

func isPrecondition(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "PreconditionFailed") || strings.Contains(msg, "412")
}
