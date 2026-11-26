package app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/githubapp/app/apptest"
	"github.com/micahhausler/recall/internal/githubapp/ghapi"
)

// testKey is one RSA key for the whole package's tests; generating one is
// the slow part.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// fakeClock is a settable clock.
type fakeClock struct{ T time.Time }

func (c *fakeClock) Now() time.Time { return c.T }

// newApp starts a fake GitHub and an app that talks to it. The fake's own
// complaints about a JWT fail the test.
func newApp(t *testing.T) (*App, *apptest.Fake, *fakeClock) {
	t.Helper()
	clk := &fakeClock{T: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)}
	g := &apptest.Fake{Key: &testKey().PublicKey, Now: clk.Now, Slug: "recall-you", Owner: "you", BotID: 4242, ClientID: "Iv23liEXAMPLE",
		Installed: map[string]int64{"you/dotfiles": 99},
		Granted:   map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for _, p := range g.Problems {
			t.Errorf("fake GitHub: %s", p)
		}
	})
	return &App{ClientID: "Iv23liEXAMPLE", Key: testKey(), API: srv.URL, HTTP: srv.Client(), Now: clk.Now}, g, clk
}

func TestJWTClaims(t *testing.T) {
	app, g, clk := newApp(t)
	if _, err := app.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.Claims) != 1 {
		t.Fatalf("GitHub saw %d JWTs, want 1", len(g.Claims))
	}
	c := g.Claims[0]
	if c["iss"] != "Iv23liEXAMPLE" {
		t.Errorf("iss = %v, want the client ID", c["iss"])
	}
	iat, exp, now := int64(c["iat"].(float64)), int64(c["exp"].(float64)), clk.T.Unix()
	if iat >= now {
		t.Errorf("iat = %d, want before now (%d): a clock ahead of GitHub's must not issue a token from the future", iat, now)
	}
	if exp <= now || exp > now+600 {
		t.Errorf("exp = %d, want within ten minutes after now (%d)", exp, now)
	}
}

func TestJWTRefusesMissingOrNonRSAKey(t *testing.T) {
	if _, err := (&App{ClientID: "x"}).JWT(); err == nil {
		t.Error("JWT with no key succeeded")
	}
	if _, err := (&App{Key: testKey()}).JWT(); err == nil {
		t.Error("JWT with no client ID succeeded")
	}
}

func TestTokenScopedToOneRepo(t *testing.T) {
	ctx := context.Background()
	app, g, clk := newApp(t)
	perms := map[string]string{"contents": "write", "pull_requests": "write"}
	tok, err := app.Token(ctx, "you", "dotfiles", perms)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "ghs_fake1" || !tok.Expires.Equal(clk.T.Add(time.Hour)) {
		t.Errorf("token = %+v", tok)
	}
	if len(g.Requests) != 1 {
		t.Fatalf("GitHub minted %d tokens, want 1", len(g.Requests))
	}
	req := g.Requests[0]
	if req.Installation != "99" || len(req.Repositories) != 1 || req.Repositories[0] != "dotfiles" {
		t.Errorf("token request = %+v, want installation 99 scoped to the one repository", req)
	}
	if len(req.Permissions) != 2 || req.Permissions["contents"] != "write" || req.Permissions["pull_requests"] != "write" {
		t.Errorf("token permissions = %v, want exactly contents and pull_requests", req.Permissions)
	}

	// Nothing is kept: a second ask mints a second token.
	if again, err := app.Token(ctx, "you", "dotfiles", perms); err != nil || again.Value == tok.Value || len(g.Requests) != 2 {
		t.Errorf("second ask = %q, %v, %d minted; want a token of its own", again.Value, err, len(g.Requests))
	}
}

func TestTokenNotInstalled(t *testing.T) {
	app, g, _ := newApp(t)
	_, err := app.Token(context.Background(), "you", "elsewhere", map[string]string{"contents": "write"})
	if !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "you/elsewhere") {
		t.Fatalf("err = %v, want ErrNotInstalled naming the repo", err)
	}
	if len(g.Requests) != 0 {
		t.Error("a token was minted for a repo the app isn't installed on")
	}
}

// A permission the installation lacks is GitHub's error, passed on: never a
// token quietly missing it.
func TestTokenPermissionNotGranted(t *testing.T) {
	app, _, _ := newApp(t)
	_, err := app.Token(context.Background(), "you", "dotfiles", map[string]string{"checks": "read"})
	var apiErr *ghapi.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 422 || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("err = %v, want GitHub's 422", err)
	}
}

func TestBotCommitIdentity(t *testing.T) {
	ctx := context.Background()
	app, g, _ := newApp(t)
	bot, err := app.Bot(ctx, "ghs_any")
	if err != nil {
		t.Fatal(err)
	}
	name, email := bot.CommitIdentity()
	if name != "recall-you[bot]" || email != "4242+recall-you[bot]@users.noreply.github.com" {
		t.Errorf("identity = %q <%s>", name, email)
	}
	if g.UserLookups != 1 {
		t.Errorf("%d user lookups, want 1", g.UserLookups)
	}
}

func TestRepositoriesListsOneInstallation(t *testing.T) {
	app, g, _ := newApp(t)
	g.Installed["you/notes"] = 99
	g.Installed["example-org/site"] = 100
	repos, err := app.Repositories(context.Background(), 99)
	if err != nil || strings.Join(repos, ",") != "you/dotfiles,you/notes" {
		t.Fatalf("repositories = %v, %v", repos, err)
	}
	req := g.Requests[len(g.Requests)-1]
	if len(req.Permissions) != 1 || req.Permissions["metadata"] != "read" {
		t.Errorf("listing minted a token with %v, want metadata read only", req.Permissions)
	}
	if live := g.Live(); len(live) != 0 {
		t.Errorf("listing left its token live: %+v", live)
	}

	// A listing whose token couldn't be revoked is a failure, not a list.
	g.NoRevoke = true
	if repos, err := app.Repositories(context.Background(), 99); err == nil || repos != nil {
		t.Errorf("with revoking refused: repositories = %v, %v; want an error and no list", repos, err)
	}
}

func TestRevokeEndsAToken(t *testing.T) {
	ctx := context.Background()
	app, g, _ := newApp(t)
	tok, err := app.Token(ctx, "you", "dotfiles", map[string]string{"contents": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Revoke(ctx, tok); err != nil || len(g.Live()) != 0 {
		t.Fatalf("revoke = %v, %d live; want the token ended", err, len(g.Live()))
	}
	// GitHub refuses a token that is already gone, and the error never
	// carries the token.
	err = app.Revoke(ctx, tok)
	if err == nil || strings.Contains(err.Error(), tok.Value) {
		t.Errorf("revoking twice = %v; want an error without the token", err)
	}
}

// countingSigner counts signatures: each one may be a slow trip to a TPM.
type countingSigner struct {
	*rsa.PrivateKey
	signs int
}

func (c *countingSigner) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	c.signs++
	return c.PrivateKey.Sign(r, digest, opts)
}

// One JWT serves every call while it is fresh; it is signed again only in
// its last minute.
func TestJWTIsSignedOncePerLifetime(t *testing.T) {
	ctx := context.Background()
	app, _, clk := newApp(t)
	signer := &countingSigner{PrivateKey: testKey()}
	app.Key = signer
	if _, err := app.Info(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Token(ctx, "you", "dotfiles", map[string]string{"contents": "write"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Installations(ctx); err != nil {
		t.Fatal(err)
	}
	if signer.signs != 1 {
		t.Errorf("signed %d JWTs for four calls a second apart, want 1", signer.signs)
	}
	clk.T = clk.T.Add(jwtLifetime - jwtReuse + time.Second)
	if _, err := app.Installations(ctx); err != nil {
		t.Fatal(err)
	}
	if signer.signs != 2 {
		t.Errorf("signed %d JWTs after the first one's last minute began, want 2", signer.signs)
	}
}

// A token that is formatted or marshaled by accident doesn't carry its
// value, by value or by pointer, alone or inside something else.
func TestTokenKeepsItsValueOutOfFormattingAndJSON(t *testing.T) {
	app, _, _ := newApp(t)
	tok, err := app.Token(context.Background(), "you", "dotfiles", map[string]string{"contents": "write"})
	if err != nil || tok.Value != "ghs_fake1" {
		t.Fatalf("token minted wrong: value %d chars, %v", len(tok.Value), err)
	}
	want := "Token{Value: redacted, Expires: 2026-03-02T13:00:00Z, Permissions: map[contents:write metadata:read]}"
	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		for name, arg := range map[string]any{"a value": tok, "a pointer": &tok} {
			if got := fmt.Sprintf(verb, arg); got != want {
				t.Errorf("%s of %s = %s, want %s", verb, name, got, want)
			}
		}
	}
	type holder struct {
		Tok  Token
		List []Token
	}
	if got := fmt.Sprintf("%+v", holder{tok, []Token{tok}}); strings.Contains(got, "ghs_fake1") || strings.Count(got, "redacted") != 2 {
		t.Errorf("a struct holding tokens = %s", got)
	}
	if err := fmt.Errorf("using %v: %w", tok, ErrNotInstalled); strings.Contains(err.Error(), "ghs_fake1") {
		t.Errorf("an error carries the token: %v", err)
	}
	data, err := json.Marshal(struct {
		Tok Token
		Ptr *Token
	}{tok, &tok})
	if err != nil || strings.Contains(string(data), "ghs_fake1") || !strings.Contains(string(data), `"Permissions":{"contents":"write","metadata":"read"}`) {
		t.Errorf("JSON = %s, %v; want the token without its value", data, err)
	}
	if got := fmt.Sprint(Token{}); !strings.HasPrefix(got, "Token{Value: none,") {
		t.Errorf("zero token = %s", got)
	}
}

// What GitHub says it granted is checked against what was asked, and a
// token that reaches further is refused, not handed on.
func TestTokenBroaderThanAskedIsRefused(t *testing.T) {
	perms := map[string]string{"contents": "write"}
	for name, c := range map[string]struct {
		break_ func(*apptest.Fake)
		want   string
	}{
		"another permission": {func(g *apptest.Fake) { g.AlsoGrants = map[string]string{"administration": "write"} }, "administration: write, which wasn't asked for"},
		"a weaker level":     {func(g *apptest.Fake) { g.AlsoGrants = map[string]string{"contents": "read"} }, `contents: "read", not the "write" asked for`},
		"a second repo":      {func(g *apptest.Fake) { g.AlsoReaches = []string{"you/secrets"} }, "not to you/dotfiles alone"},
		"another account":    {func(g *apptest.Fake) { g.InstalledAs = "someone-else" }, "its installation belongs to someone-else"},
	} {
		app, g, _ := newApp(t)
		c.break_(g)
		tok, err := app.Token(context.Background(), "you", "dotfiles", perms)
		if err == nil || tok.Value != "" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: token %v, err %v; want an error holding %q", name, tok, err, c.want)
		}
	}
	// A token with every permission the installation holds is never asked for.
	app, g, _ := newApp(t)
	if _, err := app.Token(context.Background(), "you", "dotfiles", nil); err == nil || len(g.Requests) != 0 {
		t.Errorf("a token with no permissions named: err %v, %d minted", err, len(g.Requests))
	}
}
