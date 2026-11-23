package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/api/credentials/v1/command"
)

// testKey is a throwaway App key, made once per test binary and never
// written anywhere but a test's temp dir.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// keyFile writes the test key to a PEM file, as GitHub hands it out.
func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	clientID  = "Iv1.example"
	fakeToken = "token-for-you-example"
)

// fakeGitHub is the four calls the command makes, with the App installed
// on you/example. It checks every JWT against the test key.
type fakeGitHub struct {
	t *testing.T
	// scope is the repositories a minted token says it reaches; nil is
	// the one asked for.
	scope []string
	// calls counts every request, answered or not.
	calls atomic.Int32
	// asked is the body of the token request.
	asked struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/you/example/installation":
		f.checkJWT(bearer)
		writeJSON(w, map[string]any{"id": 99})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"message": "Not Found"})
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/99/access_tokens":
		f.checkJWT(bearer)
		if err := json.NewDecoder(r.Body).Decode(&f.asked); err != nil {
			f.t.Errorf("fake GitHub: token request body: %v", err)
		}
		scope := f.scope
		if scope == nil {
			scope = []string{"you/" + strings.Join(f.asked.Repositories, ",")}
		}
		var repos []map[string]string
		for _, s := range scope {
			repos = append(repos, map[string]string{"full_name": s})
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"token": fakeToken, "expires_at": "2026-03-02T13:00:00Z", "repositories": repos})
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		f.checkJWT(bearer)
		writeJSON(w, map[string]any{"slug": "recall-you"})
	case r.Method == http.MethodGet && r.URL.Path == "/users/recall-you[bot]":
		if bearer != fakeToken {
			f.t.Errorf("fake GitHub: user lookup authenticated with %q, want the installation token", bearer)
		}
		writeJSON(w, map[string]any{"id": 4242, "login": "recall-you[bot]"})
	default:
		f.t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// checkJWT checks a JWT is RS256, issued by the client ID, and signed by
// the test key.
func (f *fakeGitHub) checkJWT(jwt string) {
	f.t.Helper()
	claims, err := verifyJWT(jwt, &testKey().PublicKey)
	if err != nil {
		f.t.Errorf("fake GitHub: %v", err)
		return
	}
	if claims.Iss != clientID {
		f.t.Errorf("fake GitHub: JWT iss = %q, want %q", claims.Iss, clientID)
	}
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// verifyJWT checks a JWT's header and signature and returns its claims.
func verifyJWT(jwt string, pub *rsa.PublicKey) (jwtClaims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return jwtClaims{}, fmt.Errorf("JWT has %d parts, want 3", len(parts))
	}
	enc := base64.RawURLEncoding
	var header struct{ Alg, Typ string }
	if err := decodePart(parts[0], &header); err != nil || header.Alg != "RS256" || header.Typ != "JWT" {
		return jwtClaims{}, fmt.Errorf("JWT header = %+v (%v), want RS256 JWT", header, err)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return jwtClaims{}, fmt.Errorf("JWT signature: %v", err)
	}
	var claims jwtClaims
	if err := decodePart(parts[1], &claims); err != nil {
		return jwtClaims{}, err
	}
	return claims, nil
}

func decodePart(part string, v any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func request(repo string) string {
	return `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"` + repo + `"}}`
}

// The whole command, with a string for RECALL_EXEC_INFO and a buffer for
// stdout: what recall reads back is a token for the one repository asked
// about, its expiry, the bot's commit identity, and your login.
func TestAnswersARequest(t *testing.T) {
	gh := &fakeGitHub{t: t}
	srv := gh.start(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--client-id", clientID, "--key", keyFile(t), "--owner", "you", "--api", srv.URL}
	if code := run(context.Background(), args, request("github.com/you/example"), &stdout, &stderr, srv.Client()); code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}

	var got credentialsv1.Credential
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout isn't a credential: %v\n%s", err, stdout.String())
	}
	if got.APIVersion != credentialsv1.APIVersion || got.Kind != credentialsv1.Kind || got.Spec.Repo != "github.com/you/example" {
		t.Errorf("answer = %s %s %s, want the request's apiVersion, kind, and repo", got.APIVersion, got.Kind, got.Spec.Repo)
	}
	if got.Status == nil {
		t.Fatal("answer has no status")
	}
	expires := time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC)
	want := credentialsv1.Status{
		Token:               fakeToken,
		ExpirationTimestamp: &expires,
		CommitIdentity:      &credentialsv1.CommitIdentity{Name: "recall-you[bot]", Email: "4242+recall-you[bot]@users.noreply.github.com"},
		Owner:               "you",
	}
	if got.Status.Token != want.Token || !got.Status.ExpirationTimestamp.Equal(expires) ||
		!reflect.DeepEqual(got.Status.CommitIdentity, want.CommitIdentity) || got.Status.Owner != want.Owner {
		t.Errorf("status = %v, want %v", got.Status, want)
	}
	if !reflect.DeepEqual(gh.asked.Repositories, []string{"example"}) || !reflect.DeepEqual(gh.asked.Permissions, permissions) {
		t.Errorf("token request = %+v, want example alone with %v", gh.asked, permissions)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// A terminal is refused before the key is read or GitHub is called, so no
// token is minted only to land on a screen.
func TestRefusesATerminal(t *testing.T) {
	tty := openTerminal(t)
	gh := &fakeGitHub{t: t}
	srv := gh.start(t)
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "no-such-key.pem")
	args := []string{"--client-id", clientID, "--key", missing, "--api", srv.URL}
	if code := run(context.Background(), args, request("github.com/you/example"), tty, &stderr, srv.Client()); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), command.ErrTerminal.Error()) {
		t.Errorf("stderr = %q, want ErrTerminal", stderr.String())
	}
	if n := gh.calls.Load(); n != 0 {
		t.Errorf("GitHub got %d calls, want none", n)
	}
}

// Each failure exits 1, says why on stderr, and leaves stdout empty, so
// recall reads no answer and the token never reaches stderr.
func TestFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		repo  string
		scope []string
		key   func(*testing.T) string
		want  string
	}{
		"another host":                      {repo: "gitlab.example.com/you/example", want: "github.com only"},
		"not installed":                     {repo: "github.com/you/other", want: "isn't installed on you/other"},
		"scoped elsewhere":                  {repo: "github.com/you/example", scope: []string{"you/example", "you/other"}, want: "the token is not used"},
		"a repo that isn't host/owner/name": {repo: "github.com/you", want: "isn't host/owner/name"},
		"a key that isn't one": {repo: "github.com/you/example", want: "holds no PEM block", key: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "app.pem")
			if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(name, func(t *testing.T) {
			gh := &fakeGitHub{t: t, scope: tc.scope}
			srv := gh.start(t)
			key := keyFile
			if tc.key != nil {
				key = tc.key
			}
			var stdout, stderr bytes.Buffer
			args := []string{"--client-id", clientID, "--key", key(t), "--api", srv.URL}
			if code := run(context.Background(), args, request(tc.repo), &stdout, &stderr, srv.Client()); code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr.String(), tc.want)
			}
			if strings.Contains(stderr.String(), fakeToken) {
				t.Errorf("stderr holds the token: %q", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

func TestUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"no client ID": {"--key", "app.pem"},
		"no key":       {"--client-id", clientID},
		"an operand":   {"--client-id", clientID, "--key", "app.pem", "token"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, request("github.com/you/example"), &stdout, &stderr, nil); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: stdout = %q, want nothing", name, stdout.String())
		}
	}
}

// The JWT is RS256 over the header and claims, issued by the client ID a
// minute before now and expiring nine minutes after.
func TestJWT(t *testing.T) {
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	a := &app{clientID: clientID, key: testKey(), now: func() time.Time { return now }}
	jwt, err := a.jwt()
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifyJWT(jwt, &testKey().PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	want := jwtClaims{Iss: clientID, Iat: now.Add(-time.Minute).Unix(), Exp: now.Add(9 * time.Minute).Unix()}
	if claims != want {
		t.Errorf("claims = %+v, want %+v", claims, want)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyJWT(jwt, &other.PublicKey); err == nil {
		t.Error("the JWT verifies under another key")
	}
}

// readKey takes the PKCS #8 form too, and refuses a key that isn't RSA's.
func TestReadKey(t *testing.T) {
	dir := t.TempDir()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pkcs8.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := readKey(path)
	if err != nil || !key.Equal(testKey()) {
		t.Errorf("readKey(PKCS #8) = %v, want the test key", err)
	}
	cert := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(cert); err == nil || !strings.Contains(err.Error(), "not a private key") {
		t.Errorf("readKey(certificate) = %v, want not a private key", err)
	}
}
