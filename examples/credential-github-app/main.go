// Command credential-github-app is an example recall credential command
// (docs/pr-credentials.md), built on api/credentials/v1/command. It answers
// each request with a GitHub App installation token that reaches only the
// requested repository, so pull requests recall opens are the App's bot's
// and you can approve them.
//
// command.Run does the contract: it reads the request, refuses to write to
// a terminal, and writes the answer. What is left here is grant, where the
// token comes from: a JWT signed with the App's private key, exchanged for
// an installation token.
package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/api/credentials/v1/command"
)

const name = "credential-github-app"

const usage = `credential-github-app answers a recall credential request with a GitHub App
installation token for the one repository recall asks about.

Usage:
  credential-github-app --client-id ID --key PATH [--owner LOGIN]

  --client-id ID   the App's client ID
  --key PATH       the App's private key, the PEM file GitHub gave you
  --owner LOGIN    your GitHub login; recall asks you to review each pull request
  --api URL        GitHub's REST root (default https://api.github.com)

recall sets the request in RECALL_EXEC_INFO and reads the answer from stdout,
which must not be a terminal.
`

// permissions are what a token may do on its one repository: push a branch
// and open a pull request, and read the checks and commit statuses recall's
// github evidence source reports. The App needs at least these.
var permissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"checks":        "read",
	"statuses":      "read",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := &http.Client{Timeout: 30 * time.Second}
	os.Exit(run(ctx, os.Args[1:], os.Getenv(credentialsv1.ExecInfoEnv), os.Stdout, os.Stderr, client))
}

// run is the whole command, with everything main reads from the process
// passed in: tests hand it a request string, a buffer, and a client for a
// fake GitHub. It returns the exit code: 0 for an answer, 1 for a failure,
// and 2 for a usage error.
func run(ctx context.Context, args []string, execInfo string, stdout, stderr io.Writer, client *http.Client) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	clientID := fs.String("client-id", "", "")
	keyPath := fs.String("key", "", "")
	owner := fs.String("owner", "", "")
	api := fs.String("api", defaultAPI, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *clientID == "" || *keyPath == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}

	grant := func(ctx context.Context, req command.Request) (credentialsv1.Status, error) {
		none := credentialsv1.Status{}
		if req.Host != "github.com" {
			return none, fmt.Errorf("spec.repo %s is on %s; a GitHub App works on github.com only", req.Spec.Repo, req.Host)
		}
		// The key is read here, not before Run: a terminal is refused
		// before anything touches the key.
		key, err := readKey(*keyPath)
		if err != nil {
			return none, err
		}
		a := &app{clientID: *clientID, key: key, api: *api, http: client, now: time.Now}
		token, expires, err := a.installationToken(ctx, req.Owner, req.Name)
		if err != nil {
			return none, err
		}
		identity, err := a.botIdentity(ctx, token)
		if err != nil {
			return none, err
		}
		return credentialsv1.Status{
			Token:               token,
			ExpirationTimestamp: &expires,
			CommitIdentity:      identity,
			Owner:               *owner,
		}, nil
	}

	if err := command.Run(ctx, execInfo, stdout, grant); err != nil {
		if errors.Is(err, command.ErrTerminal) {
			err = fmt.Errorf("%w, such as jq '.status | del(.token)'", err)
		}
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// readKey reads an RSA private key from a PEM file: PKCS #1, which is what
// GitHub hands out, or PKCS #8. No error holds the file's contents.
func readKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the App's private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s holds a %T; a GitHub App's key is RSA", path, parsed)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("%s holds a %q PEM block, not a private key", path, block.Type)
	}
}
