package archive

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/micahhausler/aws-iam-policy/policy"
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
func Policy(kind, bucket string, harnesses []string) policy.Policy {
	arn := "arn:aws:s3:::" + bucket
	if kind == "setup" {
		return policy.Policy{
			Version: policy.VersionLatest,
			Statements: policy.NewStatementOrSlice(policy.Statement{
				Sid:      "ChatArchiveSetup",
				Effect:   policy.EffectAllow,
				Action:   policy.NewStringOrSlice(false, SetupActions...),
				Resource: policy.NewStringOrSlice(true, arn),
			}),
		}
	}
	prefixes := make([]string, 0, len(harnesses))
	puts := make([]string, 0, len(harnesses))
	for _, h := range harnesses {
		prefixes = append(prefixes, h+"/*")
		puts = append(puts, arn+"/"+h+"/*")
	}
	return policy.Policy{
		Version: policy.VersionLatest,
		Statements: policy.NewStatementOrSlice(
			policy.Statement{
				Sid:      "ListOwnUploads",
				Effect:   policy.EffectAllow,
				Action:   policy.NewStringOrSlice(true, "s3:ListBucket"),
				Resource: policy.NewStringOrSlice(true, arn),
				Condition: map[string]map[string]*policy.ConditionValue{
					"StringLike": {"s3:prefix": policy.NewConditionValueString(false, prefixes...)},
				},
			},
			policy.Statement{
				Sid:      "PutWeeklyArchives",
				Effect:   policy.EffectAllow,
				Action:   policy.NewStringOrSlice(true, "s3:PutObject"),
				Resource: policy.NewStringOrSlice(false, puts...),
			},
		),
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
		fmt.Fprintln(out, "# Archiving only: learning and reducing need more grants, listed in docs/store.md.")
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
