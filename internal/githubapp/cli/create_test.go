package cli

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/githubapp/config"
)

func TestCreateRefusesAMachineThatAlreadyHasAnApp(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{})
	if code := f.run("", "create", "--key", "file:~/new.pem", "--name", "recall-you"); code != 1 || !strings.Contains(f.stderr.String(), "already has an App") {
		t.Errorf("exit %d, stderr %q", code, f.stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{{}, {"nope"}, {"token", "extra"}, {"import-key"}, {"status", "--bogus"}} {
		if code := f.run("", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := f.run("", "help"); code != 0 || !strings.Contains(f.stdout.String(), "recall-example token") {
		t.Errorf("help: exit %d", code)
	}
}

// create, end to end against a fake GitHub: the browser confirms, the key
// from GitHub's one-time answer is stored, and the config is complete.
// creating adds GitHub's side of the manifest flow to the fixture, and a
// browser that confirms at once. It returns the manifest GitHub was sent,
// and a channel closed when the browser is done.
func creating(t *testing.T, f *fixture) (manifestSeen *string, browsed chan struct{}) {
	t.Helper()
	manifestSeen = new(string)
	// GitHub's site: confirm at once and send the browser back with a code.
	// GitHub's API: the fake App, plus the one-time conversion.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /settings/apps/new", func(w http.ResponseWriter, r *http.Request) {
		*manifestSeen = r.FormValue("manifest")
		var m struct {
			Redirect string `json:"redirect_url"`
		}
		_ = json.Unmarshal([]byte(*manifestSeen), &m)
		http.Redirect(w, r, m.Redirect+"?code=good&state="+url.QueryEscape(r.URL.Query().Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /app-manifests/good/conversions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "slug": "recall-you", "name": "recall-you", "client_id": "Iv23liEXAMPLE",
			"html_url": "https://github.com/apps/recall-you", "owner": map[string]string{"login": "you"}, "pem": string(keyPEM())})
	})
	mux.Handle("/", f.github.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.env.api, f.env.web, f.env.client = srv.URL, srv.URL, srv.Client()

	browsed = make(chan struct{})
	f.env.listening = func(addr string) {
		go func() {
			defer close(browsed)
			resp, err := http.Get(addr)
			if err != nil {
				t.Error(err)
				return
			}
			page, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			action := regexp.MustCompile(`<form action="([^"]+)"`).FindSubmatch(page)
			manifest := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindSubmatch(page)
			if action == nil || manifest == nil {
				t.Errorf("no form on the page:\n%s", page)
				return
			}
			resp, err = http.PostForm(html.UnescapeString(string(action[1])), url.Values{"manifest": {html.UnescapeString(string(manifest[1]))}})
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	return manifestSeen, browsed
}

func TestCreateRegistersStoresAndConfigures(t *testing.T) {
	f := newFixture(t)
	seen, browsed := creating(t, f)
	code := f.run("", "create", "--key", "file:~/keys/app.pem", "--port", "0", "--name", "recall-you")
	<-browsed
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, f.stderr, f.stdout)
	}
	cfg, err := config.Load(config.Path(f.env.home, testName))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{ClientID: "Iv23liEXAMPLE", AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you", Key: "file:~/keys/app.pem", KeyFingerprint: keyFingerprint()}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("config = %+v, want %+v", *cfg, want)
	}
	if strings.Contains(f.stdout.String(), "PRIVATE KEY") {
		t.Error("create printed the private key")
	}
	if !strings.Contains(f.stdout.String(), "client ID Iv23liEXAMPLE") || strings.Contains(f.stdout.String(), "WARNING") {
		t.Errorf("create doesn't name the client ID, or warns:\n%s", f.stdout)
	}
	if !strings.Contains(f.stdout.String(), "/apps/recall-you/installations/new") {
		t.Errorf("create doesn't say where to install:\n%s", f.stdout)
	}
	var m struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		URL         string            `json:"url"`
		Perms       map[string]string `json:"default_permissions"`
	}
	if err := json.Unmarshal([]byte(*seen), &m); err != nil || m.Name != "recall-you" || m.Description != defaultDescription || m.URL != defaultHomepage || mustJSON(m.Perms) != mustJSON(tokenPermissions) {
		t.Errorf("manifest = %s", *seen)
	}
	// The machine now answers credential requests with the stored key.
	if code := f.token(request); code != 0 {
		t.Fatalf("token after create: exit %d: %s", code, f.stderr)
	}
}

// GitHub hands a new App's key over once, and nothing here can put it
// into an agent, so create refuses before it asks.
func TestCreateRefusesAnSSHAgentKey(t *testing.T) {
	f := newFixture(t)
	code := f.run("", "create", "--key", "ssh-agent:~/agent.sock", "--name", "recall-you")
	if code != 1 || !strings.Contains(f.stderr.String(), "import-key") {
		t.Errorf("exit %d, stderr %q", code, f.stderr)
	}
}

// The key and the config are kept the moment GitHub hands the App over. A
// lookup that fails afterward costs a warning, not the App.
func TestCreateKeepsTheAppWhenALaterLookupFails(t *testing.T) {
	f := newFixture(t)
	_, browsed := creating(t, f)
	f.github.NoBot = true
	code := f.run("", "create", "--key", "file:~/keys/app.pem", "--port", "0", "--name", "recall-you")
	<-browsed
	if code != 0 || !strings.Contains(f.stdout.String(), "WARNING: the bot user's ID isn't in the config yet") {
		t.Fatalf("exit %d: %s\n%s", code, f.stderr, f.stdout)
	}
	cfg, err := config.Load(config.Path(f.env.home, testName))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{ClientID: "Iv23liEXAMPLE", AppID: 7, Slug: "recall-you", Owner: "you", Key: "file:~/keys/app.pem", KeyFingerprint: keyFingerprint()}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("config = %+v, want %+v", *cfg, want)
	}
	// token asks GitHub for the bot itself once it answers again.
	f.github.NoBot = false
	if code := f.token(request); code != 0 || !strings.Contains(f.stdout.String(), "4242+recall-you[bot]") {
		t.Fatalf("token after create: exit %d: %s", code, f.stderr)
	}
}

// A key path that can't be written is found before the owner is sent to
// GitHub, where confirming would make an App whose key has nowhere to go.
func TestCreateChecksTheKeyCanBeWrittenFirst(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.env.home, "blocked"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.env.listening = func(string) { t.Error("the page was served") }
	code := f.run("", "create", "--key", "file:~/blocked/app.pem", "--port", "0", "--name", "recall-you")
	if code != 1 {
		t.Errorf("exit %d, stderr %q", code, f.stderr)
	}
}

// A flag a command would ignore is refused, as is a key path that would
// mean a different file from each directory.
func TestStrayFlagsAndRelativeKeyPaths(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{})
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"status", "--key", "file:/elsewhere.pem"}, 2, "status doesn't take --key"},
		{[]string{"token", "--client-id", "Iv23liOTHER"}, 2, "token doesn't take --client-id"},
		{[]string{"import-key", "--name", "x", "-"}, 2, "import-key doesn't take --name"},
		{[]string{"import-key", "--key", "file:keys/app.pem", "-"}, 1, "must be absolute"},
		{[]string{"create", "--name", "recall-you", "--key", "tpm:app.blob"}, 1, "must be absolute"},
		{[]string{"create", "--key", "file:~/keys/app.pem"}, 2, "create needs --name"},
		{[]string{"status", "--homepage", "https://example.com"}, 2, "status doesn't take --homepage"},
		{[]string{"import-key", "--description", "x", "-"}, 2, "import-key doesn't take --description"},
	} {
		if code := f.run("", c.args...); code != c.code || !strings.Contains(f.stderr.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d and %q", c.args, code, f.stderr, c.code, c.want)
		}
	}
}
