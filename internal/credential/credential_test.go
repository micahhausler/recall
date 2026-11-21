package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
)

// command writes a shell script that records each request it reads and
// then runs body, and returns the script and the path of its request log.
func command(t *testing.T, body string) (cmd []string, log string) {
	t.Helper()
	dir := t.TempDir()
	script, log := filepath.Join(dir, "cred"), filepath.Join(dir, "requests")
	text := "#!/bin/sh\ncat >> '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(script, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{script, "token"}, log
}

func requests(t *testing.T, log string) []object {
	t.Helper()
	data, _ := os.ReadFile(log)
	var out []object
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r object
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("request %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

const full = `cat <<'JSON'
{
  "apiVersion": "recall.credentials/v1",
  "kind": "Credential",
  "spec": {"repo": "github.com/you/dotfiles"},
  "status": {
    "token": "ghs_fake",
    "expirationTimestamp": "2026-03-02T13:00:00Z",
    "commitIdentity": {"name": "recall-you[bot]", "email": "4242+recall-you[bot]@users.noreply.github.com"},
    "owner": "you",
    "somethingNewer": true
  },
  "somethingNewerStill": {}
}
JSON`

func TestExecRunsTheContract(t *testing.T) {
	cmd, log := command(t, full)
	clk := &clock.Fake{T: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}
	e := &Exec{Command: cmd, Clock: clk}
	c, err := e.For(context.Background(), "github.com/you/dotfiles")
	if err != nil {
		t.Fatal(err)
	}
	want := Credential{Token: "ghs_fake", Expires: time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC),
		Name: "recall-you[bot]", Email: "4242+recall-you[bot]@users.noreply.github.com", Owner: "you"}
	if c != want {
		t.Errorf("credential = %+v, want %+v", c, want)
	}
	reqs := requests(t, log)
	if len(reqs) != 1 || reqs[0] != (object{APIVersion: "recall.credentials/v1", Kind: "Credential", Spec: spec{Repo: "github.com/you/dotfiles"}}) {
		t.Errorf("requests = %+v, want the object with only spec set", reqs)
	}
}

// One run per repository, reused until a minute before it expires.
func TestExecCachesByRepoUntilNearExpiry(t *testing.T) {
	ctx := context.Background()
	cmd, log := command(t, full)
	clk := &clock.Fake{T: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}
	e := &Exec{Command: cmd, Clock: clk}
	for range 3 {
		if _, err := e.For(ctx, "github.com/you/dotfiles"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.For(ctx, "github.com/you/kit"); err != nil {
		t.Fatal(err)
	}
	if n := len(requests(t, log)); n != 2 {
		t.Fatalf("the command ran %d times, want once per repo", n)
	}
	clk.T = time.Date(2026, 3, 2, 12, 58, 59, 0, time.UTC)
	if _, err := e.For(ctx, "github.com/you/dotfiles"); err != nil || len(requests(t, log)) != 2 {
		t.Errorf("ran again with over a minute left: %v", err)
	}
	clk.T = time.Date(2026, 3, 2, 12, 59, 1, 0, time.UTC)
	// Inside the last minute the token is asked for again; this command
	// hands back the same expiry, which is still ahead.
	if _, err := e.For(ctx, "github.com/you/dotfiles"); err != nil || len(requests(t, log)) != 3 {
		t.Errorf("inside the last minute: err %v, %d runs; want a fresh run", err, len(requests(t, log)))
	}
}

// Only the token is required; with no expiry the credential lasts the run.
func TestExecMinimalAnswerLastsTheRun(t *testing.T) {
	cmd, log := command(t, `echo '{"apiVersion":"recall.credentials/v1","kind":"Credential","status":{"token":"t"}}'`)
	clk := &clock.Fake{T: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}
	e := &Exec{Command: cmd, Clock: clk}
	c, err := e.For(context.Background(), "github.com/you/dotfiles")
	if err != nil || c != (Credential{Token: "t"}) {
		t.Fatalf("credential = %+v, %v", c, err)
	}
	clk.T = clk.T.Add(48 * time.Hour)
	if _, err := e.For(context.Background(), "github.com/you/dotfiles"); err != nil || len(requests(t, log)) != 1 {
		t.Errorf("a credential with no expiry was asked for again: %v", err)
	}
}

// A failed command fails the publish with its stderr, and a token it may
// have printed on the way down is never shown.
func TestExecFailureShowsStderrNeverStdout(t *testing.T) {
	cmd, _ := command(t, "echo 'ghs_leaked'\necho 'the app is not installed on you/dotfiles' >&2\nexit 3")
	_, err := (&Exec{Command: cmd}).For(context.Background(), "github.com/you/dotfiles")
	if err == nil || !strings.Contains(err.Error(), "the app is not installed on you/dotfiles") || !strings.Contains(err.Error(), "github.com/you/dotfiles") {
		t.Fatalf("err = %v, want the command's stderr and the repo", err)
	}
	if strings.Contains(err.Error(), "ghs_leaked") {
		t.Errorf("the error shows the command's stdout: %v", err)
	}
}

func TestExecRejectsAnswersOutsideTheContract(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}
	for name, tc := range map[string]struct{ body, want string }{
		"not JSON":        {`echo 'ghs_justatoken'`, "isn't one JSON object"},
		"no status":       {`echo '{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/dotfiles"}}'`, "no status.token"},
		"no token":        {`echo '{"apiVersion":"recall.credentials/v1","kind":"Credential","status":{}}'`, "no status.token"},
		"a flat token":    {`echo '{"apiVersion":"recall.credentials/v1","kind":"Credential","token":"t"}'`, "no status.token"},
		"another version": {`echo '{"apiVersion":"recall.credentials/v2","kind":"Credential","status":{"token":"t"}}'`, `want "recall.credentials/v1" "Credential"`},
		"another kind":    {`echo '{"apiVersion":"recall.credentials/v1","kind":"ExecCredential","status":{"token":"t"}}'`, `want "recall.credentials/v1" "Credential"`},
		"already expired": {`echo '{"apiVersion":"recall.credentials/v1","kind":"Credential","status":{"token":"t","expirationTimestamp":"2026-03-02T11:00:00Z"}}'`, "expired at 2026-03-02T11:00:00Z"},
		"half identity":   {`echo '{"apiVersion":"recall.credentials/v1","kind":"Credential","status":{"token":"t","commitIdentity":{"name":"bot"}}}'`, "missing its name or email"},
		"nothing":         {`true`, "isn't one JSON object"},
	} {
		cmd, _ := command(t, tc.body)
		_, err := (&Exec{Command: cmd, Clock: clk}).For(context.Background(), "github.com/you/dotfiles")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "ghs_justatoken") {
			t.Errorf("%s: the error shows the command's output: %v", name, err)
		}
	}
	if _, err := (&Exec{}).For(context.Background(), "github.com/you/dotfiles"); err == nil {
		t.Error("an Exec with no command answered")
	}
	if _, err := (&Exec{Command: []string{filepath.Join(t.TempDir(), "missing")}}).For(context.Background(), "github.com/you/dotfiles"); err == nil {
		t.Error("a missing command answered")
	}
}

func TestExecTimesOut(t *testing.T) {
	cmd, _ := command(t, "sleep 30")
	start := time.Now()
	_, err := (&Exec{Command: cmd, Timeout: 200 * time.Millisecond}).For(context.Background(), "github.com/you/dotfiles")
	if err == nil || !strings.Contains(err.Error(), "no answer after 200ms") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the timeout took %s", took)
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Credential: Credential{Token: "t"}}
	if c, err := f.For(context.Background(), "github.com/you/dotfiles"); err != nil || c.Token != "t" || len(f.Asked) != 1 {
		t.Errorf("fake = %+v, %v, asked %v", c, err, f.Asked)
	}
}

// A credential that is formatted or marshaled by accident doesn't carry its
// token, by value or by pointer, alone or inside something else.
func TestCredentialKeepsItsTokenOutOfFormattingAndJSON(t *testing.T) {
	c := Credential{Token: "ghs_secret", Expires: time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC),
		Name: "recall-you[bot]", Email: "4242+recall-you[bot]@users.noreply.github.com", Owner: "you"}
	want := `Credential{Token: redacted, Expires: 2026-03-02T13:00:00Z, Name: "recall-you[bot]", Email: "4242+recall-you[bot]@users.noreply.github.com", Owner: "you"}`
	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		for name, arg := range map[string]any{"a value": c, "a pointer": &c} {
			if got := fmt.Sprintf(verb, arg); got != want {
				t.Errorf("%s of %s = %s, want %s", verb, name, got, want)
			}
		}
	}
	type holder struct {
		Cred  Credential
		List  []Credential
		ByKey map[string]Credential
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(verb, holder{Cred: c, List: []Credential{c}, ByKey: map[string]Credential{"k": c}})
		if strings.Contains(got, "ghs_secret") || strings.Count(got, "redacted") != 3 {
			t.Errorf("%s of a struct holding credentials = %s", verb, got)
		}
	}
	if err := fmt.Errorf("publishing with %v: %w", c, os.ErrPermission); strings.Contains(err.Error(), "ghs_secret") {
		t.Errorf("an error carries the token: %v", err)
	}
	data, err := json.Marshal(struct {
		Cred Credential
		Ptr  *Credential
	}{c, &c})
	if err != nil || strings.Contains(string(data), "ghs_secret") || !strings.Contains(string(data), `"Owner":"you"`) {
		t.Errorf("JSON = %s, %v; want the credential without its token", data, err)
	}
	// An empty credential says so, and doesn't claim a token it lacks.
	if got := fmt.Sprint(Credential{}); got != `Credential{Token: none, Expires: unknown, Name: "", Email: "", Owner: ""}` {
		t.Errorf("zero credential = %s", got)
	}
}
