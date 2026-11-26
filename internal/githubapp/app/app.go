// Package app acts on GitHub as a GitHub App: it signs the app's JWT with a
// private key it never sees (a crypto.Signer, so the key can live in a
// TPM), finds the app's installation on a repository, and mints one-hour
// installation tokens scoped to that one repository.
//
// Nothing here stores a token: each call that needs one mints it.
package app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/githubapp/ghapi"
)

// DefaultAPI is GitHub's REST root.
const DefaultAPI = ghapi.DefaultAPI

const (
	// jwtBackdate is how far in the past the JWT's iat sits, so a clock a
	// little ahead of GitHub's doesn't make the token "issued in the future".
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how long past now the JWT stays valid. GitHub refuses
	// an exp more than ten minutes out; a JWT is used within seconds.
	jwtLifetime = 9 * time.Minute
	// perPage is the most GitHub returns in one page of a list.
	perPage = 100
)

// ErrNotInstalled reports that the app isn't installed on a repository, or
// the repository doesn't exist: GitHub answers both the same way.
var ErrNotInstalled = errors.New("the GitHub App isn't installed on the repository, or the repository doesn't exist")

// App is one GitHub App, identified by its client ID and private key.
type App struct {
	// ClientID is the app's client ID, the JWT's issuer.
	ClientID string
	// Key signs the JWT. GitHub Apps use RSA keys; the key is asked for
	// PKCS #1 v1.5 over SHA-256, which is RS256. A key that takes the
	// message whole (a crypto.MessageSigner) gets it that way.
	Key crypto.Signer
	// API is the REST root; empty is DefaultAPI.
	API string
	// HTTP is the client; nil is http.DefaultClient.
	HTTP *http.Client
	// Now is the time source; nil is time.Now.
	Now func() time.Time

	// Signing can cost seconds when the key lives in a TPM, and may ask a
	// person when it lives in an agent, so one JWT serves every call made
	// while it is fresh. An App is used from one goroutine.
	jwt        string
	jwtExpires time.Time
	info       *Info
}

// Info is what GitHub says about the app itself.
type Info struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	HTMLURL  string `json:"html_url"`
	// Owner is the account that registered the app.
	Owner       Account           `json:"owner"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
}

// Account is a GitHub user or organization.
type Account struct {
	Login string `json:"login"`
	// Type is "User" or "Organization".
	Type string `json:"type"`
}

// User is a GitHub user, as the commit identity needs it.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Installation is the app installed on one account.
type Installation struct {
	ID      int64   `json:"id"`
	Account Account `json:"account"`
	// RepositorySelection is "all" or "selected".
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
}

// Token is a short-lived installation token.
type Token struct {
	// Value is the token itself. It is a secret: String and GoString
	// leave it out, and it is never marshaled to JSON.
	Value       string `json:"-"`
	Expires     time.Time
	Permissions map[string]string
}

// String formats a token without its value, so one that reaches a log or
// an error message by accident doesn't carry the secret. fmt calls it for
// %v, %+v, and %s. It can't help a token reached through another struct's
// unexported field, which fmt prints without asking.
func (t Token) String() string {
	value := "none"
	if t.Value != "" {
		value = "redacted"
	}
	return fmt.Sprintf("Token{Value: %s, Expires: %s, Permissions: %v}", value, t.Expires.UTC().Format(time.RFC3339), t.Permissions)
}

// GoString is String for %#v, which would otherwise print every field.
func (t Token) GoString() string { return t.String() }

// tokenResponse is GitHub's answer to a token request. It is a separate
// type so that Token itself never reads or writes its value as JSON.
type tokenResponse struct {
	Token        string            `json:"token"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

func (r tokenResponse) token() Token {
	return Token{Value: r.Token, Expires: r.ExpiresAt, Permissions: r.Permissions}
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// jwtReuse is how much life a cached JWT must have left to be used again.
const jwtReuse = time.Minute

// JWT returns a signed JSON Web Token that authenticates as the app itself:
// RS256, issued by the client ID, issued a minute ago, and valid for nine
// more. A JWT already signed is reused until its last minute.
func (a *App) JWT() (string, error) {
	if a.jwt != "" && a.now().Before(a.jwtExpires.Add(-jwtReuse)) {
		return a.jwt, nil
	}
	jwt, expires, err := a.sign()
	if err != nil {
		return "", err
	}
	a.jwt, a.jwtExpires = jwt, expires
	return jwt, nil
}

// sign makes a new JWT.
func (a *App) sign() (string, time.Time, error) {
	if a.ClientID == "" {
		return "", time.Time{}, errors.New("github app: no client ID")
	}
	if a.Key == nil {
		return "", time.Time{}, errors.New("github app: no private key")
	}
	if _, ok := a.Key.Public().(*rsa.PublicKey); !ok {
		return "", time.Time{}, fmt.Errorf("github app: the key is %T; GitHub Apps sign with RSA", a.Key.Public())
	}
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{a.ClientID, now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sig, err := crypto.SignMessage(a.Key, rand.Reader, []byte(signing), crypto.SHA256)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: signing the JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), now.Add(jwtLifetime), nil
}

// Info asks GitHub about the app, once per process.
func (a *App) Info(ctx context.Context) (*Info, error) {
	if a.info != nil {
		return a.info, nil
	}
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	var info Info
	if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodGet, "/app", jwt, nil, &info); err != nil {
		return nil, err
	}
	a.info = &info
	return a.info, nil
}

// Installations lists the accounts the app is installed on.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	var all []Installation
	for page := 1; ; page++ {
		var list []Installation
		if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodGet, fmt.Sprintf("/app/installations?per_page=%d&page=%d", perPage, page), jwt, nil, &list); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < perPage {
			return all, nil
		}
	}
}

// installation finds the app's installation on owner/repo.
// ErrNotInstalled says there is none.
func (a *App) installation(ctx context.Context, owner, repo string) (*Installation, error) {
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	var in Installation
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodGet, path, jwt, nil, &in); err != nil {
		var apiErr *ghapi.Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%s/%s: %w", owner, repo, ErrNotInstalled)
		}
		return nil, err
	}
	return &in, nil
}

// Token mints an installation token that can reach only owner/repo, with
// only the given permissions (like "contents": "write"). What GitHub says
// it granted is checked against what was asked: a token for another
// repository, for more than one, or with other permissions is an error
// and is not returned. Asking for a permission the installation lacks is
// an error from GitHub.
func (a *App) Token(ctx context.Context, owner, repo string, permissions map[string]string) (Token, error) {
	if len(permissions) == 0 {
		return Token{}, errors.New("minting a token: no permissions named; a token is never minted with all the installation holds")
	}
	in, err := a.installation(ctx, owner, repo)
	if err != nil {
		return Token{}, err
	}
	want := owner + "/" + repo
	// GitHub follows a renamed or transferred repository to wherever it
	// lives now; the installation must still be the asked-for owner's.
	if !strings.EqualFold(in.Account.Login, owner) {
		return Token{}, fmt.Errorf("minting a token for %s: its installation belongs to %s", want, in.Account.Login)
	}
	jwt, err := a.JWT()
	if err != nil {
		return Token{}, err
	}
	body := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{[]string{repo}, permissions}
	var r tokenResponse
	path := fmt.Sprintf("/app/installations/%d/access_tokens", in.ID)
	if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodPost, path, jwt, body, &r); err != nil {
		return Token{}, fmt.Errorf("minting a token for %s: %w", want, err)
	}
	if r.Token == "" {
		return Token{}, fmt.Errorf("minting a token for %s: GitHub returned no token", want)
	}
	if err := r.scopedTo(want, permissions); err != nil {
		return Token{}, fmt.Errorf("minting a token for %s: %w; the token is not used", want, err)
	}
	return r.token(), nil
}

// Revoke ends a token now instead of leaving it to expire, for a token
// that was minted and has no further use.
func (a *App) Revoke(ctx context.Context, t Token) error {
	if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodDelete, "/installation/token", t.Value, nil, nil); err != nil {
		return fmt.Errorf("revoking a token: %w", err)
	}
	return nil
}

// scopedTo reports how the token GitHub says it minted differs from the
// one asked for. GitHub adds metadata: read to every token.
func (r tokenResponse) scopedTo(repo string, permissions map[string]string) error {
	if len(r.Repositories) != 1 || !strings.EqualFold(r.Repositories[0].FullName, repo) {
		var got []string
		for _, g := range r.Repositories {
			got = append(got, g.FullName)
		}
		return fmt.Errorf("GitHub scoped the token to %d repositories %v, not to %s alone", len(got), got, repo)
	}
	for name, level := range permissions {
		if r.Permissions[name] != level {
			return fmt.Errorf("GitHub granted %s: %q, not the %q asked for", name, r.Permissions[name], level)
		}
	}
	for name, level := range r.Permissions {
		if _, asked := permissions[name]; !asked && !(name == "metadata" && level == "read") {
			return fmt.Errorf("GitHub granted %s: %s, which wasn't asked for", name, level)
		}
	}
	return nil
}

// Repositories lists the repositories one installation covers, as
// owner/name. It mints a token that can read their metadata and nothing
// else, and revokes it when the listing is done.
func (a *App) Repositories(ctx context.Context, installation int64) (names []string, err error) {
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	body := struct {
		Permissions map[string]string `json:"permissions"`
	}{map[string]string{"metadata": "read"}}
	var r tokenResponse
	if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installation), jwt, body, &r); err != nil {
		return nil, err
	}
	t := r.token()
	// The token was only for this listing; it doesn't outlive it.
	defer func() {
		if rerr := a.Revoke(ctx, t); rerr != nil && err == nil {
			names, err = nil, rerr
		}
	}()
	for page := 1; ; page++ {
		var list struct {
			Total        int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", perPage, page), t.Value, nil, &list); err != nil {
			return nil, err
		}
		for _, r := range list.Repositories {
			names = append(names, r.FullName)
		}
		if len(list.Repositories) == 0 || len(names) >= list.Total {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}

// Bot is the app's bot user, <slug>[bot], whose ID its commit email
// carries. token is any installation token; GitHub's user lookup is public,
// and the token only keeps it off the anonymous rate limit.
func (a *App) Bot(ctx context.Context, token string) (*User, error) {
	info, err := a.Info(ctx)
	if err != nil {
		return nil, err
	}
	var u User
	if err := ghapi.Call(ctx, a.HTTP, a.API, http.MethodGet, "/users/"+url.PathEscape(info.Slug+"[bot]"), token, nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// CommitIdentity is the git author name and email that make GitHub show a
// commit as the bot user's: <login> and <id>+<login>@users.noreply.github.com.
func (u *User) CommitIdentity() (name, email string) {
	return u.Login, fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
}

// Grants reports whether holding a permission at level have covers asking
// for it at level want: the same level, or write, which includes read.
func Grants(have, want string) bool {
	return have == want || have == "write"
}
