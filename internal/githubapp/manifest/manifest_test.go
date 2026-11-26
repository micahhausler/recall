package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var perms = map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read"}

// github stands in for both of GitHub's halves of the flow: the site's
// "create from manifest" form, which here confirms at once and redirects
// back with a code, and the API's conversion endpoint, which trades a code
// for the App once.
type github struct {
	mu        sync.Mutex
	manifests []string
	spent     map[string]bool
}

func (g *github) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /settings/apps/new", func(w http.ResponseWriter, r *http.Request) {
		raw := r.FormValue("manifest")
		var m Manifest
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			http.Error(w, "bad manifest", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.manifests = append(g.manifests, raw)
		g.mu.Unlock()
		http.Redirect(w, r, m.RedirectURL+"?code=good&state="+url.QueryEscape(r.URL.Query().Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /app-manifests/{code}/conversions", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.PathValue("code") != "good" || g.spent["good"] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		g.spent["good"] = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7,"slug":"recall-you","client_id":"Iv23liEXAMPLE","html_url":"https://github.com/apps/recall-you","owner":{"login":"you"},"pem":"the-pem"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var (
	actionRE   = regexp.MustCompile(`<form action="([^"]+)"`)
	manifestRE = regexp.MustCompile(`name="manifest" value="([^"]+)"`)
)

// browse does what the owner's browser does: load the local page, submit
// its form to GitHub, and follow the redirect back. It runs beside the
// flow, so it reports failures without stopping the test.
func browse(t *testing.T, addr string) (status int, body string) {
	resp, err := http.Get(addr)
	if err != nil {
		t.Error(err)
		return 0, ""
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	action, manifest := actionRE.FindSubmatch(page), manifestRE.FindSubmatch(page)
	if action == nil || manifest == nil {
		t.Errorf("the page has no form:\n%s", page)
		return 0, ""
	}
	resp, err = http.PostForm(html.UnescapeString(string(action[1])), url.Values{"manifest": {html.UnescapeString(string(manifest[1]))}})
	if err != nil {
		t.Error(err)
		return 0, ""
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func newFlow(srv *httptest.Server, stored *[]byte, out io.Writer) *Flow {
	return &Flow{
		Manifest: func(redirect string) Manifest {
			return New("recall-you", "Proposes rule changes.", "https://github.com/you/example", redirect, perms)
		},
		Web: srv.URL, API: srv.URL, Client: srv.Client(), Out: out, Timeout: 10 * time.Second,
		Ready: func() error { return nil },
		Store: func(c *Conversion) error { *stored = []byte(c.PEM); return nil },
	}
}

func TestFlowRegistersAndStoresTheKey(t *testing.T) {
	g := &github{spent: map[string]bool{}}
	srv := g.server(t)
	var stored []byte
	var out bytes.Buffer
	f := newFlow(srv, &stored, &out)
	var status int
	var page string
	browsed := make(chan struct{})
	f.Listening = func(addr string) {
		go func() {
			defer close(browsed)
			status, page = browse(t, addr)
		}()
	}
	conv, err := f.Run(context.Background())
	<-browsed
	if err != nil {
		t.Fatal(err)
	}
	if conv.Slug != "recall-you" || conv.ClientID != "Iv23liEXAMPLE" || conv.ID != 7 || conv.Owner.Login != "you" {
		t.Errorf("conversion = %+v", conv)
	}
	if string(stored) != "the-pem" {
		t.Errorf("stored key = %q", stored)
	}
	if conv.PEM != "" {
		t.Error("the returned conversion still holds the private key")
	}
	if status != http.StatusOK || !strings.Contains(page, "/apps/recall-you/installations/new") {
		t.Errorf("the browser ended on %d:\n%s", status, page)
	}
	if strings.Contains(page, "the-pem") || strings.Contains(out.String(), "the-pem") {
		t.Error("the private key was shown to the browser or the terminal")
	}
	if !strings.Contains(out.String(), "ssh -L") {
		t.Errorf("instructions = %q, want the tunnel hint", out.String())
	}

	// What GitHub was sent: private, no webhook, no events, and exactly
	// the permissions asked for. Pointers tell an explicit false or empty
	// list from a missing field, which GitHub would default to a public
	// App with an active webhook.
	if len(g.manifests) != 1 {
		t.Fatalf("GitHub got %d manifests", len(g.manifests))
	}
	var got struct {
		Name     string            `json:"name"`
		Redirect string            `json:"redirect_url"`
		Public   *bool             `json:"public"`
		Perms    map[string]string `json:"default_permissions"`
		Events   *[]string         `json:"default_events"`
		Hook     struct {
			Active *bool `json:"active"`
		} `json:"hook_attributes"`
	}
	if err := json.Unmarshal([]byte(g.manifests[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "recall-you" || !strings.HasPrefix(got.Redirect, "http://localhost:") || !strings.HasSuffix(got.Redirect, "/callback") {
		t.Errorf("manifest = %s", g.manifests[0])
	}
	if got.Public == nil || *got.Public {
		t.Errorf("public = %v, want an explicit false", got.Public)
	}
	if got.Hook.Active == nil || *got.Hook.Active {
		t.Errorf("webhook active = %v, want an explicit false", got.Hook.Active)
	}
	if got.Events == nil || len(*got.Events) != 0 {
		t.Errorf("events = %v, want an explicit empty list", got.Events)
	}
	if len(got.Perms) != len(perms) {
		t.Errorf("permissions = %v, want exactly %v", got.Perms, perms)
	}
	for name, level := range perms {
		if got.Perms[name] != level {
			t.Errorf("permission %s = %q, want %q", name, got.Perms[name], level)
		}
	}
}

// A redirect that doesn't carry this run's state is someone else's link:
// it is refused without spending the code, and the flow keeps waiting.
func TestFlowRefusesAForeignRedirect(t *testing.T) {
	g := &github{spent: map[string]bool{}}
	srv := g.server(t)
	var stored []byte
	f := newFlow(srv, &stored, io.Discard)
	f.Timeout = 300 * time.Millisecond
	var status int
	asked := make(chan struct{})
	f.Listening = func(addr string) {
		go func() {
			defer close(asked)
			resp, err := http.Get(addr + "/callback?code=good&state=not-this-run")
			if err == nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
		}()
	}
	_, err := f.Run(context.Background())
	<-asked
	if err == nil || !strings.Contains(err.Error(), "no answer from the browser") {
		t.Errorf("err = %v, want the wait to time out", err)
	}
	if status != http.StatusBadRequest || stored != nil || g.spent["good"] {
		t.Errorf("foreign redirect: status %d, stored %q, code spent %v", status, stored, g.spent["good"])
	}
}

// Nowhere to put the key stops the flow before the owner is sent to
// GitHub, where confirming would make an App whose key is lost.
func TestFlowChecksTheKeyHasAHomeFirst(t *testing.T) {
	g := &github{spent: map[string]bool{}}
	srv := g.server(t)
	var stored []byte
	f := newFlow(srv, &stored, io.Discard)
	f.Ready = func() error { return errors.New("the TPM isn't reachable") }
	f.Listening = func(string) { t.Error("the page was served") }
	if _, err := f.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "TPM isn't reachable") {
		t.Errorf("err = %v", err)
	}
}

// When storing the key fails after GitHub made the App, the error says the
// App exists and how to get a key into it.
func TestFlowSaysWhenTheAppExistsWithoutItsKey(t *testing.T) {
	g := &github{spent: map[string]bool{}}
	srv := g.server(t)
	var stored []byte
	f := newFlow(srv, &stored, io.Discard)
	f.Store = func(*Conversion) error { return errors.New("disk full") }
	browsed := make(chan struct{})
	f.Listening = func(addr string) {
		go func() {
			defer close(browsed)
			browse(t, addr)
		}()
	}
	_, err := f.Run(context.Background())
	<-browsed
	if err == nil || !strings.Contains(err.Error(), "was created") || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "import-key") {
		t.Errorf("err = %v", err)
	}
}

func TestExchangeSpentOrEmptyCode(t *testing.T) {
	g := &github{spent: map[string]bool{"good": true}}
	srv := g.server(t)
	if _, err := Exchange(context.Background(), srv.Client(), srv.URL, "good"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("spent code: err = %v", err)
	}
	if _, err := Exchange(context.Background(), srv.Client(), srv.URL, ""); err == nil {
		t.Error("an empty code was exchanged")
	}
}

// A page on another site that points its own name at this machine gets
// nothing: not the form, and so not the state.
func TestFlowAnswersOnlyAsLocalhost(t *testing.T) {
	g := &github{spent: map[string]bool{}}
	srv := g.server(t)
	var stored []byte
	f := newFlow(srv, &stored, io.Discard)
	f.Timeout = 300 * time.Millisecond
	var status int
	var body string
	asked := make(chan struct{})
	f.Listening = func(addr string) {
		go func() {
			defer close(asked)
			req, _ := http.NewRequest(http.MethodGet, addr+"/", nil)
			req.Host = "evil.example:8765"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			status, body = resp.StatusCode, string(data)
		}()
	}
	_, _ = f.Run(context.Background())
	<-asked
	if status != http.StatusForbidden || strings.Contains(body, "state=") {
		t.Errorf("another host: status %d, body %q", status, body)
	}
}

// A redirect whose code GitHub refuses doesn't end the run: the real one
// can still arrive.
func TestFlowKeepsWaitingAfterARefusedCode(t *testing.T) {
	g := &github{spent: map[string]bool{}}
	srv := g.server(t)
	var stored []byte
	f := newFlow(srv, &stored, io.Discard)
	var refused int
	browsed := make(chan struct{})
	f.Listening = func(addr string) {
		go func() {
			defer close(browsed)
			resp, err := http.Get(addr + "/")
			if err != nil {
				t.Error(err)
				return
			}
			page, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			m := regexp.MustCompile(`state=([0-9a-f]+)`).FindSubmatch(page)
			if m == nil {
				t.Errorf("no state in the page: %s", page)
				return
			}
			if resp, err = http.Get(addr + "/callback?code=not-githubs&state=" + string(m[1])); err == nil {
				refused = resp.StatusCode
				resp.Body.Close()
			}
			browse(t, addr)
		}()
	}
	conv, err := f.Run(context.Background())
	<-browsed
	if err != nil || conv == nil || stored == nil {
		t.Fatalf("after a refused code: conv %v, err %v", conv, err)
	}
	if refused != http.StatusBadGateway {
		t.Errorf("the refused code's status = %d", refused)
	}
}

// The code is good for an hour, so an exchange that never reached GitHub
// doesn't repeat it.
func TestExchangeErrorLeavesTheCodeOut(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := Exchange(context.Background(), nil, srv.URL, "one-time-code")
	if err == nil || strings.Contains(err.Error(), "one-time-code") {
		t.Errorf("err = %v, want an error without the code", err)
	}
}

// The answer carries the App's only copy of its key, so an exchange is
// never asked twice, even after a failure or a rate limit.
func TestExchangeIsAskedOnce(t *testing.T) {
	for _, h := range []map[string]string{nil, {"Retry-After": "1"}} {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			for k, v := range h {
				w.Header().Set(k, v)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		if _, err := Exchange(context.Background(), srv.Client(), srv.URL, "good"); err == nil {
			t.Error("no error")
		}
		srv.Close()
		if calls != 1 {
			t.Errorf("headers %v: exchange asked %d times, want 1", h, calls)
		}
	}
}
