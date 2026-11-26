// Package apptest is an in-memory GitHub for tests.
package apptest

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory GitHub for tests: the handful of endpoints an app
// calls, behind the checks GitHub makes. It verifies every app-level call's
// JWT against Key, refuses a token request beyond what the installation
// was granted, and records what it was asked. Serve its Handler from an
// httptest server and point App.API there.
type Fake struct {
	// Key verifies the app's JWTs.
	Key *rsa.PublicKey
	// Now is the fake's clock, for a token's expiry; nil is time.Now.
	Now func() time.Time
	// Slug is the app's slug and Owner the account that registered it;
	// OwnerType is "User" unless set.
	Slug, Owner, OwnerType string
	// BotID is the ID of the app's bot user, <Slug>[bot].
	BotID int64
	// Installed maps "owner/repo" to the installation ID covering it.
	Installed map[string]int64
	// Granted is what every installation holds.
	Granted map[string]string
	// ClientID, when set, is the issuer every JWT must name.
	ClientID string
	// AlsoGrants is added to every token's permissions beyond what was
	// asked, and AlsoReaches to its repositories: a GitHub that hands out
	// more than it was asked for.
	AlsoGrants  map[string]string
	AlsoReaches []string
	// Selection is each installation's repository_selection; "selected"
	// unless set.
	Selection string
	// NoBot makes the bot user lookup fail, as a rate limit would.
	NoBot bool
	// InstalledAs, when set, is the account every installation reports,
	// whatever owner was asked about.
	InstalledAs string
	// NoRevoke makes revoking a token fail.
	NoRevoke bool
	// Faults maps "METHOD /path" to failures answered in turn, one per
	// call, before the fake answers normally.
	Faults map[string][]Fault

	mu sync.Mutex
	// Claims are the claims of each JWT that verified, in order.
	Claims []map[string]any
	// Requests are the token requests granted, in order.
	Requests []FakeTokenRequest
	// Problems lists each malformed or unverifiable JWT seen.
	Problems []string
	// UserLookups counts bot user lookups.
	UserLookups int
	// Calls counts each "METHOD /path" seen.
	Calls map[string]int
}

// Fault is one failed answer.
type Fault struct {
	Status int
	Header map[string]string
	// Handled answers the call first and drops its answer: GitHub did the
	// work, and the response was lost.
	Handled bool
}

// CallCount is how many times "METHOD /path" was called.
func (f *Fake) CallCount(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[call]
}

func (f *Fake) fault(call string) (Fault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Calls == nil {
		f.Calls = map[string]int{}
	}
	f.Calls[call]++
	q := f.Faults[call]
	if len(q) == 0 {
		return Fault{}, false
	}
	f.Faults[call] = q[1:]
	return q[0], true
}

// FakeTokenRequest is one installation token request the fake granted.
type FakeTokenRequest struct {
	Installation string            `json:"-"`
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
	// Token is the token handed back.
	Token string `json:"-"`
	// Revoked is whether the token was revoked since.
	Revoked bool `json:"-"`
}

// Live lists the granted requests whose tokens still work.
func (f *Fake) Live() []FakeTokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []FakeTokenRequest
	for _, req := range f.Requests {
		if !req.Revoked {
			live = append(live, req)
		}
	}
	return live
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fake) problem(format string, args ...any) {
	f.mu.Lock()
	f.Problems = append(f.Problems, fmt.Sprintf(format, args...))
	f.mu.Unlock()
}

// checkJWT verifies an app JWT and records its claims: RS256, signed by
// Key, issued by ClientID when one is set, not issued in the future, and
// expiring within GitHub's ten minutes.
func (f *Fake) checkJWT(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(token, ".")
	if !ok || len(parts) != 3 {
		f.problem("%s %s: no JWT bearer", r.Method, r.URL.Path)
		return false
	}
	enc := base64.RawURLEncoding
	var header struct{ Alg, Typ string }
	hb, _ := enc.DecodeString(parts[0])
	if json.Unmarshal(hb, &header) != nil || header.Alg != "RS256" || header.Typ != "JWT" {
		f.problem("JWT header = %s, want alg RS256 and typ JWT", hb)
		return false
	}
	sig, _ := enc.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.Key, crypto.SHA256, digest[:], sig); err != nil {
		f.problem("JWT signature: %v", err)
		return false
	}
	var claims map[string]any
	cb, _ := enc.DecodeString(parts[1])
	if json.Unmarshal(cb, &claims) != nil {
		f.problem("JWT claims = %s", cb)
		return false
	}
	iss, _ := claims["iss"].(string)
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	now := float64(f.now().Unix())
	switch {
	case iss == "" || (f.ClientID != "" && iss != f.ClientID):
		f.problem("JWT iss = %q, want the client ID %q", iss, f.ClientID)
		return false
	case iat == 0 || iat > now:
		f.problem("JWT iat = %v, which is in the future of %v", iat, now)
		return false
	case exp <= now || exp > now+600:
		f.problem("JWT exp = %v, want within ten minutes after %v", exp, now)
		return false
	}
	f.mu.Lock()
	f.Claims = append(f.Claims, claims)
	f.mu.Unlock()
	return true
}

// Handler serves the fake's endpoints.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	deny := func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	selection := func() string {
		if f.Selection != "" {
			return f.Selection
		}
		return "selected"
	}
	ownerType := func() string {
		if f.OwnerType != "" {
			return f.OwnerType
		}
		return "User"
	}
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(r) {
			deny(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "slug": f.Slug, "name": f.Slug, "html_url": "https://github.com/apps/" + f.Slug,
			"owner": map[string]string{"login": f.Owner, "type": ownerType()}, "permissions": f.Granted,
		})
	})
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(r) {
			deny(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		seen := map[int64]bool{}
		list := []map[string]any{}
		for repo, id := range f.Installed {
			if seen[id] {
				continue
			}
			seen[id] = true
			owner, _, _ := strings.Cut(repo, "/")
			list = append(list, map[string]any{"id": id, "account": map[string]string{"login": owner, "type": "User"},
				"repository_selection": selection(), "permissions": f.Granted})
		}
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(r) {
			deny(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		id, ok := f.Installed[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !ok {
			deny(w, http.StatusNotFound, "Not Found")
			return
		}
		account := r.PathValue("owner")
		if f.InstalledAs != "" {
			account = f.InstalledAs
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "account": map[string]string{"login": account, "type": "User"},
			"repository_selection": selection(), "permissions": f.Granted,
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(r) {
			deny(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		req := FakeTokenRequest{Installation: r.PathValue("id")}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			deny(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		for name, level := range req.Permissions {
			// Written out here, not shared with the code under test, so the
			// two can disagree.
			if have := f.Granted[name]; have != level && have != "write" {
				deny(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
				return
			}
		}
		f.mu.Lock()
		req.Token = fmt.Sprintf("ghs_fake%d", len(f.Requests)+1)
		f.Requests = append(f.Requests, req)
		f.mu.Unlock()
		// As GitHub answers: metadata: read on every token, and the
		// repositories a scoped token reaches.
		granted := map[string]string{"metadata": "read"}
		for name, level := range req.Permissions {
			granted[name] = level
		}
		for name, level := range f.AlsoGrants {
			granted[name] = level
		}
		answer := map[string]any{
			"token": req.Token, "expires_at": f.now().Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": granted,
		}
		if len(req.Repositories) > 0 {
			var owner string
			for repo, id := range f.Installed {
				if fmt.Sprint(id) == req.Installation {
					owner, _, _ = strings.Cut(repo, "/")
				}
			}
			repos := []map[string]string{}
			for _, name := range req.Repositories {
				repos = append(repos, map[string]string{"full_name": owner + "/" + name})
			}
			for _, full := range f.AlsoReaches {
				repos = append(repos, map[string]string{"full_name": full})
			}
			answer["repositories"] = repos
			answer["repository_selection"] = "selected"
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(answer)
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		var req *FakeTokenRequest
		for i := range f.Requests {
			if f.Requests[i].Token == token && !f.Requests[i].Revoked {
				req = &f.Requests[i]
			}
		}
		f.mu.Unlock()
		if req == nil {
			deny(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		repos := []map[string]string{}
		for repo, id := range f.Installed {
			if fmt.Sprint(id) == req.Installation {
				repos = append(repos, map[string]string{"full_name": repo})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(repos), "repositories": repos})
	})
	mux.HandleFunc("DELETE /installation/token", func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.NoRevoke {
			deny(w, http.StatusInternalServerError, "Server Error")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.Requests {
			if f.Requests[i].Token == token && !f.Requests[i].Revoked {
				f.Requests[i].Revoked = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		deny(w, http.StatusUnauthorized, "Bad credentials")
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.UserLookups++
		f.mu.Unlock()
		if f.NoBot {
			deny(w, http.StatusForbidden, "API rate limit exceeded")
			return
		}
		if r.PathValue("login") != f.Slug+"[bot]" {
			deny(w, http.StatusNotFound, "Not Found")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.BotID, "login": f.Slug + "[bot]"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fault, ok := f.fault(r.Method + " " + r.URL.Path)
		if !ok {
			mux.ServeHTTP(w, r)
			return
		}
		if fault.Handled {
			mux.ServeHTTP(httptest.NewRecorder(), r)
		}
		for k, v := range fault.Header {
			w.Header().Set(k, v)
		}
		deny(w, fault.Status, http.StatusText(fault.Status))
	})
}
