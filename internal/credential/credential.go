// Package credential gets recall a credential for a hosted repository from
// outside itself: recall runs a configured command, hands it a request in
// its environment, and uses the token it prints. recall knows nothing about
// where the token comes from, so a GitHub App, a bot user's token in a password manager,
// or anything else that speaks the contract (docs/credentials.md) works.
// The contract's object is api/credentials/v1, which such a command can
// import.
//
// A token is held in memory until it is close to expiring and is never
// written anywhere.
package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/internal/clock"
)

// DefaultTimeout is how long the command gets to answer.
const DefaultTimeout = 30 * time.Second

// expirySlack is how long before its expiry a cached credential stops
// being used, so a gh or git process started with it has time to finish.
const expirySlack = time.Minute

// Credential is who recall acts as on one repository.
type Credential struct {
	// Token authenticates gh and git push. It is a secret: String and
	// GoString leave it out, and it is never marshaled to JSON.
	Token string `json:"-"`
	// Expires is when the token stops working; zero is unknown, and the
	// credential is kept for the run.
	Expires time.Time
	// Name and Email, when both are set, author recall's commits, so they
	// match whoever the token opens the pull request as.
	Name, Email string
	// Owner is the login of the person the credential acts for. A token
	// that belongs to a bot names the bot, so the credential says whose
	// review comments count as the owner's. Empty is unknown.
	Owner string
}

// String formats a credential without its token, so one that reaches a log
// or an error message by accident doesn't carry the secret. fmt calls it
// for %v, %+v, and %s. It can't help a credential reached through another
// struct's unexported field, which fmt prints without asking.
func (c Credential) String() string {
	token := "none"
	if c.Token != "" {
		token = "redacted"
	}
	expires := "unknown"
	if !c.Expires.IsZero() {
		expires = c.Expires.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("Credential{Token: %s, Expires: %s, Name: %q, Email: %q, Owner: %q}", token, expires, c.Name, c.Email, c.Owner)
}

// GoString is String for %#v, which would otherwise print every field.
func (c Credential) GoString() string { return c.String() }

// Source supplies a credential per repository.
type Source interface {
	// For returns the credential to act on repo with. repo is a slug like
	// github.com/you/example. A repo the source can't cover is an error:
	// recall never falls back to the machine's own login.
	For(ctx context.Context, repo string) (Credential, error)
}

// Exec is the Source that runs a command (review.credentials.command). It
// runs the command once per repository and reuses the answer until a
// minute before it expires, or for as long as the process lives when the
// answer names no expiry.
type Exec struct {
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Timeout bounds one run; zero is DefaultTimeout.
	Timeout time.Duration
	// Clock is the time source; nil is the system clock.
	Clock clock.Clock

	mu    sync.Mutex
	cache map[string]Credential
}

func (e *Exec) now() time.Time {
	if e.Clock != nil {
		return e.Clock.Now()
	}
	return time.Now()
}

func (e *Exec) For(ctx context.Context, repo string) (Credential, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.cache[repo]; ok && (c.Expires.IsZero() || e.now().Before(c.Expires.Add(-expirySlack))) {
		return c, nil
	}
	c, err := e.run(ctx, repo)
	if err != nil {
		return Credential{}, err
	}
	if e.cache == nil {
		e.cache = map[string]Credential{}
	}
	e.cache[repo] = c
	return c, nil
}

// run asks the command for repo's credential.
func (e *Exec) run(ctx context.Context, repo string) (Credential, error) {
	if len(e.Command) == 0 {
		return Credential{}, errors.New("no credential command is configured")
	}
	name := e.Command[0]
	fail := func(format string, args ...any) (Credential, error) {
		return Credential{}, fmt.Errorf("credential command %s for %s: %s", name, repo, fmt.Sprintf(format, args...))
	}
	in, err := json.Marshal(credentialsv1.Credential{
		APIVersion: credentialsv1.APIVersion, Kind: credentialsv1.Kind,
		Spec: credentialsv1.Spec{Repo: repo},
	})
	if err != nil {
		return Credential{}, err
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, e.Command[1:]...)
	// The request goes in the environment and nowhere else. The command's
	// stdin is empty: recall runs unattended, so there is no one to answer
	// a prompt.
	cmd.Env = append(os.Environ(), credentialsv1.ExecInfoEnv+"="+string(in))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	// A command that leaves a child holding its output open would
	// otherwise outlive the timeout.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fail("no answer after %s", timeout)
		}
		// The command's own words say why; its stdout is never shown,
		// since a failed run may still have printed a token.
		if detail := strings.TrimSpace(errOut.String()); detail != "" {
			return fail("%s", detail)
		}
		return fail("%v", err)
	}
	var o credentialsv1.Credential
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		return fail("its output isn't one JSON object: %v", err)
	}
	if o.APIVersion != credentialsv1.APIVersion || o.Kind != credentialsv1.Kind {
		return fail("answered %q %q, want %q %q", o.APIVersion, o.Kind, credentialsv1.APIVersion, credentialsv1.Kind)
	}
	if o.Status == nil || o.Status.Token == "" {
		return fail("answered with no status.token")
	}
	r := o.Status
	c := Credential{Token: r.Token, Owner: r.Owner}
	if r.ExpirationTimestamp != nil {
		c.Expires = *r.ExpirationTimestamp
		if !e.now().Before(c.Expires) {
			return fail("answered with a token that expired at %s", c.Expires.UTC().Format(time.RFC3339))
		}
	}
	if id := r.CommitIdentity; id != nil {
		if id.Name == "" || id.Email == "" {
			return fail("answered with a commitIdentity missing its name or email")
		}
		c.Name, c.Email = id.Name, id.Email
	}
	return c, nil
}

// Fake is an in-memory Source for tests. It records the repos it was
// asked about.
type Fake struct {
	// Credential is what every repo gets; Err fails For instead.
	Credential Credential
	Err        error
	// Asked lists each call's repo.
	Asked []string
}

func (f *Fake) For(ctx context.Context, repo string) (Credential, error) {
	f.Asked = append(f.Asked, repo)
	if f.Err != nil {
		return Credential{}, f.Err
	}
	return f.Credential, nil
}
