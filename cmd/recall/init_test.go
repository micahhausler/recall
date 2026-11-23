package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/store"
)

func TestGUIPaths(t *testing.T) {
	linux := strings.Join(guiPaths("linux"), ":")
	if linux != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("linux = %s", linux)
	}
	darwin := strings.Join(guiPaths("darwin"), ":")
	for _, want := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(darwin+":", want+":") {
			t.Errorf("darwin %s misses %s", darwin, want)
		}
	}
}

func TestOffGUIPath(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, "on")
	elsewhere := filepath.Join(dir, "elsewhere")
	for _, d := range []string{onPath, elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{onPath}

	// A binary in a GUI path warns about nothing.
	if msg := offGUIPath(filepath.Join(onPath, "recall"), paths); msg != "" {
		t.Errorf("a binary on the GUI PATH warned: %s", msg)
	}

	// One anywhere else warns, naming its directory and the PATH.
	msg := offGUIPath(filepath.Join(elsewhere, "recall"), paths)
	for _, want := range []string{"warning", elsewhere, onPath} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q misses %q", msg, want)
		}
	}

	// A binary reachable through a link on a GUI path is fine: that's how
	// Homebrew installs land.
	real := filepath.Join(elsewhere, "recall")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(onPath, "recall")); err != nil {
		t.Fatal(err)
	}
	if msg := offGUIPath(real, paths); msg != "" {
		t.Errorf("a linked binary warned: %s", msg)
	}
}

// With two machines on one store, only one runs the reduce: the first init
// claims learn/reducer, and the second reads the claim and leaves
// hooks.learn_every off.
func TestOneReducerPerStore(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()

	// Machine a: nothing has claimed the store, so init turns the reduce on
	// and claims it.
	host, err := storeReducer(ctx, st)
	if err != nil || host != "" {
		t.Fatalf("fresh store reducer = %q, %v", host, err)
	}
	if err := claimReducer(ctx, st, "laptop-a"); err != nil {
		t.Fatal(err)
	}

	// Machine b: the claim holds, and a race for it changes nothing.
	host, err = storeReducer(ctx, st)
	if err != nil || host != "laptop-a" {
		t.Fatalf("claimed store reducer = %q, %v", host, err)
	}
	if err := claimReducer(ctx, st, "laptop-b"); err != nil {
		t.Fatalf("a second claim should be a no-op: %v", err)
	}
	if host, _ = storeReducer(ctx, st); host != "laptop-a" {
		t.Errorf("reducer = %q, want the first claim kept", host)
	}

	// Re-running init on the reducer is also a no-op.
	if err := claimReducer(ctx, st, "laptop-a"); err != nil {
		t.Fatal(err)
	}
}

// fakeKiroCLI puts a kiro-cli on PATH that runs script with $REC set to a
// directory it can record into, and points init at it.
func fakeKiroCLI(t *testing.T, script string) (rec string) {
	t.Helper()
	bin, rec := t.TempDir(), t.TempDir()
	body := "#!/bin/sh\nREC=" + rec + "\n" + script
	if err := os.WriteFile(filepath.Join(bin, "kiro-cli"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("RECALL_HOOK", "")
	old := kiroPowerCLI
	kiroPowerCLI = "kiro-cli"
	t.Cleanup(func() { kiroPowerCLI = old })
	return rec
}

func kiroSettings(on bool) *config.Settings {
	s := &config.Settings{Harnesses: map[string]config.HarnessSettings{"claude": {Enabled: true}}}
	if on {
		s.Harnesses["kiro"] = config.HarnessSettings{Enabled: true}
	}
	return s
}

const recordingKiroCLI = `echo "$@" > "$REC/args"
echo "$RECALL_HOOK" > "$REC/guard"
cp -R "$3" "$REC/power"
echo 'Installed power "recall"'
`

// With the kiro harness on, init installs the power from a temp dir
// holding exactly the manifest and the two skills, as they are in the repo,
// and removes the temp dir afterward.
func TestInitKiroPowerInstalls(t *testing.T) {
	rec := fakeKiroCLI(t, recordingKiroCLI)
	var out bytes.Buffer
	if !initKiroPower(context.Background(), &out, kiroSettings(true), false) {
		t.Fatalf("not installed: %s", out.String())
	}
	if want := `kiro power: Installed power "recall" (keep and recall-setup)`; strings.TrimSpace(out.String()) != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args"))
	if !strings.HasPrefix(string(args), "powers install ") {
		t.Errorf("args = %q", args)
	}
	if guard, _ := os.ReadFile(filepath.Join(rec, "guard")); strings.TrimSpace(string(guard)) != "1" {
		t.Errorf("RECALL_HOOK = %q, want the hook guard set", guard)
	}

	var got []string
	power := filepath.Join(rec, "power")
	err := filepath.WalkDir(power, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(power, path)
		got = append(got, filepath.ToSlash(rel))
		installed, _ := os.ReadFile(path)
		repo, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(installed, repo) {
			t.Errorf("%s differs from the repo's", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "plugin.json skills/keep/SKILL.md skills/recall-setup/SKILL.md"
	if strings.Join(got, " ") != want {
		t.Errorf("installed %v, want %s", got, want)
	}

	left, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil || len(left) != 0 {
		t.Errorf("temp dir left behind: %v, %v", left, err)
	}
}

func TestInitKiroPowerDryRun(t *testing.T) {
	rec := fakeKiroCLI(t, recordingKiroCLI)
	var out bytes.Buffer
	if initKiroPower(context.Background(), &out, kiroSettings(true), true) {
		t.Error("a dry run reported an install")
	}
	if !strings.HasPrefix(out.String(), "would install the keep and recall-setup skills into Kiro") {
		t.Errorf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args")); !os.IsNotExist(err) {
		t.Errorf("a dry run ran kiro-cli: %v", err)
	}
}

func TestInitKiroPowerHarnessOff(t *testing.T) {
	rec := fakeKiroCLI(t, recordingKiroCLI)
	var out bytes.Buffer
	if initKiroPower(context.Background(), &out, kiroSettings(false), false) || out.Len() != 0 {
		t.Errorf("harness off: installed or printed %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args")); !os.IsNotExist(err) {
		t.Errorf("harness off ran kiro-cli: %v", err)
	}
}

func TestInitKiroPowerMissingCLI(t *testing.T) {
	fakeKiroCLI(t, recordingKiroCLI)
	kiroPowerCLI = "recall-test-no-such-kiro-cli"
	var out bytes.Buffer
	if initKiroPower(context.Background(), &out, kiroSettings(true), false) {
		t.Error("a missing CLI reported an install")
	}
	if !strings.HasPrefix(out.String(), "warning: recall-test-no-such-kiro-cli isn't on PATH") {
		t.Errorf("output = %q", out.String())
	}
}

func TestInitKiroPowerFailingCLI(t *testing.T) {
	fakeKiroCLI(t, "echo 'not logged in' >&2\nexit 3\n")
	var out bytes.Buffer
	if initKiroPower(context.Background(), &out, kiroSettings(true), false) {
		t.Error("a failing CLI reported an install")
	}
	for _, want := range []string{"warning: couldn't install the keep and recall-setup skills into Kiro", "exit status 3", "not logged in"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q misses %q", out.String(), want)
		}
	}
	left, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil || len(left) != 0 {
		t.Errorf("temp dir left behind: %v, %v", left, err)
	}
}
