package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
)

// defaultAPI is GitHub's REST root.
const defaultAPI = "https://api.github.com"

const (
	// jwtBackdate is how far in the past the JWT's iat sits, so a clock a
	// little ahead of GitHub's doesn't make it "issued in the future".
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how long past now the JWT stays valid. GitHub refuses
	// an exp more than ten minutes out.
	jwtLifetime = 9 * time.Minute
)

// app is one GitHub App: its client ID and private key, and where and how
// to reach GitHub. Tests point api and http at an httptest server.
type app struct {
	clientID string
	key      *rsa.PrivateKey
	api      string
	http     *http.Client
	now      func() time.Time
}

// jwt returns a JSON Web Token that authenticates as the App itself: RS256,
// issued by the client ID, issued a minute ago, and valid for nine more.
func (a *app) jwt() (string, error) {
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{a.clientID, now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix()})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing the App's JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// installationToken mints a token that reaches only owner/repo, with only
// the permissions this command asks for, and returns it with its expiry.
// A token GitHub scoped to anything but owner/repo is an error and is not
// returned.
func (a *app) installationToken(ctx context.Context, owner, repo string) (string, time.Time, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", time.Time{}, err
	}
	full := owner + "/" + repo
	var in struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if err := a.call(ctx, http.MethodGet, path, jwt, nil, &in); err != nil {
		if e := (*apiError)(nil); errors.As(err, &e) && e.status == http.StatusNotFound {
			return "", time.Time{}, fmt.Errorf("the App isn't installed on %s, or the repository doesn't exist", full)
		}
		return "", time.Time{}, err
	}
	body := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{[]string{repo}, permissions}
	var out struct {
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expires_at"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", in.ID), jwt, body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("minting a token for %s: %w", full, err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("minting a token for %s: GitHub returned no token", full)
	}
	if len(out.Repositories) != 1 || !strings.EqualFold(out.Repositories[0].FullName, full) {
		var got []string
		for _, r := range out.Repositories {
			got = append(got, r.FullName)
		}
		return "", time.Time{}, fmt.Errorf("minting a token for %s: GitHub scoped it to %v, not to %s alone; the token is not used", full, got, full)
	}
	return out.Token, out.ExpiresAt, nil
}

// botIdentity is the git author that makes GitHub show a commit as the
// App's bot user: <slug>[bot] and <id>+<slug>[bot]@users.noreply.github.com.
// token is the installation token; it only keeps the user lookup off the
// anonymous rate limit.
func (a *app) botIdentity(ctx context.Context, token string) (*credentialsv1.CommitIdentity, error) {
	jwt, err := a.jwt()
	if err != nil {
		return nil, err
	}
	var info struct {
		Slug string `json:"slug"`
	}
	if err := a.call(ctx, http.MethodGet, "/app", jwt, nil, &info); err != nil {
		return nil, err
	}
	login := info.Slug + "[bot]"
	var user struct {
		ID int64 `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, "/users/"+url.PathEscape(login), token, nil, &user); err != nil {
		return nil, err
	}
	return &credentialsv1.CommitIdentity{Name: login, Email: fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, login)}, nil
}

// apiError is a call GitHub answered with a failure. It holds the path and
// GitHub's message, never a bearer.
type apiError struct {
	method, path string
	status       int
	message      string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.method, e.path, e.status, e.message)
}

// call makes one GitHub REST call with bearer in the Authorization header,
// body as JSON when not nil, and decodes the answer into out.
func (a *app) call(ctx context.Context, method, path, bearer string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.api, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", name)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &apiError{method: method, path: path, status: resp.StatusCode, message: e.Message}
	}
	return json.Unmarshal(data, out)
}
