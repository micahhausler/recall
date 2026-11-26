package cli

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/micahhausler/recall/internal/githubapp/config"
	"github.com/micahhausler/recall/internal/githubapp/keys/sshagent/agenttest"
)

// import-key on a new machine: it checks the key with GitHub, stores it,
// destroys the download, and writes a complete config.
func TestImportKeyOnANewMachine(t *testing.T) {
	f := newFixture(t)
	download := filepath.Join(f.env.home, "Downloads", "app.private-key.pem")
	if err := os.MkdirAll(filepath.Dir(download), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(download, keyPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("", "import-key", "--key", "file:~/keys/app.pem", download); code != 1 || !strings.Contains(f.stderr.String(), "--client-id") {
		t.Fatalf("without a client ID: exit %d, stderr %q", code, f.stderr)
	}
	if code := f.run("", "import-key", "--key", "file:~/keys/app.pem", "--client-id", "Iv23liEXAMPLE", download); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if _, err := os.Stat(download); !os.IsNotExist(err) {
		t.Errorf("the downloaded key is still there: %v", err)
	}
	cfg, err := config.Load(config.Path(f.env.home, testName))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{ClientID: "Iv23liEXAMPLE", AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you", Key: "file:~/keys/app.pem", KeyFingerprint: keyFingerprint()}
	if *cfg != want {
		t.Errorf("config = %+v, want %+v", *cfg, want)
	}
	// The machine now answers credential requests.
	if code := f.token(request); code != 0 {
		t.Fatalf("token after import: exit %d: %s", code, f.stderr)
	}

	// Rotation: a second key, read from stdin, goes to a new path; the
	// config moves to it and the old one is named for deletion.
	if code := f.run(string(keyPEM()), "import-key", "--key", "file:~/keys/app-2.pem", "-"); code != 0 {
		t.Fatalf("rotation: exit %d: %s", code, f.stderr)
	}
	if cfg, _ := config.Load(config.Path(f.env.home, testName)); cfg.Key != "file:~/keys/app-2.pem" {
		t.Errorf("config key = %s after rotation", cfg.Key)
	}
	if !strings.Contains(f.stdout.String(), "file:~/keys/app.pem before") {
		t.Errorf("rotation output doesn't name the old key:\n%s", f.stdout)
	}
	// The same path again would replace the only copy of a key.
	if code := f.run(string(keyPEM()), "import-key", "--key", "file:~/keys/app-2.pem", "-"); code != 1 || !strings.Contains(f.stderr.String(), "already exists") {
		t.Errorf("import over an existing key: exit %d, stderr %q", code, f.stderr)
	}
}

// A key GitHub doesn't accept for the App is refused before anything is
// stored or destroyed.
func TestImportKeyRefusesAKeyThatIsNotTheApps(t *testing.T) {
	f := newFixture(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(f.env.home, "other.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(other)})
	if err := os.WriteFile(download, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code := f.run("", "import-key", "--key", "file:~/keys/app.pem", "--client-id", "Iv23liEXAMPLE", download)
	if code != 1 || !strings.Contains(f.stderr.String(), "GitHub doesn't accept this key") {
		t.Fatalf("exit %d, stderr %q", code, f.stderr)
	}
	if _, err := os.Stat(download); err != nil {
		t.Errorf("the download was destroyed although the key was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.env.home, "keys", "app.pem")); !os.IsNotExist(err) {
		t.Error("a refused key was stored")
	}
	if _, err := os.Stat(config.Path(f.env.home, testName)); !os.IsNotExist(err) {
		t.Error("a config was written for a refused key")
	}
	f.github.Problems = nil // the bad signature is this test's point
}

// import-key with an ssh-agent key: the agent must already hold the key,
// the download is left alone, and the agent signs from then on.
func TestImportKeyThatAnSSHAgentHolds(t *testing.T) {
	f := newFixture(t)
	download := filepath.Join(f.env.home, "app.private-key.pem")
	if err := os.WriteFile(download, keyPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	// An agent without the key: nothing is written and nothing destroyed.
	empty, err := agenttest.NewFake(other)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	code := f.run("", "import-key", "--key", "ssh-agent:"+empty.Socket, "--client-id", "Iv23liEXAMPLE", download)
	if code != 1 || !strings.Contains(f.stderr.String(), "none with the fingerprint") {
		t.Fatalf("an agent without the key: exit %d, stderr %q", code, f.stderr)
	}
	if _, err := os.Stat(config.Path(f.env.home, testName)); !os.IsNotExist(err) {
		t.Error("a config was written for a key the agent doesn't hold")
	}

	agent, err := agenttest.NewFake(other, testKey())
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	keyRef := "ssh-agent:" + agent.Socket
	if code := f.run("", "import-key", "--key", keyRef, "--client-id", "Iv23liEXAMPLE", download); code != 0 {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	// The tool can't tell whether an agent keeps its keys, so the download
	// may be the only lasting copy.
	if _, err := os.Stat(download); err != nil {
		t.Errorf("the download was destroyed: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "WARNING: "+download+" still holds the key in the clear") {
		t.Errorf("no warning about the download:\n%s", f.stdout)
	}
	cfg, err := config.Load(config.Path(f.env.home, testName))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{ClientID: "Iv23liEXAMPLE", AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you", Key: keyRef, KeyFingerprint: keyFingerprint()}
	if *cfg != want {
		t.Errorf("config = %+v, want %+v", *cfg, want)
	}
	if code := f.token(request); code != 0 {
		t.Fatalf("token signed by the agent: exit %d: %s", code, f.stderr)
	}
	if code := f.run("", "status"); code != 0 || !strings.Contains(f.stdout.String(), keyFingerprint()) {
		t.Fatalf("status: exit %d: %s\n%s", code, f.stderr, f.stdout)
	}

	// With the agent gone, token fails and names the socket.
	agent.Close()
	if code := f.token(request); code != 1 || !strings.Contains(f.stderr.String(), agent.Socket) || f.stdout.Len() != 0 {
		t.Errorf("token with no agent: exit %d, stderr %q, stdout %q", code, f.stderr, f.stdout)
	}
}

// Nothing is destroyed until the key is kept and the config written. Each
// of these fails partway, and the download is still there afterward.
func TestImportKeyLeavesTheDownloadWhenItFails(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f *fixture) []string
		want  string
	}{
		"an organization's App": {func(f *fixture) []string {
			f.github.OwnerType = "Organization"
			return []string{"--key", "file:~/keys/app.pem"}
		}, "this tool is for one person's App"},
		"no bot lookup": {func(f *fixture) []string {
			f.github.NoBot = true
			return []string{"--key", "file:~/keys/app.pem"}
		}, "looking up the App's bot user"},
		"a config that can't be written": {func(f *fixture) []string {
			if err := os.Mkdir(filepath.Join(f.env.home, "readonly"), 0o500); err != nil {
				f.t.Fatal(err)
			}
			return []string{"--key", "file:~/keys/app.pem", "--config", filepath.Join(f.env.home, "readonly", "config.toml")}
		}, "the config can't be written"},
	} {
		f := newFixture(t)
		download := filepath.Join(f.env.home, "app.private-key.pem")
		if err := os.WriteFile(download, keyPEM(), 0o600); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"import-key", "--client-id", "Iv23liEXAMPLE"}, c.setup(f)...)
		code := f.run("", append(args, download)...)
		if code != 1 || !strings.Contains(f.stderr.String(), c.want) {
			t.Errorf("%s: exit %d, stderr %q; want exit 1 and %q", name, code, f.stderr, c.want)
		}
		if _, err := os.Stat(download); err != nil {
			t.Errorf("%s: the download is gone: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(f.env.home, "keys", "app.pem")); !os.IsNotExist(err) {
			t.Errorf("%s: a key was stored: %v", name, err)
		}
		if strings.Contains(f.stdout.String(), "destroyed") {
			t.Errorf("%s: output says the download was destroyed:\n%s", name, f.stdout)
		}
	}
}

// refusing is an agent that lists its keys and signs with none, like one
// whose prompt is declined.
type refusing struct{ agent.ExtendedAgent }

func (refusing) SignWithFlags(ssh.PublicKey, []byte, agent.SignatureFlags) (*ssh.Signature, error) {
	return nil, agent.ErrExtensionUnsupported
}

// Rotation moves the config only to a key that has signed. An agent that
// holds the new key but won't sign with it leaves the config where it was.
func TestImportKeyMovesTheConfigOnlyToAKeyThatSigns(t *testing.T) {
	f := newFixture(t)
	f.configured(config.Config{AppID: 7, Slug: "recall-you", BotUserID: 4242, Owner: "you"})
	before, err := os.ReadFile(config.Path(f.env.home, testName))
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring().(agent.ExtendedAgent)
	if err := ring.Add(agent.AddedKey{PrivateKey: testKey()}); err != nil {
		t.Fatal(err)
	}
	held, err := agenttest.ServeFake(refusing{ring})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	code := f.run(string(keyPEM()), "import-key", "--key", "ssh-agent:"+held.Socket, "-")
	if code != 1 || !strings.Contains(f.stderr.String(), "doesn't sign") {
		t.Fatalf("exit %d, stderr %q", code, f.stderr)
	}
	after, err := os.ReadFile(config.Path(f.env.home, testName))
	if err != nil || string(after) != string(before) {
		t.Errorf("the config changed: %v\n%s", err, after)
	}
}
