package review

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/micahhausler/recall/internal/credential"
)

// Committer is a publisher that knows who should author a change's commits.
// Empty answers keep the machine's git identity.
type Committer interface {
	Committer(ctx context.Context, w *Worktree) (name, email string, err error)
}

// tokenEnv is the variable that carries the token to gh, which reads it
// natively, and to git's credential helper below.
const tokenEnv = "GH_TOKEN"

// credentialHelper answers git's request for a password with the token in
// the environment. The token is never in this string, so it is never in
// argv, a remote URL, or a config file.
const credentialHelper = `!f() { test "$1" = get && printf 'username=x-access-token\npassword=%s\n' "$` + tokenEnv + `"; }; f`

// remoteAuth is what one repo's git and gh processes run with.
type remoteAuth struct {
	// slug is host/owner/name; url is the HTTPS remote pushes go to.
	slug, url string
	// git and gh are added to each process's environment.
	git, gh []string
	// owner is the login the credential acts for, or "".
	owner string
}

// splitSlug splits host/owner/name. ok is false for any other shape.
func splitSlug(slug string) (host, owner, name string, ok bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// auth gets slug's credential and builds the environment that carries its
// token. base is the remote's scheme and host, like https://github.com;
// empty derives it from the slug.
func auth(ctx context.Context, creds credential.Source, slug, base string) (*remoteAuth, error) {
	host, owner, name, ok := splitSlug(slug)
	if !ok {
		return nil, fmt.Errorf("%q isn't a hosted repository (want host/owner/name)", slug)
	}
	cred, err := creds.For(ctx, slug)
	if err != nil {
		return nil, err
	}
	token := cred.Token
	if token == "" {
		return nil, errors.New("the credentials returned an empty token for " + slug)
	}
	if base == "" {
		base = "https://" + host
	}
	a := &remoteAuth{slug: slug, url: base + "/" + owner + "/" + name + ".git", owner: cred.Owner}
	a.gh = []string{tokenEnv + "=" + token}
	a.git = append([]string{tokenEnv + "=" + token, "GIT_TERMINAL_PROMPT=0"}, configEnv(
		// Drop every configured helper first: one would offer the machine
		// owner's stored login, and after a successful push would be told
		// to store this token as if it were theirs.
		[2]string{"credential.helper", ""},
		// Answer only for this host, so the token can't follow a redirect
		// or a rewritten URL anywhere else.
		[2]string{"credential." + base + ".helper", credentialHelper},
		// Pin the URL to itself. The longest rewrite wins, so this beats a
		// broader rule in the owner's config, like one that turns every
		// https://github.com/ URL into SSH and would push as the owner.
		[2]string{"url." + a.url + ".insteadOf", a.url},
		[2]string{"url." + a.url + ".pushInsteadOf", a.url},
	)...)
	return a, nil
}

// pullRequestSlug reads host/owner/name from a pull request URL like
// https://github.com/you/example/pull/12.
func pullRequestSlug(where string) (slug string, ok bool) {
	slug, _, ok = pullRequestRef(where)
	return slug, ok
}

// pullRequestRef is pullRequestSlug with the pull request's number.
func pullRequestRef(where string) (slug, number string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(where))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || parts[3] == "" {
		return "", "", false
	}
	return strings.ToLower(u.Hostname()) + "/" + parts[0] + "/" + parts[1], parts[3], true
}
