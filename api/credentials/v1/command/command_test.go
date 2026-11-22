package command_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/api/credentials/v1/command"
	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/credential"
)

// helperEnv turns this test binary into a credential command built on Run,
// so recall's own side of the contract can run one for real.
const helperEnv = "RECALL_COMMAND_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		if err := command.Run(context.Background(), os.Getenv(credentialsv1.ExecInfoEnv), os.Stdout, grant); err != nil {
			fmt.Fprintln(os.Stderr, "recall-credential:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const (
	request = `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"example.com/you/example"}}`
	secret  = "example-token"
)

// granted is a grant function that returns status and counts its calls.
func granted(status credentialsv1.Status, calls *int) func(context.Context, command.Request) (credentialsv1.Status, error) {
	return func(context.Context, command.Request) (credentialsv1.Status, error) {
		*calls++
		return status, nil
	}
}

func TestParseRequest(t *testing.T) {
	got, err := command.ParseRequest(request)
	want := command.Request{Spec: credentialsv1.Spec{Repo: "example.com/you/example"}, Host: "example.com", Owner: "you", Name: "example"}
	if err != nil || got != want {
		t.Errorf("request = %+v, %v\nwant      %+v", got, err, want)
	}
}

// The contract grows by adding fields, so a request from a newer recall
// still parses, and the fields this version doesn't know are dropped.
func TestParseRequestIgnoresUnknownFields(t *testing.T) {
	const newer = `{"apiVersion":"recall.credentials/v1","kind":"Credential","more":1,
		"spec":{"repo":"example.com/you/example","later":"field"},"status":{"token":"from-the-request","later":true}}`
	got, err := command.ParseRequest(newer)
	if err != nil {
		t.Fatalf("a request with fields this version doesn't know: %v", err)
	}
	if got.Spec != (credentialsv1.Spec{Repo: "example.com/you/example"}) || got.Host != "example.com" || got.Owner != "you" || got.Name != "example" {
		t.Errorf("request = %+v", got)
	}
	var out bytes.Buffer
	if err := got.WriteAnswer(&out, credentialsv1.Status{Token: secret}); err != nil {
		t.Fatal(err)
	}
	for _, stray := range []string{"later", "more", "from-the-request"} {
		if strings.Contains(out.String(), stray) {
			t.Errorf("the answer carries %q from the request:\n%s", stray, &out)
		}
	}
}

// Each way a request can be wrong has its own error, and none of them
// reaches grant or stdout.
func TestBadRequests(t *testing.T) {
	object := func(apiVersion, kind, rest string) string {
		return fmt.Sprintf(`{"apiVersion":%q,"kind":%q%s}`, apiVersion, kind, rest)
	}
	repo := func(slug string) string {
		return object("recall.credentials/v1", "Credential", fmt.Sprintf(`,"spec":{"repo":%q}`, slug))
	}
	for name, tc := range map[string]struct{ execInfo, want string }{
		"unset":              {``, "RECALL_EXEC_INFO isn't set"},
		"not JSON":           {`v1`, "reading the credential request from RECALL_EXEC_INFO"},
		"half an object":     {`{"apiVersion":"recall.credentials/v1"`, "reading the credential request from RECALL_EXEC_INFO"},
		"an array":           {`[]`, "reading the credential request from RECALL_EXEC_INFO"},
		"two objects":        {request + request, "reading the credential request from RECALL_EXEC_INFO"},
		"null":               {`null`, `the request is "" ""`},
		"a newer version":    {object("recall.credentials/v2", "Credential", `,"spec":{"repo":"example.com/you/example"}`), `the request is "recall.credentials/v2" "Credential"; this command answers "recall.credentials/v1" "Credential"`},
		"no version":         {`{"kind":"Credential","spec":{"repo":"example.com/you/example"}}`, `the request is "" "Credential"`},
		"another kind":       {object("recall.credentials/v1", "TokenRequest", `,"spec":{"repo":"example.com/you/example"}`), `the request is "recall.credentials/v1" "TokenRequest"`},
		"no kind":            {`{"apiVersion":"recall.credentials/v1","spec":{"repo":"example.com/you/example"}}`, `the request is "recall.credentials/v1" ""`},
		"no spec":            {object("recall.credentials/v1", "Credential", ``), `spec.repo "" isn't host/owner/name`},
		"a flat repo":        {object("recall.credentials/v1", "Credential", `,"repo":"example.com/you/example"`), `spec.repo "" isn't host/owner/name`},
		"a name alone":       {repo("example"), `spec.repo "example" isn't host/owner/name`},
		"no host":            {repo("you/example"), `spec.repo "you/example" isn't host/owner/name`},
		"an empty host":      {repo("/you/example"), "isn't host/owner/name"},
		"an empty owner":     {repo("example.com//example"), "isn't host/owner/name"},
		"an empty name":      {repo("example.com/you/"), "isn't host/owner/name"},
		"a fourth part":      {repo("example.com/you/example/more"), "isn't host/owner/name"},
		"a URL":              {repo("https://example.com/you/example"), "isn't host/owner/name"},
		"a repo of a number": {object("recall.credentials/v1", "Credential", `,"spec":{"repo":7}`), "reading the credential request from RECALL_EXEC_INFO"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := command.ParseRequest(tc.execInfo); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseRequest: err = %v, want %q", err, tc.want)
			}
			var out bytes.Buffer
			calls := 0
			err := command.Run(context.Background(), tc.execInfo, &out, granted(credentialsv1.Status{Token: secret}, &calls))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Run: err = %v, want %q", err, tc.want)
			}
			if calls != 0 || out.Len() != 0 {
				t.Errorf("Run called grant %d times and wrote %q; want neither", calls, &out)
			}
		})
	}
}

// The bytes here are what recall reads. The object's shape is the
// contract's, tested beside its types; this pins how Run lays it out.
func TestRunWritesTheAnswer(t *testing.T) {
	// An expiry with a zone and a fraction of a second goes out in UTC, to
	// the second.
	expires := time.Date(2026, 3, 2, 8, 0, 0, 987654321, time.FixedZone("", -5*60*60))
	given := expires
	status := credentialsv1.Status{
		Token: secret, ExpirationTimestamp: &expires,
		CommitIdentity: &credentialsv1.CommitIdentity{Name: "recall-you[bot]", Email: "recall-you[bot]@example.com"},
		Owner:          "you",
	}
	const want = `{
  "apiVersion": "recall.credentials/v1",
  "kind": "Credential",
  "spec": {
    "repo": "example.com/you/example"
  },
  "status": {
    "token": "example-token",
    "expirationTimestamp": "2026-03-02T13:00:00Z",
    "commitIdentity": {
      "name": "recall-you[bot]",
      "email": "recall-you[bot]@example.com"
    },
    "owner": "you"
  }
}
`
	var out bytes.Buffer
	var asked command.Request
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "the caller's context")
	var passed any
	err := command.Run(ctx, request, &out, func(ctx context.Context, req command.Request) (credentialsv1.Status, error) {
		asked, passed = req, ctx.Value(key{})
		return status, nil
	})
	if passed != "the caller's context" {
		t.Errorf("grant got a context holding %v, want the one Run was given", passed)
	}
	if err != nil || out.String() != want {
		t.Errorf("Run wrote, with error %v:\n%s\nwant:\n%s", err, &out, want)
	}
	if asked.Host != "example.com" || asked.Owner != "you" || asked.Name != "example" {
		t.Errorf("grant was asked for %+v", asked)
	}
	if !expires.Equal(given) || expires.Location() != given.Location() {
		t.Errorf("Run changed the caller's expiry to %s", expires)
	}

	// Only the token is required, and the rest is left out.
	const minimal = `{
  "apiVersion": "recall.credentials/v1",
  "kind": "Credential",
  "spec": {
    "repo": "example.com/you/example"
  },
  "status": {
    "token": "example-token"
  }
}
`
	out.Reset()
	calls := 0
	if err := command.Run(context.Background(), request, &out, granted(credentialsv1.Status{Token: secret}, &calls)); err != nil || out.String() != minimal {
		t.Errorf("Run wrote, with error %v:\n%s\nwant:\n%s", err, &out, minimal)
	}
}

// A failure after the request parsed still leaves stdout empty, so recall
// never mistakes an error for a credential, and no error holds the token.
func TestFailuresWriteNothing(t *testing.T) {
	errGrant := errors.New("the bot isn't installed on you/example")
	for name, tc := range map[string]struct {
		status credentialsv1.Status
		err    error
		want   string
	}{
		"grant fails":               {credentialsv1.Status{}, errGrant, "the bot isn't installed on you/example"},
		"grant fails with a status": {credentialsv1.Status{Token: secret}, errGrant, "the bot isn't installed on you/example"},
		"no status":                 {credentialsv1.Status{}, nil, "no status.token"},
		"an owner alone":            {credentialsv1.Status{Owner: "you"}, nil, "no status.token"},
		"an identity with no email": {credentialsv1.Status{Token: secret, CommitIdentity: &credentialsv1.CommitIdentity{Name: "recall-you[bot]"}}, nil, "missing its name or email"},
		"an identity with no name":  {credentialsv1.Status{Token: secret, CommitIdentity: &credentialsv1.CommitIdentity{Email: "recall-you[bot]@example.com"}}, nil, "missing its name or email"},
		"an empty identity":         {credentialsv1.Status{Token: secret, CommitIdentity: &credentialsv1.CommitIdentity{}}, nil, "missing its name or email"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := command.Run(context.Background(), request, &out, func(context.Context, command.Request) (credentialsv1.Status, error) { return tc.status, tc.err })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want grant's own error", err)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("the error holds the token: %v", err)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", &out)
			}
		})
	}
}

// failing is a writer that takes nothing.
type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }

func TestAWriteErrorComesBackWithoutTheToken(t *testing.T) {
	calls := 0
	err := command.Run(context.Background(), request, failing{}, granted(credentialsv1.Status{Token: secret}, &calls))
	if err == nil || !strings.Contains(err.Error(), "the pipe is closed") || strings.Contains(err.Error(), secret) {
		t.Errorf("err = %v", err)
	}
}

// A terminal is refused before anything else happens: no token is made
// only to be thrown away, and a bad request doesn't hide the refusal.
func TestATerminalIsRefused(t *testing.T) {
	tty := openTerminal(t)
	for name, execInfo := range map[string]string{"a good request": request, "no request": ""} {
		calls := 0
		err := command.Run(context.Background(), execInfo, tty, granted(credentialsv1.Status{Token: secret}, &calls))
		if !errors.Is(err, command.ErrTerminal) || calls != 0 {
			t.Errorf("%s: Run to a terminal: err = %v after %d grants; want ErrTerminal and none", name, err, calls)
		}
	}
	req, err := command.ParseRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.WriteAnswer(tty, credentialsv1.Status{Token: secret}); !errors.Is(err, command.ErrTerminal) {
		t.Errorf("WriteAnswer to a terminal: err = %v, want ErrTerminal", err)
	}
	if strings.Contains(command.ErrTerminal.Error(), secret) {
		t.Error("ErrTerminal holds a token")
	}
}

// Only a terminal is refused. The null device is a character device too,
// and a pipe is what recall reads from.
func TestOtherFilesAreNotRefused(t *testing.T) {
	want := new(bytes.Buffer)
	calls := 0
	grant := granted(credentialsv1.Status{Token: secret}, &calls)
	if err := command.Run(context.Background(), request, want, grant); err != nil {
		t.Fatal(err)
	}

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if err := command.Run(context.Background(), request, null, grant); err != nil {
		t.Errorf("Run to %s: %v", os.DevNull, err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := command.Run(context.Background(), request, w, grant); err != nil {
		t.Errorf("Run to a pipe: %v", err)
	}
	w.Close()
	if got, err := io.ReadAll(r); err != nil || string(got) != want.String() {
		t.Errorf("the pipe carried %q, %v\nwant %q", got, err, want)
	}

	path := filepath.Join(t.TempDir(), "answer.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(context.Background(), request, file, grant); err != nil {
		t.Errorf("Run to a file: %v", err)
	}
	file.Close()
	if got, err := os.ReadFile(path); err != nil || string(got) != want.String() {
		t.Errorf("the file holds %q, %v\nwant %q", got, err, want)
	}
}

// recall's own side of the contract runs a command built on Run, with the
// request in the real environment and the answer on a real pipe, and
// takes its answer. The command is this test binary; see TestMain.
func TestRecallAcceptsWhatRunWrites(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperEnv, "1")
	source := &credential.Exec{Command: []string{self}, Clock: &clock.Fake{T: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}}
	got, err := source.For(context.Background(), "example.com/you/example")
	if err != nil {
		t.Fatal(err)
	}
	want := credential.Credential{
		Token: "token-for-you-example", Expires: time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC),
		Name: "recall-you[bot]", Email: "recall-you[bot]@example.com", Owner: "you",
	}
	if got != want {
		t.Errorf("credential = %+v, want %+v", got, want)
	}

	// What the command refuses reaches recall as the command's own words.
	_, err = source.For(context.Background(), "example.org/you/example")
	if err == nil || !strings.Contains(err.Error(), "recall-credential: example.org/you/example is on example.org; this command covers example.com only") {
		t.Errorf("err = %v, want the command's reason", err)
	}
}
