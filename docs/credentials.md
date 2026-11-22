# Credentials: acting as someone other than the machine's login

By default recall reaches a hosted repository as whatever `gh` and `git` are signed in as on the machine.
That makes you the author of the pull requests recall opens, and a pull request's author can't approve it.
`review.credentials.command` names a command that supplies a credential instead, so a bot can open the pull request and you can review and approve it as yourself.

recall knows nothing about where the credential comes from.
It runs the command and uses what comes back, the way a Kubernetes client runs an exec credential plugin.
A GitHub App that mints one-hour installation tokens, a bot user's token read from a password manager, and anything else that prints the same JSON all work.

```toml
[review.credentials]
command = ["recall-credential", "token"]
timeout = "30s"
```

## The contract

recall runs the command once per repository per run, without a shell.
It sets `RECALL_EXEC_INFO` in the command's environment to one JSON object, the way a Kubernetes client sets `KUBERNETES_EXEC_INFO`, and reads one from the command's stdout.
The command's stdin is empty: recall runs unattended, so a command can't prompt there.
Both are the same object, shaped like a Kubernetes `TokenRequest`: recall fills in `spec` with what it asks for, and the command answers with `status`, what it grants.

What recall sets `RECALL_EXEC_INFO` to:

```json
{
  "apiVersion": "recall.credentials/v1",
  "kind": "Credential",
  "spec": {"repo": "github.com/you/example"}
}
```

What the command prints:

```json
{
  "apiVersion": "recall.credentials/v1",
  "kind": "Credential",
  "spec": {"repo": "github.com/you/example"},
  "status": {
    "token": "ghs_...",
    "expirationTimestamp": "2026-03-02T13:00:00Z",
    "commitIdentity": {"name": "recall-you[bot]", "email": "4242+recall-you[bot]@users.noreply.github.com"},
    "owner": "you"
  }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `apiVersion`, `kind` | yes | `recall.credentials/v1` and `Credential`, in both directions; anything else is refused. |
| `spec.repo` | yes, from recall | The repository to act on, as `host/owner/name`: the origin's slug, the same whatever protocol the checkout was cloned over. |
| `status.token` | yes | Authenticates `gh` and `git push` on the requested repository. |
| `status.expirationTimestamp` | no | When the token stops working, in RFC 3339. |
| `status.commitIdentity` | no | The git author and committer of recall's commits, so they match the pull request's author. Both `name` and `email` are needed. |
| `status.owner` | no | Your login. recall asks you to review each pull request it opens, and counts your review comments as yours; a bot's token names the bot, so recall can't learn who you are from GitHub. |

- The command may echo `spec` back or leave it out; recall reads only `status`.
- A command should ignore fields it doesn't know, and recall does the same, so `spec` and `status` can each grow.
- A non-zero exit fails that repository's publish or evidence scan, and recall shows the command's stderr.
  It never shows the command's stdout, which may hold a token.
- The command gets `timeout` to answer; the default is 30 seconds.

## The contract as Go types

A command written in Go can import the object instead of copying its shape:

```go
import credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
```

`credentialsv1.Credential` is the object above, with `Spec`, `Status`, and `CommitIdentity` beside it; `APIVersion` and `Kind` are the two strings to check and send, and `ExecInfoEnv` is the variable's name.
The package imports only the standard library, and recall's own side of the contract is built on it, so the two can't spell a field differently.
A `Status` prints without its token under `%v`, `%+v`, `%s`, and `%#v`; only `encoding/json` writes it.

## A command in Go writes only where its token comes from

The `command` package beside the types is the command's side of the contract, so a command doesn't write it again:

```go
package main

import (
	"context"
	"fmt"
	"os"

	credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
	"github.com/micahhausler/recall/api/credentials/v1/command"
)

func main() {
	err := command.Run(context.Background(), os.Getenv(credentialsv1.ExecInfoEnv), os.Stdout, grant)
	if err != nil {
		fmt.Fprintln(os.Stderr, "recall-credential:", err)
		os.Exit(1)
	}
}

func grant(ctx context.Context, req command.Request) (credentialsv1.Status, error) {
	if req.Host != "example.com" {
		return credentialsv1.Status{}, fmt.Errorf("%s isn't on example.com", req.Spec.Repo)
	}
	token, err := mint(ctx, req.Owner, req.Name) // yours to write
	if err != nil {
		return credentialsv1.Status{}, err
	}
	return credentialsv1.Status{Token: token, Owner: "you"}, nil
}
```

- `command.Run` reads and checks the request, calls `grant`, and writes the answer.
  A request with another `apiVersion` or `kind`, or a `spec.repo` that isn't `host/owner/name`, is an error that says which.
- `command.Request` holds the `spec` and the three parts of its `repo`: `Host`, `Owner`, and `Name`.
- `Run` refuses to write to a terminal, which would put a live token on a screen, before it calls `grant`; the error is `command.ErrTerminal`.
- `Run` writes nothing to stdout unless every step succeeded, so on an error the command prints it to stderr and exits non-zero.
- `command.ParseRequest` and `Request.WriteAnswer` are the two halves of `Run`, for a command that needs them apart.
- The request's text and the writer are arguments, so a command's tests pass a string and a buffer.
- `command` imports `golang.org/x/term` to tell a terminal from any other file; the types alone still import only the standard library.

## What recall does with it

- **Caching.** recall keeps each credential in memory until one minute before `expirationTimestamp`, or for the run when there is none.
  The publisher, the review check, and the evidence scan share it.
- **The token stays in the environment.** `gh` gets it as `GH_TOKEN`.
  `git push` gets it through a credential helper set for that one process, which reads the token from the environment.
  The token is never written to disk, an argument list, a remote URL, or `.git/config`.
- **The push goes over HTTPS.** A token can't authenticate an SSH remote, so the push names `https://<host>/<owner>/<repo>.git` outright, whatever the checkout's origin is.
  The machine's own credential helpers are turned off for that push, so none is offered your login or told to store the token, and the URL is pinned so a rewrite rule in your git config can't send it elsewhere.
  Fetches still use origin as the checkout has it.
- **Commits.** With a `commitIdentity`, each proposal's commit is authored and committed as it, and isn't signed: your signing key would vouch for an author it isn't.
- **Review request.** With an `owner`, recall asks them to review each pull request it opens, so the change lands in your review queue with a notification.
  A request GitHub refuses is a warning; the pull request stays open.
- **Evidence.** The `github` evidence source runs `gh` with the token too.
  A review comment counts as yours only when its author is `owner`; without `owner`, none does.
- **No fallback.** With a command configured, a repository it can't cover fails, named.
  recall never falls back to the machine's own login, which would open the pull request as the wrong author without saying so.

The token needs to push a branch and open and read pull requests.
For the `github` evidence source it also needs to read check runs and commit statuses.
