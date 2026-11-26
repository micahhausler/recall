package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSaveThenLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recall-example", "config.toml")
	want := &Config{ClientID: "Iv23liEXAMPLE", AppID: 123456, Slug: "recall-you", BotUserID: 234567,
		ExtraPermissions: map[string]string{"issues": "write"},
		Key:              "tpm:~/.local/state/recall-example/app-key.blob"}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, %v; want 0600", st.Mode(), err)
	}
	got, err := Load(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("loaded %+v, %v; want %+v", got, err, want)
	}
	// Saving again replaces the file and leaves no temp file behind.
	want.BotUserID = 1
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("config dir holds %d files, want only the config", len(entries))
	}
}

func TestLoadSaysWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct{ body, want string }{
		"missing file": {"", "doesn't exist"},
		"no client":    {"key = \"file:/x\"\n", "client_id isn't set"},
		"no key":       {"client_id = \"x\"\n", "key isn't set"},
		"not toml":     {"client_id = \n", "config.toml"},
	} {
		path := filepath.Join(dir, name, "config.toml")
		if tc.body != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
