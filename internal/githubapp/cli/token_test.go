package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/internal/githubapp/app/apptest"
	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/config"
	"github.com/micahhausler/recall/internal/githubapp/ghapi"
	"github.com/micahhausler/recall/internal/version"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func keyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})
}

func keyFingerprint() string {
	fp, err := appkey.Fingerprint(&testKey().PublicKey)
	if err != nil {
		panic(err)
	}
	return fp
}

// testName names the command in tests, unlike the real one.
const testName = "recall-example"

// fixture is a machine with a fake GitHub that has the App installed on
// you/dotfiles.
type fixture struct {
	t      *testing.T
	env    *env
	github *apptest.Fake
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	gh := &apptest.Fake{Key: &testKey().PublicKey, Now: func() time.Time { return now }, Slug: "recall-you", Owner: "you", BotID: 4242, ClientID: "Iv23liEXAMPLE",
		Installed: map[string]int64{"you/dotfiles": 99},
		Granted:   map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read", "metadata": "read"}}
	srv := httptest.NewServer(gh.Handler())
	t.Cleanup(srv.Close)
	home := t.TempDir()
	f := &fixture{t: t, github: gh, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	f.env = &env{name: testName, home: home, api: srv.URL, web: "https://github.example", client: srv.Client(),
		stdin: strings.NewReader(""), stdout: f.stdout, stderr: f.stderr, now: func() time.Time { return now }}
	t.Cleanup(func() {
		for _, p := range gh.Problems {
			t.Errorf("fake GitHub: %s", p)
		}
	})
	return f
}

// configured writes a PEM key and a config that names it.
func (f *fixture) configured(cfg config.Config) {
	f.t.Helper()
	keyPath := filepath.Join(f.env.home, "app.pem")
	if err := os.WriteFile(keyPath, keyPEM(), 0o600); err != nil {
		f.t.Fatal(err)
	}
	cfg.ClientID, cfg.Key = "Iv23liEXAMPLE", "file:~/app.pem"
	if err := cfg.Save(config.Path(f.env.home, testName)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) run(stdin string, args ...string) int {
	f.stdout.Reset()
	f.stderr.Reset()
	f.env.stdin = strings.NewReader(stdin)
	return f.env.run(context.Background(), args)
}

// token runs the token command the way recall does: the request in
// RECALL_EXEC_INFO, and nothing on stdin.
func (f *fixture) token(request string) int {
	f.env.execInfo = request
	return f.run("", "token")
}

const request = `{"apiVersion": "recall.credentials/v1", "kind": "Credential", "spec": {"repo": "github.com/you/dotfiles"}}`

// token reads a request and prints a credential that matches the contract:
// the spec as it came, and a status holding a token scoped to the one
// repository, its expiry, the bot's commit identity, and the owner.
func TestTokenAnswersTheContract(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	if code := f.token(request); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	var got map[string]any
	if err := json.Unmarshal(f.stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout isn't one JSON object: %v\n%s", err, f.stdout)
	}
	want := map[string]any{
		"apiVersion": "recall.credentials/v1", "kind": "Credential",
		"spec": map[string]any{"repo": "github.com/you/dotfiles"},
		"status": map[string]any{
			"token": "ghs_fake1", "expirationTimestamp": "2026-03-02T13:00:00Z", "owner": "you",
			"commitIdentity": map[string]any{"name": "recall-you[bot]", "email": "4242+recall-you[bot]@users.noreply.github.com"},
		},
	}
	if a, b := mustJSON(got), mustJSON(want); a != b {
		t.Errorf("credential =\n%s\nwant\n%s", a, b)
	}
	if len(f.github.Requests) != 1 {
		t.Fatalf("GitHub minted %d tokens", len(f.github.Requests))
	}
	req := f.github.Requests[0]
	if len(req.Repositories) != 1 || req.Repositories[0] != "dotfiles" {
		t.Errorf("token covers %v, want only dotfiles", req.Repositories)
	}
	if mustJSON(req.Permissions) != mustJSON(tokenPermissions) {
		t.Errorf("token permissions = %v, want %v", req.Permissions, tokenPermissions)
	}
	// The config held everything, so nothing but the token was fetched.
	if f.github.UserLookups != 0 {
		t.Error("token looked up the bot user although the config names it")
	}
}

func mustJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// A config with only the client ID and key still answers in full: token
// asks GitHub for the rest, and doesn't rewrite the file.
func TestTokenFillsInAMinimalConfig(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{})
	before, _ := os.ReadFile(config.Path(f.env.home, testName))
	if code := f.token(request); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	var got credentialsv1.Credential
	if err := json.Unmarshal(f.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if st := got.Status; st == nil || st.Owner != "you" || st.CommitIdentity == nil || st.CommitIdentity.Email != "4242+recall-you[bot]@users.noreply.github.com" {
		t.Errorf("credential = %+v", got)
	}
	if after, _ := os.ReadFile(config.Path(f.env.home, testName)); string(after) != string(before) {
		t.Error("token rewrote the config")
	}
}

// A failure exits non-zero with the reason on stderr and nothing on
// stdout, so recall never mistakes an error for a credential.
func TestTokenFailuresSayWhyOnStderr(t *testing.T) {
	for name, tc := range map[string]struct{ request, want string }{
		"not installed": {`{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/elsewhere"}}`,
			"you/elsewhere: the GitHub App isn't installed on the repository, or the repository doesn't exist; install it at https://github.example/apps/recall-you/installations/new"},
		"another host":    {`{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"git.example.com/you/dotfiles"}}`, "github.com only"},
		"not a repo":      {`{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"dotfiles"}}`, "isn't host/owner/name"},
		"no spec":         {`{"apiVersion":"recall.credentials/v1","kind":"Credential"}`, "isn't host/owner/name"},
		"a flat repo":     {`{"apiVersion":"recall.credentials/v1","kind":"Credential","repo":"github.com/you/dotfiles"}`, "isn't host/owner/name"},
		"a newer version": {`{"apiVersion":"recall.credentials/v2","kind":"Credential","spec":{"repo":"github.com/you/dotfiles"}}`, `this command answers "recall.credentials/v1"`},
		"another kind":    {`{"apiVersion":"recall.credentials/v1","kind":"TokenRequest","spec":{"repo":"github.com/you/dotfiles"}}`, "this command answers"},
		"not JSON":        {`v1`, "reading the credential request from RECALL_EXEC_INFO"},
		"no request":      {``, "RECALL_EXEC_INFO isn't set"},
	} {
		f := newFixture(t)
		f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
		code := f.token(tc.request)
		if code != 1 || !strings.Contains(f.stderr.String(), tc.want) || f.stdout.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 1 and %q", name, code, f.stdout, f.stderr, tc.want)
		}
		if len(f.github.Requests) != 0 {
			t.Errorf("%s: a token was minted", name)
		}
	}
	// No config at all says how to make one.
	f := newFixture(t)
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), "create") {
		t.Errorf("no config: exit %d, stderr %q", code, f.stderr)
	}
}

// The request is RECALL_EXEC_INFO and nothing else: a request on stdin
// is not one, whether or not the variable is set.
func TestTokenReadsTheRequestFromTheEnvironmentOnly(t *testing.T) {
	const other = `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/elsewhere"}}`
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	f.env.execInfo = request
	code := f.run(other, "token")
	var got credentialsv1.Credential
	if err := json.Unmarshal(f.stdout.Bytes(), &got); code != 0 || err != nil || got.Spec.Repo != "github.com/you/dotfiles" {
		t.Errorf("exit %d, %v, stderr %q; want a credential for the repository the variable names", code, err, f.stderr)
	}

	f = newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	if code := f.run(request, "token"); code != 1 || !strings.Contains(f.stderr.String(), "RECALL_EXEC_INFO isn't set") || f.stdout.Len() != 0 || len(f.github.Requests) != 0 {
		t.Errorf("a request on stdin alone: exit %d, stderr %q, %d minted; want exit 1 and nothing minted", code, f.stderr, len(f.github.Requests))
	}
}

// Unknown request fields are ignored, so recall can add to the contract.
func TestTokenIgnoresFieldsItDoesNotKnow(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	in := `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/dotfiles","somethingNewer":true},"metadata":{}}`
	if code := f.token(in); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
}

// A config key this version doesn't know is an error, so a misspelled
// key_fingerprint can't switch its check off; and a key file others can
// read is refused.
func TestTokenRefusesAMisspelledConfigAndAReadableKey(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	path := config.Path(f.env.home, testName)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(good, []byte("key_fingerprnt = 'SHA256:x'\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), "key_fingerprnt") || f.stdout.Len() != 0 {
		t.Errorf("a misspelled key: exit %d, stderr %q", code, f.stderr)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.env.home, "app.pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), "chmod 600") || f.stdout.Len() != 0 {
		t.Errorf("a readable key file: exit %d, stderr %q", code, f.stderr)
	}
}

const (
	installationCall = "GET /repos/you/dotfiles/installation"
	mintCall         = "POST /app/installations/99/access_tokens"
	revokeCall       = "DELETE /installation/token"
)

func retryFixture(t *testing.T) *fixture {
	t.Helper()
	saved := ghapi.Backoff
	ghapi.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { ghapi.Backoff = saved })
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	return f
}

func (f *fixture) noTokenOnStderr() {
	f.t.Helper()
	for _, r := range f.github.Requests {
		if strings.Contains(f.stderr.String(), r.Token) {
			f.t.Errorf("stderr holds a token: %s", f.stderr)
		}
	}
}

func TestTokenRetriesALookupGitHubFailed(t *testing.T) {
	f := retryFixture(t)
	f.github.Faults = map[string][]apptest.Fault{installationCall: {{Status: 502}, {Status: 503}}}
	if code := f.token(request); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if got := f.github.CallCount(installationCall); got != 3 {
		t.Errorf("installation looked up %d times, want 3", got)
	}
	if got := len(f.github.Requests); got != 1 {
		t.Errorf("%d tokens minted, want 1", got)
	}
}

func TestTokenGivesUpAfterThreeTries(t *testing.T) {
	f := retryFixture(t)
	f.github.Faults = map[string][]apptest.Fault{installationCall: {{Status: 502}, {Status: 502}, {Status: 502}}}
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), "502") || f.stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and the 502", code, f.stdout, f.stderr)
	}
	if got := f.github.CallCount(installationCall); got != 3 {
		t.Errorf("installation looked up %d times, want 3", got)
	}
	if got := len(f.github.Requests); got != 0 {
		t.Errorf("%d tokens minted, want 0", got)
	}
}

// GitHub minted, and the answer was lost: asking again would mint a
// second token nobody holds.
func TestTokenNeverMintsTwiceAfterALostAnswer(t *testing.T) {
	f := retryFixture(t)
	f.github.Faults = map[string][]apptest.Fault{mintCall: {{Status: 502, Handled: true}}}
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), "502") || f.stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and the 502", code, f.stdout, f.stderr)
	}
	if calls, minted := f.github.CallCount(mintCall), len(f.github.Requests); calls != 1 || minted != 1 {
		t.Errorf("mint called %d times, %d minted; want 1 and 1", calls, minted)
	}
	f.noTokenOnStderr()
}

func TestTokenWaitsOutARateLimit(t *testing.T) {
	f := retryFixture(t)
	f.github.Faults = map[string][]apptest.Fault{mintCall: {{Status: 429, Header: map[string]string{"Retry-After": "1"}}}}
	start := time.Now()
	if code := f.token(request); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("answered in %v, before the second GitHub asked for", elapsed)
	}
	if calls, minted := f.github.CallCount(mintCall), len(f.github.Requests); calls != 2 || minted != 1 {
		t.Errorf("mint called %d times, %d minted; want 2 and 1", calls, minted)
	}
}

func TestTokenFailsAtOnceOnALongRateLimit(t *testing.T) {
	f := retryFixture(t)
	f.github.Faults = map[string][]apptest.Fault{mintCall: {{Status: 403, Header: map[string]string{"Retry-After": "60"}}}}
	start := time.Now()
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), "retry after 1m0s") || f.stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and the wait named", code, f.stdout, f.stderr)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v; want no waiting", elapsed)
	}
	if calls, minted := f.github.CallCount(mintCall), len(f.github.Requests); calls != 1 || minted != 0 {
		t.Errorf("mint called %d times, %d minted; want 1 and 0", calls, minted)
	}
}

// --version prints the command's name and the release tag as written.
func TestVersionPrintsTheTag(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.2.3"
	f := newFixture(t)
	if code := f.run("", "--version"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if got, want := f.stdout.String(), testName+" v1.2.3\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if code := f.run("", "token", "--version"); code != 2 {
		t.Errorf("token --version: exit %d, want 2", code)
	}
}

// --permission adds what the config's extra_permissions hold to the
// token, and nothing they don't.
func TestTokenAsksForAnExtraPermissionTheConfigHolds(t *testing.T) {
	f := newFixture(t)
	f.github.Granted["issues"] = "write"
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you",
		ExtraPermissions: map[string]string{"issues": "write"}})
	f.env.execInfo = request
	if code := f.run("", "token", "--permission", "issues=write"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	want := map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read", "issues": "write"}
	if got := f.github.Requests[len(f.github.Requests)-1].Permissions; !maps.Equal(got, want) {
		t.Errorf("asked for %v, want %v", got, want)
	}
	minted := len(f.github.Requests)
	for _, arg := range []string{"administration=write", "checks=write", "issues=admin"} {
		if code := f.run("", "token", "--permission", arg); code == 0 {
			t.Errorf("token --permission %s: exit 0, want a refusal", arg)
		}
	}
	if len(f.github.Requests) != minted {
		t.Errorf("a refused --permission minted a token")
	}
}
