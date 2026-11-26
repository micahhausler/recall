package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/githubapp/appkey"
)

// TestRealAgent runs import-key, token, and status against an SSH agent
// someone really runs, such as 1Password's, with a fake GitHub that knows
// the key. It needs a throwaway key that is already in the agent:
//
//	RECALL_AGENT_TEST=<socket> RECALL_AGENT_PEM=<the key's PEM file> go test -run TestRealAgent -v .
//
// The agent may ask for approval at each of the three signatures.
func TestRealAgent(t *testing.T) {
	socket, pemFile := os.Getenv("RECALL_AGENT_TEST"), os.Getenv("RECALL_AGENT_PEM")
	if socket == "" || pemFile == "" {
		t.Skip("set RECALL_AGENT_TEST to an agent's socket and RECALL_AGENT_PEM to a throwaway key it holds")
	}
	data, err := os.ReadFile(pemFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := appkey.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := appkey.Fingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.github.Key = &key.PublicKey
	download := filepath.Join(f.env.home, "key.pem")
	if err := os.WriteFile(download, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("", "import-key", "--key", "ssh-agent:"+socket, "--client-id", "Iv23liEXAMPLE", download); code != 0 {
		t.Fatalf("import-key: exit %d: %s", code, f.stderr)
	}
	t.Logf("import-key:\n%s", f.stdout)
	if code := f.token(request); code != 0 {
		t.Fatalf("token: exit %d: %s", code, f.stderr)
	}
	if code := f.run("", "status"); code != 0 || !strings.Contains(f.stdout.String(), fp) {
		t.Fatalf("status: exit %d: %s\n%s", code, f.stderr, f.stdout)
	}
	t.Logf("status:\n%s", f.stdout)
}
