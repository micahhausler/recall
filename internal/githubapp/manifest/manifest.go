// Package manifest registers a GitHub App through GitHub's manifest flow:
// a local page posts the App's settings to GitHub, the owner confirms them
// in the browser, and GitHub sends the browser back with a code that is
// traded, once and within an hour, for the new App's identifiers and its
// first private key.
package manifest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/micahhausler/recall/internal/githubapp/ghapi"
)

// Manifest describes the App to register.
type Manifest struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	// RedirectURL is where GitHub sends the browser, with the code, once
	// the owner confirms.
	RedirectURL string `json:"redirect_url"`
	// Public false keeps anyone but the owner from installing the App.
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
	Hook               Hook              `json:"hook_attributes"`
}

// Hook is the App's webhook. Nothing listens for one, so Active is false;
// GitHub still wants a URL.
type Hook struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// New is a manifest for an App private to its owner, with no webhook, no
// events, and the given repository permissions.
func New(name, description, homepage, redirect string, permissions map[string]string) Manifest {
	return Manifest{
		Name: name, URL: homepage, Description: description, RedirectURL: redirect,
		Public: false, DefaultPermissions: permissions, DefaultEvents: []string{},
		Hook: Hook{URL: homepage, Active: false},
	}
}

// Conversion is a newly registered App, as GitHub hands it over exactly
// once.
type Conversion struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	HTMLURL  string `json:"html_url"`
	Owner    struct {
		Login string `json:"login"`
		// Type is "User" or "Organization".
		Type string `json:"type"`
	} `json:"owner"`
	// PEM is the App's private key. Nothing else ever returns it.
	PEM string `json:"pem"`
}

// Exchange trades the code GitHub's redirect carried for the new App. api
// is GitHub's REST root; client may be nil. The code is still good for an
// hour, so no error here repeats it.
func Exchange(ctx context.Context, client *http.Client, api, code string) (*Conversion, error) {
	if code == "" {
		return nil, errors.New("no code to exchange")
	}
	var c Conversion
	path := "/app-manifests/" + url.PathEscape(code) + "/conversions"
	if err := ghapi.CallOnce(ctx, client, api, http.MethodPost, path, "", nil, &c); err != nil {
		var apiErr *ghapi.Error
		var netErr *ghapi.TransportError
		switch {
		case errors.As(err, &apiErr):
			return nil, fmt.Errorf("exchanging the code: GitHub answered %d %s (a code is good once, for an hour)", apiErr.Status, apiErr.Message)
		case errors.As(err, &netErr):
			return nil, fmt.Errorf("exchanging the code: %w", netErr.Err)
		}
		return nil, errors.New("exchanging the code: the request couldn't be made")
	}
	if c.ClientID == "" || c.PEM == "" {
		return nil, errors.New("exchanging the code: GitHub's answer has no client ID or private key")
	}
	return &c, nil
}

// Flow is one run of the manifest flow.
type Flow struct {
	// Manifest builds the manifest once the redirect URL is known.
	Manifest func(redirect string) Manifest
	// Port is where the local page listens and GitHub's redirect returns;
	// 0 picks a free one.
	Port int
	// Web is GitHub's site and API its REST root.
	Web, API string
	// Client makes the exchange; nil is http.DefaultClient.
	Client *http.Client
	// Ready is asked, before the owner is sent to GitHub, whether the key
	// can be stored. The key is handed over exactly once, right after they
	// confirm, so there must be somewhere to put it first.
	Ready func() error
	// Store keeps the new App: its private key, in conv.PEM, and whatever
	// record of the App the caller needs to find it again. It runs once,
	// right after GitHub hands the key over, so it should do nothing that
	// can fail for a reason unrelated to keeping them.
	Store func(conv *Conversion) error
	// Out gets the instructions.
	Out io.Writer
	// Listening, when set, is told the page's address once it is up.
	Listening func(addr string)
	// Timeout is how long to wait for the browser; zero is 15 minutes.
	Timeout time.Duration
}

var createPage = template.Must(template.New("create").Parse(`<!doctype html>
<meta charset="utf-8">
<title>Create the GitHub App {{.Name}}</title>
<body style="font: 16px system-ui; max-width: 40em; margin: 4em auto">
<h1>Create the GitHub App {{.Name}}</h1>
<p>This sends the App's settings to GitHub, where you confirm them.
The App is private to your account, receives no webhooks, and gets these
repository permissions:</p>
<ul>{{range .Permissions}}<li>{{.}}</li>{{end}}</ul>
<form action="{{.Action}}" method="post">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<button type="submit" style="font: inherit; padding: .5em 1em">Continue to GitHub</button>
</form>
</body>
`))

var donePage = template.Must(template.New("done").Parse(`<!doctype html>
<meta charset="utf-8">
<title>{{.Slug}} is created</title>
<body style="font: 16px system-ui; max-width: 40em; margin: 4em auto">
<h1>{{.Slug}} is created</h1>
<p>Its private key is stored on the machine that ran the command.</p>
<p>Next, <a href="{{.Install}}">install it</a> on only the repositories it
opens pull requests on. The terminal has the rest.</p>
</body>
`))

// Run serves the registration page, waits for GitHub to send the browser
// back with a code, trades the code for the new App, and stores its key.
// The returned conversion no longer holds the key.
func (f *Flow) Run(ctx context.Context) (*Conversion, error) {
	if err := f.Ready(); err != nil {
		return nil, err
	}
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, err
	}
	state := hex.EncodeToString(stateBytes)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.Port))
	if err != nil {
		return nil, fmt.Errorf("%w; choose another with --port", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	// GitHub redirects the browser here. localhost, not 127.0.0.1: through
	// an SSH tunnel it is the browser's own machine either way.
	origin := fmt.Sprintf("http://localhost:%d", port)
	m := f.Manifest(origin + "/callback")
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}

	type result struct {
		conv *Conversion
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		var perms []string
		for name, level := range m.DefaultPermissions {
			perms = append(perms, name+": "+level)
		}
		sort.Strings(perms)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = createPage.Execute(w, map[string]any{"Name": m.Name, "Permissions": perms,
			"Action": f.Web + "/settings/apps/new?state=" + state, "Manifest": string(manifest)})
	})
	// One redirect is answered at a time, and none after the App is kept.
	var mu sync.Mutex
	kept := false
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		// The state proves this redirect answers the page served above,
		// not a link someone else made.
		if r.URL.Query().Get("state") != state {
			http.Error(w, "This isn't the page the command is waiting for (wrong state). Start again from "+origin, http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if kept {
			http.Error(w, "The App is already created; the terminal has the rest.", http.StatusGone)
			return
		}
		conv, err := Exchange(r.Context(), f.Client, f.API, r.URL.Query().Get("code"))
		if err != nil {
			// Nothing was handed over, so nothing is lost: a code GitHub
			// refused may not be the owner's, and one that didn't reach
			// GitHub is still good. Keep waiting either way.
			http.Error(w, err.Error()+"\nThe command is still waiting. Reload this page to try the same code again, or start again from "+origin, http.StatusBadGateway)
			return
		}
		kept = true
		err = f.Store(conv)
		conv.PEM = ""
		if err != nil {
			err = fmt.Errorf("the App %s was created, but keeping it failed: %w\n"+
				"Generate a new private key on its settings page (%s) and store it with import-key --client-id %s", conv.Slug, err, conv.HTMLURL, conv.ClientID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			done <- result{err: err}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = donePage.Execute(w, map[string]string{"Slug": conv.Slug, "Install": f.Web + "/apps/" + conv.Slug + "/installations/new"})
		done <- result{conv: conv}
	})
	// Only a browser that asked for localhost is answered. A page on
	// another site that points its own name at 127.0.0.1 could otherwise
	// read the state from the page above and send its own redirect.
	local := map[string]bool{fmt.Sprintf("localhost:%d", port): true, fmt.Sprintf("127.0.0.1:%d", port): true}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !local[r.Host] {
			http.Error(w, "This page answers only as "+origin, http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()

	fmt.Fprintf(f.Out, "Open %s in a browser signed in to GitHub, and confirm the App there.\n", origin)
	fmt.Fprintf(f.Out, "From another machine, tunnel the port first: ssh -L %d:localhost:%d <this host>\n", port, port)
	if f.Listening != nil {
		f.Listening(origin)
	}
	timeout := f.Timeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	select {
	case res := <-done:
		return res.conv, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("no answer from the browser after %s; nothing was created unless you confirmed on GitHub", timeout)
	}
}
