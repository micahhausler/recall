package archive

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fakeAdmin records setup calls and answers verify reads from what was applied.
type fakeAdmin struct {
	exists      bool
	calls       []string
	create      *s3.CreateBucketInput
	pab         *types.PublicAccessBlockConfiguration
	sse         *types.ServerSideEncryptionConfiguration
	policy      string
	lifecycle   *types.BucketLifecycleConfiguration
	driftedDays *int32
}

func (f *fakeAdmin) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.calls = append(f.calls, "head-bucket")
	if f.exists {
		return &s3.HeadBucketOutput{}, nil
	}
	return nil, errors.New("api error NotFound: Not Found (404)")
}

func (f *fakeAdmin) CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.calls = append(f.calls, "create-bucket")
	f.create = in
	f.exists = true
	return &s3.CreateBucketOutput{}, nil
}

func (f *fakeAdmin) PutPublicAccessBlock(ctx context.Context, in *s3.PutPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	f.calls = append(f.calls, "put-public-access-block")
	f.pab = in.PublicAccessBlockConfiguration
	return &s3.PutPublicAccessBlockOutput{}, nil
}

func (f *fakeAdmin) PutBucketEncryption(ctx context.Context, in *s3.PutBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	f.calls = append(f.calls, "put-bucket-encryption")
	f.sse = in.ServerSideEncryptionConfiguration
	return &s3.PutBucketEncryptionOutput{}, nil
}

func (f *fakeAdmin) PutBucketPolicy(ctx context.Context, in *s3.PutBucketPolicyInput, opts ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	f.calls = append(f.calls, "put-bucket-policy")
	f.policy = aws.ToString(in.Policy)
	return &s3.PutBucketPolicyOutput{}, nil
}

func (f *fakeAdmin) PutBucketLifecycleConfiguration(ctx context.Context, in *s3.PutBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	f.calls = append(f.calls, "put-bucket-lifecycle-configuration")
	f.lifecycle = in.LifecycleConfiguration
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}

func (f *fakeAdmin) GetPublicAccessBlock(ctx context.Context, in *s3.GetPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.GetPublicAccessBlockOutput, error) {
	return &s3.GetPublicAccessBlockOutput{PublicAccessBlockConfiguration: f.pab}, nil
}

func (f *fakeAdmin) GetBucketEncryption(ctx context.Context, in *s3.GetBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	return &s3.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: f.sse}, nil
}

func (f *fakeAdmin) GetBucketPolicy(ctx context.Context, in *s3.GetBucketPolicyInput, opts ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	return &s3.GetBucketPolicyOutput{Policy: aws.String(f.policy)}, nil
}

func (f *fakeAdmin) GetBucketLifecycleConfiguration(ctx context.Context, in *s3.GetBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	out := &s3.GetBucketLifecycleConfigurationOutput{}
	if f.lifecycle != nil {
		out.Rules = f.lifecycle.Rules
	}
	if f.driftedDays != nil {
		out.Rules = []types.LifecycleRule{{ID: aws.String("expire"), Expiration: &types.LifecycleExpiration{Days: f.driftedDays}}}
	}
	return out, nil
}

func (f *fakeAdmin) GetBucketOwnershipControls(ctx context.Context, in *s3.GetBucketOwnershipControlsInput, opts ...func(*s3.Options)) (*s3.GetBucketOwnershipControlsOutput, error) {
	ownership := types.ObjectOwnershipBucketOwnerEnforced
	if f.create != nil {
		ownership = f.create.ObjectOwnership
	}
	return &s3.GetBucketOwnershipControlsOutput{OwnershipControls: &types.OwnershipControls{
		Rules: []types.OwnershipControlsRule{{ObjectOwnership: ownership}},
	}}, nil
}

func newSetup(f *fakeAdmin) (*Setup, *strings.Builder) {
	out := &strings.Builder{}
	return &Setup{API: f, Bucket: "bkt", Region: "us-west-2", Namespaced: true, Retention: 3650, Out: out}, out
}

func TestSetupCreatesPrivateBucketAndVerifies(t *testing.T) {
	f := &fakeAdmin{}
	s, out := newSetup(f)
	rc, err := s.Run(context.Background(), false)
	if err != nil || rc != 0 {
		t.Fatalf("rc=%d err=%v\n%s", rc, err, out)
	}
	wantCalls := []string{"head-bucket", "create-bucket", "put-public-access-block",
		"put-bucket-encryption", "put-bucket-policy", "put-bucket-lifecycle-configuration"}
	if !equal(f.calls, wantCalls) {
		t.Errorf("calls = %v", f.calls)
	}
	if f.create.BucketNamespace != types.BucketNamespaceAccountRegional {
		t.Errorf("namespace = %v", f.create.BucketNamespace)
	}
	if f.create.CreateBucketConfiguration.LocationConstraint != "us-west-2" {
		t.Error("location constraint missing")
	}
	if f.create.ObjectOwnership != types.ObjectOwnershipBucketOwnerEnforced {
		t.Errorf("ownership = %v", f.create.ObjectOwnership)
	}
	if !pabMatches(f.pab) {
		t.Errorf("public access block = %+v", f.pab)
	}
	var policy struct {
		Statement []struct {
			Effect    string
			Condition map[string]map[string]string
		}
	}
	json.Unmarshal([]byte(f.policy), &policy)
	if len(policy.Statement) != 1 || policy.Statement[0].Effect != "Deny" ||
		policy.Statement[0].Condition["Bool"]["aws:SecureTransport"] != "false" {
		t.Errorf("policy = %s", f.policy)
	}
	if days := *f.lifecycle.Rules[0].Expiration.Days; days != 3650 {
		t.Errorf("expiration = %d", days)
	}
	for _, want := range []string{
		"ok    verify lifecycle expiration: 3650",
		"ok    verify bucket policy (TLS only): matches",
		"ok    verify object ownership: BucketOwnerEnforced",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
}

func TestSetupVerifyCatchesAMismatch(t *testing.T) {
	one := int32(1)
	f := &fakeAdmin{driftedDays: &one}
	s, out := newSetup(f)
	rc, err := s.Run(context.Background(), false)
	if err != nil || rc != 1 {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	if !strings.Contains(out.String(), "FAIL  verify lifecycle expiration") {
		t.Errorf("output = %s", out)
	}
}

func TestSetupUsEast1AndExplicitBucket(t *testing.T) {
	f := &fakeAdmin{}
	s, _ := newSetup(f)
	s.Region = "us-east-1"
	s.Namespaced = false
	if _, err := s.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if f.create.CreateBucketConfiguration != nil {
		t.Error("us-east-1 got a location constraint")
	}
	if f.create.BucketNamespace != "" {
		t.Error("explicit bucket got a namespace")
	}
}

func TestSetupExistingBucketReappliesSettings(t *testing.T) {
	f := &fakeAdmin{exists: true}
	s, out := newSetup(f)
	rc, err := s.Run(context.Background(), false)
	if err != nil || rc != 0 {
		t.Fatalf("rc=%d err=%v\n%s", rc, err, out)
	}
	for _, c := range f.calls {
		if c == "create-bucket" {
			t.Error("create-bucket called for an existing bucket")
		}
	}
	if !strings.Contains(out.String(), "already existed") {
		t.Errorf("output = %s", out)
	}
}

func TestSetupDryRunChangesNothing(t *testing.T) {
	f := &fakeAdmin{}
	s, out := newSetup(f)
	rc, err := s.Run(context.Background(), true)
	if err != nil || rc != 0 {
		t.Fatal(err)
	}
	if !equal(f.calls, []string{"head-bucket"}) {
		t.Errorf("calls = %v", f.calls)
	}
	if !strings.Contains(out.String(), "would run: create-bucket") {
		t.Errorf("output = %s", out)
	}
}

func TestPolicySetupCoversEverySetupCall(t *testing.T) {
	p := Policy("setup", "bkt", []string{"claude"})
	stmt := p["Statement"].([]map[string]any)[0]
	if stmt["Resource"] != "arn:aws:s3:::bkt" {
		t.Errorf("resource = %v", stmt["Resource"])
	}
	actions := map[string]bool{}
	for _, a := range stmt["Action"].([]string) {
		actions[a] = true
	}
	// Every call setup makes maps to an action the policy allows.
	for call, action := range map[string]string{
		"head-bucket":                        "s3:ListBucket",
		"create-bucket":                      "s3:CreateBucket",
		"put-public-access-block":            "s3:PutBucketPublicAccessBlock",
		"put-bucket-encryption":              "s3:PutEncryptionConfiguration",
		"put-bucket-policy":                  "s3:PutBucketPolicy",
		"put-bucket-lifecycle-configuration": "s3:PutLifecycleConfiguration",
	} {
		if !actions[action] {
			t.Errorf("policy misses %s for %s", action, call)
		}
	}
	// create-bucket --object-ownership also needs this.
	if !actions["s3:PutBucketOwnershipControls"] {
		t.Error("policy misses s3:PutBucketOwnershipControls")
	}
	for _, get := range []string{"s3:GetBucketPublicAccessBlock", "s3:GetEncryptionConfiguration",
		"s3:GetBucketPolicy", "s3:GetLifecycleConfiguration", "s3:GetBucketOwnershipControls"} {
		if !actions[get] {
			t.Errorf("policy misses %s for verify", get)
		}
	}
}

func TestPolicyUploadIsWriteOnlyAndScoped(t *testing.T) {
	p := Policy("upload", "bkt", []string{"claude"})
	stmts := p["Statement"].([]map[string]any)
	if len(stmts) != 2 {
		t.Fatalf("statements = %v", stmts)
	}
	ls, put := stmts[0], stmts[1]
	if ls["Action"] != "s3:ListBucket" || put["Action"] != "s3:PutObject" {
		t.Errorf("actions = %v, %v", ls["Action"], put["Action"])
	}
	if got := put["Resource"].([]string); !equal(got, []string{"arn:aws:s3:::bkt/claude/*"}) {
		t.Errorf("put resource = %v", got)
	}
	cond := ls["Condition"].(map[string]any)["StringLike"].(map[string]any)["s3:prefix"].([]string)
	if !equal(cond, []string{"claude/*"}) {
		t.Errorf("list condition = %v", cond)
	}
}

// fakeSTS and fakeCheckS3 drive Check without AWS.
type fakeSTS struct{ err error }

func (f *fakeSTS) GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{
		Account: aws.String("111122223333"),
		Arn:     aws.String("arn:aws:iam::111122223333:user/you"),
	}, nil
}

type fakeCheckS3 struct {
	headErr error
	listErr error
	putErr  error
	putKey  string
}

func (f *fakeCheckS3) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeCheckS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &s3.ListObjectsV2Output{}, nil
}

func (f *fakeCheckS3) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putKey = aws.ToString(in.Key)
	if f.putErr != nil {
		return nil, f.putErr
	}
	return &s3.PutObjectOutput{}, nil
}

func check(f3 *fakeCheckS3, fs *fakeSTS, setup bool) (int, string) {
	out := &strings.Builder{}
	c := &Check{
		Region: "us-west-2", Bucket: "bkt", Host: "box", Harnesses: []string{"claude"},
		Setup: setup, STS: fs, S3: f3, Out: out,
	}
	return c.Run(context.Background()), out.String()
}

func TestCheckSetupOnANewBucketPasses(t *testing.T) {
	rc, out := check(&fakeCheckS3{headErr: errors.New("NotFound: 404")}, &fakeSTS{}, true)
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	for _, want := range []string{"setup will create it", "skip  setup permissions"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
}

func TestCheckUploadProbe412ProvesWriteAccess(t *testing.T) {
	f3 := &fakeCheckS3{putErr: errors.New("api error PreconditionFailed (412)")}
	rc, out := check(f3, &fakeSTS{}, false)
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	if !strings.Contains(out, "ok    s3:PutObject") {
		t.Errorf("output = %s", out)
	}
	if f3.putKey != "claude/box/.write-check" {
		t.Errorf("probe key = %s", f3.putKey)
	}
}

func TestCheckUploadDenied(t *testing.T) {
	rc, out := check(&fakeCheckS3{putErr: errors.New("AccessDenied")}, &fakeSTS{}, false)
	if rc != 1 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	for _, want := range []string{"FAIL  s3:PutObject: AccessDenied", "recall archive-chats policy upload"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
}

func TestCheckRegionProblems(t *testing.T) {
	out := &strings.Builder{}
	c := &Check{Region: "", Out: out}
	if rc := c.Run(context.Background()); rc != 1 || !strings.Contains(out.String(), "FAIL  region: set one") {
		t.Errorf("rc=%d out=%s", rc, out)
	}
	out.Reset()
	c = &Check{Region: "nowhere", Out: out}
	if rc := c.Run(context.Background()); rc != 1 || !strings.Contains(out.String(), "isn't a region code") {
		t.Errorf("rc=%d out=%s", rc, out)
	}
}

func TestCheckBadCredentialsVsBadRegion(t *testing.T) {
	for errText, what := range map[string]string{
		"dial tcp: lookup sts.nowhere.amazonaws.com: no such host": "region",
		"failed to retrieve credentials":                           "credentials",
	} {
		rc, out := check(&fakeCheckS3{}, &fakeSTS{err: errors.New(errText)}, false)
		if rc != 1 || !strings.Contains(out, "FAIL  "+what) {
			t.Errorf("%q: rc=%d out=%s", errText, rc, out)
		}
	}
}
