// Package cli is the command that gives recall a GitHub credential from a
// GitHub App. recall runs `<name> token` as its credential command; the
// other subcommands set the App up.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/internal/githubapp/app"
	"github.com/micahhausler/recall/internal/githubapp/config"
	"github.com/micahhausler/recall/internal/githubapp/ghapi"
	"github.com/micahhausler/recall/internal/githubapp/keys"
	"github.com/micahhausler/recall/internal/version"
)

// Name is the command, the directory its config and key live under, and
// its User-Agent.
const Name = "recall-github-app"

// Defaults for what the App's GitHub page shows.
const (
	defaultDescription = "Opens pull requests that recall proposes."
	defaultHomepage    = "https://github.com/micahhausler/recall"
)

const usageText = `NAME gives recall a GitHub credential from a GitHub App.

Usage:
  NAME token              print a credential for the request in
                                       RECALL_EXEC_INFO; recall runs this
  NAME status             show the App, its key, and where it is
                                       installed, and mint a test token for
                                       each repository; exit 1 on a problem
  NAME create             register the App on your account, in
                                       the browser, and store its key here
  NAME import-key PEM     store a key downloaded from the App's
                                       settings page, for rotation or another
                                       machine; PEM is a file, or - for stdin
  NAME --version          print the release tag

  --config PATH       the config file (default ~/.config/NAME/config.toml)
  --key KIND:PATH     create, import-key: where the key goes. tpm wraps it
                      with this machine's TPM; file writes a PEM file;
                      ssh-agent (import-key only) is the socket of an SSH
                      agent that already holds it, such as 1Password's. PATH
                      is absolute or starts with ~/.
                      (default on Linux: tpm:~/.local/state/NAME/app-key.blob;
                      elsewhere there is none)
  --client-id ID      import-key: the App's client ID, when no config names
                      it yet
  --name APP          create: the App's name, required; GitHub App names are
                      unique across GitHub
  --description TEXT  create: what the App's GitHub page says it does
                      (default "Opens pull requests that recall proposes.")
  --homepage URL      create: the App's homepage
                      (default https://github.com/micahhausler/recall)
  --port N            create: the local port the browser comes back to (default 8765)
`

// tokenPermissions are what a token may do on its one repository: push a branch
// and open a pull request, and read the checks and commit statuses recall's
// evidence scan reports. The App is registered with the same set.
var tokenPermissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"checks":        "read",
	"statuses":      "read",
}

// usage is the help text, with the command's name.
func usage(name string) string { return strings.ReplaceAll(usageText, "NAME", name) }

// tpmKey is where the key goes on Linux when --key doesn't say. No other
// system has a default: tpm: can only fail there.
func tpmKey(name string) string { return "tpm:~/.local/state/" + name + "/app-key.blob" }

// httpTimeout bounds one call to GitHub.
const httpTimeout = 30 * time.Second

// flagsFor names the flags each command takes, beyond --config. A flag a
// command would ignore is a usage error, not a silent no-op.
var flagsFor = map[string][]string{
	"token":      nil,
	"status":     nil,
	"help":       nil,
	"create":     {"key", "name", "description", "homepage", "port"},
	"import-key": {"key", "client-id"},
}

// env is what a command runs with; tests replace GitHub and the streams.
type env struct {
	// name is the command's name; tests use another.
	name       string
	home       string
	configPath string
	// api is GitHub's REST root and web its site.
	api, web string
	client   *http.Client
	stdin    io.Reader
	// execInfo is RECALL_EXEC_INFO: the credential request recall sets
	// when it runs token.
	execInfo string
	stdout   io.Writer
	stderr   io.Writer
	now      func() time.Time
	// listening, when set, is told create's local page address once it is
	// up, so a test can play the browser.
	listening func(addr string)
}

// Main runs the command os.Args names and returns its exit code.
func Main() int {
	ghapi.UserAgent = Name + "/" + version.Number()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", Name, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// The first interrupt cancels whatever takes a context. A signature
	// in a TPM or an agent takes none, so after the first, the next one
	// ends the process the usual way.
	go func() {
		<-ctx.Done()
		stop()
	}()
	e := &env{name: Name, home: home, api: app.DefaultAPI, web: "https://github.com",
		client: &http.Client{Timeout: httpTimeout},
		stdin:  os.Stdin, stdout: os.Stdout, stderr: os.Stderr, now: time.Now,
		execInfo: os.Getenv(credentialsv1.ExecInfoEnv)}
	return e.run(ctx, os.Args[1:])
}

// run dispatches one command and returns its exit code: 0 for success, 1
// for a failure, 2 for a usage error.
func (e *env) run(ctx context.Context, args []string) int {
	n := e.name
	usage := usage(n)
	fs := flag.NewFlagSet(n, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() { fmt.Fprint(e.stderr, usage) }
	configPath := fs.String("config", "", "")
	keyRef := fs.String("key", "", "")
	clientID := fs.String("client-id", "", "")
	name := fs.String("name", "", "")
	description := fs.String("description", defaultDescription, "")
	homepage := fs.String("homepage", defaultHomepage, "")
	port := fs.Int("port", 8765, "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		if fs.NFlag() != 1 || fs.NArg() != 0 {
			fmt.Fprint(e.stderr, usage)
			return 2
		}
		fmt.Fprintln(e.stdout, n, version.Version)
		return 0
	}
	if fs.NArg() == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	// Flags may follow the command too.
	cmd := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	operands := fs.Args()
	e.configPath = *configPath
	if e.configPath == "" {
		e.configPath = config.Path(e.home, n)
	}
	want := 0
	if cmd == "import-key" {
		want = 1
	}
	if len(operands) != want {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	if allowed, known := flagsFor[cmd]; known {
		var stray string
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "config" && !slices.Contains(allowed, f.Name) {
				stray = f.Name
			}
		})
		if stray != "" {
			fmt.Fprintf(e.stderr, "%s: %s doesn't take --%s\n\n%s", n, cmd, stray, usage)
			return 2
		}
	}
	if *keyRef == "" && (cmd == "create" || cmd == "import-key") {
		if runtime.GOOS != "linux" {
			fmt.Fprintf(e.stderr, "%s: %s needs --key on this system: the default, a tpm: key, works on Linux only\n", n, cmd)
			return 2
		}
		*keyRef = tpmKey(n)
	}
	if cmd == "create" && *name == "" {
		fmt.Fprintf(e.stderr, "%s: create needs --name: GitHub App names are unique across GitHub\n\n%s", n, usage)
		return 2
	}
	var err error
	switch cmd {
	case "token":
		err = e.token(ctx)
	case "status":
		err = e.status(ctx)
	case "create":
		err = e.create(ctx, manifestText{*name, *description, *homepage}, *keyRef, *port)
	case "import-key":
		err = e.importKey(ctx, *keyRef, *clientID, operands[0])
	case "help":
		fmt.Fprint(e.stdout, usage)
		return 0
	default:
		fmt.Fprintf(e.stderr, "%s: unknown command %q\n\n%s", n, cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "%s: %v\n", n, err)
		return 1
	}
	return 0
}

// tilde shortens a path under home for display.
func (e *env) tilde(path string) string {
	if rest, ok := strings.CutPrefix(path, e.home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return path
}

// open loads the config and the App it names, with the key ready to sign.
func (e *env) open() (*config.Config, keys.Ref, *app.App, error) {
	cfg, err := config.Load(e.configPath)
	if err != nil {
		return nil, keys.Ref{}, nil, err
	}
	ref, err := keys.Parse(cfg.Key, e.home)
	if err != nil {
		return nil, ref, nil, fmt.Errorf("%s: %w", e.configPath, err)
	}
	signer, err := ref.Open(cfg.KeyFingerprint)
	if err != nil {
		return nil, ref, nil, fmt.Errorf("opening the key %s: %w", ref, err)
	}
	return cfg, ref, &app.App{ClientID: cfg.ClientID, Key: signer, API: e.api, HTTP: e.client, Now: e.now}, nil
}

// describe fills in what the config doesn't hold about the App from GitHub:
// its ID, slug, owner, and bot user. token is an installation token for
// the bot lookup, or empty to ask anonymously.
func describe(ctx context.Context, a *app.App, cfg *config.Config, token string) error {
	if cfg.AppID == 0 || cfg.Slug == "" || cfg.Owner == "" {
		info, err := a.Info(ctx)
		if err != nil {
			return err
		}
		if err := person(info.Owner); err != nil {
			return err
		}
		cfg.AppID, cfg.Slug, cfg.Owner = info.ID, info.Slug, info.Owner.Login
	}
	if cfg.BotUserID == 0 {
		bot, err := a.Bot(ctx, token)
		if err != nil {
			return err
		}
		cfg.BotUserID = bot.ID
	}
	return nil
}
