package archive

import (
	"encoding/json"
	"fmt"
	"io"
)

// SetupActions is every IAM action `archive-chats setup` needs, once per
// account.
var SetupActions = []string{
	"s3:CreateBucket",
	"s3:PutBucketOwnershipControls",
	"s3:GetBucketOwnershipControls",
	"s3:PutBucketPublicAccessBlock",
	"s3:GetBucketPublicAccessBlock",
	"s3:PutEncryptionConfiguration",
	"s3:GetEncryptionConfiguration",
	"s3:PutBucketPolicy",
	"s3:GetBucketPolicy",
	"s3:PutLifecycleConfiguration",
	"s3:GetLifecycleConfiguration",
	"s3:ListBucket",
}

// Policy returns the IAM policy for kind "setup" or "upload". The upload
// policy is write-only and scoped to the harness prefixes: a machine that's
// compromised can add objects but can't read or remove other machines'
// transcripts.
func Policy(kind, bucket string, harnesses []string) map[string]any {
	arn := "arn:aws:s3:::" + bucket
	if kind == "setup" {
		return map[string]any{
			"Version": "2012-10-17",
			"Statement": []map[string]any{{
				"Sid": "ChatArchiveSetup", "Effect": "Allow",
				"Action": SetupActions, "Resource": arn,
			}},
		}
	}
	prefixes := make([]string, 0, len(harnesses))
	puts := make([]string, 0, len(harnesses))
	for _, h := range harnesses {
		prefixes = append(prefixes, h+"/*")
		puts = append(puts, arn+"/"+h+"/*")
	}
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Sid": "ListOwnUploads", "Effect": "Allow",
				"Action": "s3:ListBucket", "Resource": arn,
				"Condition": map[string]any{"StringLike": map[string]any{"s3:prefix": prefixes}},
			},
			{
				"Sid": "PutWeeklyArchives", "Effect": "Allow",
				"Action": "s3:PutObject", "Resource": puts,
			},
		},
	}
}

// PrintPolicy writes one policy with a header saying when it's needed.
func PrintPolicy(out io.Writer, kind, bucket, host string, harnesses []string) error {
	what := map[string]string{
		"setup":  "creating and configuring the bucket, once per account (recall archive-chats setup)",
		"upload": "weekly uploads from every machine (recall archive-chats run); no read or delete",
	}[kind]
	if what == "" {
		return fmt.Errorf("policy wants setup or upload, got %q", kind)
	}
	fmt.Fprintf(out, "# IAM policy for %s. Bucket: %s\n", what, bucket)
	if kind == "upload" {
		for _, h := range harnesses {
			fmt.Fprintf(out, "# To limit a machine to its own uploads, narrow %s/* to %s/%s/*.\n", h, h, host)
		}
	}
	data, err := json.MarshalIndent(Policy(kind, bucket, harnesses), "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(data))
	return nil
}
