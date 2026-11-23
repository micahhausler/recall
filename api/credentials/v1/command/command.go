// Package command is the command's side of the recall.credentials/v1
// contract (docs/pr-credentials.md): read the request recall set in the
// environment, check it, and write the answer to stdout. A credential
// command written in Go calls Run with a function that grants a token, and
// that function is the only part the command writes itself.
//
// Nothing here knows where a token comes from, and nothing names a host or
// a provider. The package reads no environment and opens no file: the
// request's text and the writer are arguments, so a command's tests pass a
// string and a buffer.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
)

// ErrTerminal is what Run and WriteAnswer return instead of writing to a
// terminal. The answer is a live token, and recall reads it from a pipe; a
// terminal would only put it on a screen and in its scrollback.
var ErrTerminal = errors.New("stdout is a terminal, and the answer holds a live token; pipe it to what will use it")

// Request is one credential request, read and checked.
type Request struct {
	// Spec is the request's spec as this package read it. The answer
	// carries it back: the fields this version knows, and none it doesn't.
	Spec credentialsv1.Spec
	// Host, Owner, and Name are the three parts of Spec.Repo, each
	// non-empty: example.com, you, and example for example.com/you/example.
	Host, Owner, Name string
}

// ParseRequest reads and checks a request. execInfo is the value of the
// environment variable credentialsv1.ExecInfoEnv. An empty value, text
// that isn't a JSON object, another apiVersion or kind, and a spec.repo
// that isn't host/owner/name are each an error that says which. Fields
// this version doesn't know are ignored, so the contract can grow.
func ParseRequest(execInfo string) (Request, error) {
	if execInfo == "" {
		return Request{}, fmt.Errorf("%s isn't set; recall sets it to the credential request when it runs this command", credentialsv1.ExecInfoEnv)
	}
	var c credentialsv1.Credential
	if err := json.Unmarshal([]byte(execInfo), &c); err != nil {
		return Request{}, fmt.Errorf("reading the credential request from %s: %w", credentialsv1.ExecInfoEnv, err)
	}
	if c.APIVersion != credentialsv1.APIVersion || c.Kind != credentialsv1.Kind {
		return Request{}, fmt.Errorf("the request is %q %q; this command answers %q %q", c.APIVersion, c.Kind, credentialsv1.APIVersion, credentialsv1.Kind)
	}
	parts := strings.Split(c.Spec.Repo, "/")
	if len(parts) != 3 || slices.Contains(parts, "") {
		return Request{}, fmt.Errorf("spec.repo %q isn't host/owner/name", c.Spec.Repo)
	}
	return Request{Spec: c.Spec, Host: parts[0], Owner: parts[1], Name: parts[2]}, nil
}

// WriteAnswer writes the answer to r on w: r's spec with status, what the
// command grants, as one indented JSON object and a newline. An expiry is
// written to the second, in UTC.
//
// It refuses, and writes nothing, when w is a terminal (ErrTerminal), when
// status has no token, and when status has a commit identity missing its
// name or email: recall would refuse both of those answers. No error holds
// the token.
func (r Request) WriteAnswer(w io.Writer, status credentialsv1.Status) error {
	if terminal(w) {
		return ErrTerminal
	}
	if status.Token == "" {
		return errors.New("the answer has no status.token")
	}
	if id := status.CommitIdentity; id != nil && (id.Name == "" || id.Email == "") {
		return errors.New("the answer's commitIdentity is missing its name or email")
	}
	if status.ExpirationTimestamp != nil {
		expires := status.ExpirationTimestamp.UTC().Truncate(time.Second)
		status.ExpirationTimestamp = &expires
	}
	out, err := json.MarshalIndent(credentialsv1.Credential{
		APIVersion: credentialsv1.APIVersion, Kind: credentialsv1.Kind,
		Spec: r.Spec, Status: &status,
	}, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

// Run answers one request: it parses execInfo, calls grant with ctx and
// the request, and writes what grant returns to stdout. It is a whole
// credential command but for where the token comes from, which is grant.
//
// execInfo is the value of the environment variable
// credentialsv1.ExecInfoEnv, and stdout is where recall reads the answer:
// os.Stdout itself, since a writer wrapped around it hides what it is.
// Run returns ErrTerminal before it parses anything or calls grant when
// stdout is a terminal, so no token is made only to be refused. It
// returns grant's error as it is. Nothing is written to stdout unless
// every step succeeded, so on an error the command only has to print it
// to stderr and exit non-zero.
func Run(ctx context.Context, execInfo string, stdout io.Writer, grant func(context.Context, Request) (credentialsv1.Status, error)) error {
	if terminal(stdout) {
		return ErrTerminal
	}
	req, err := ParseRequest(execInfo)
	if err != nil {
		return err
	}
	status, err := grant(ctx, req)
	if err != nil {
		return err
	}
	return req.WriteAnswer(stdout, status)
}

// terminal reports whether w is a terminal. Only an *os.File can be one:
// a pipe, a regular file, the null device, and a writer that wraps a file
// are not.
func terminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	// Control, not Fd: Fd would put the caller's file into blocking mode.
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var is bool
	err = conn.Control(func(fd uintptr) { is = term.IsTerminal(int(fd)) })
	return err == nil && is
}
