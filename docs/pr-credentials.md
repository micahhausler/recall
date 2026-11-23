# Pull request credentials: acting as someone other than the machine's login

By default recall reaches a hosted repository as whatever `gh` and `git` are signed in as on the machine.
That makes you the author of the pull requests recall opens, and a pull request's author can't approve it.
`review.credentials.command` names a command that supplies a credential instead, so a bot can open the pull request and you can review and approve it as yourself.
This is a credential for the git host, used to push branches and open pull requests.
The AWS credentials an S3 store needs are separate, and `docs/store.md` covers them.

recall knows nothing about where the credential comes from.
It runs the command and uses what comes back.
The contract is modeled on Kubernetes exec credential plugins.
A GitHub App that mints one-hour installation tokens, a bot user's token read from a password manager, and anything else that prints the same JSON all work.

```toml
[review.credentials]
command = ["recall-credential", "token"]
timeout = "30s"
```

## The contract

recall runs the command once per repository per run, without a shell.
It sets `RECALL_EXEC_INFO` in the command's environment to one JSON object, and reads one from the command's stdout.
The command's stdin is empty as recall runs unattended and a command can't prompt there.
Both are the same object: recall fills in `spec` with what it asks for, and the command answers with `status`, what it grants.

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
- A non-zero exit fails that repository's publish or evidence scan, and recall shows the command's stderr. It never shows the command's stdout, which may hold a token.
- The command gets `timeout` to answer; the default is 30 seconds.

## A shell script can be the command

The command doesn't have to be written in Go. This one prints a bot user's token from a password manager:

```bash
#!/bin/sh
set -eu
token=$(pass show recall-bot/github-token)
printf '{"apiVersion":"recall.credentials/v1","kind":"Credential","status":{"token":"%s","owner":"you"}}\n' "$token"
```

It ignores `RECALL_EXEC_INFO`, so it hands the same token to every repository recall asks about. Read `spec.repo` from that variable to answer per repository.

## Test a command by hand

Set `RECALL_EXEC_INFO` yourself and pipe the output through `jq`, dropping the token so it never reaches your screen:

```bash
RECALL_EXEC_INFO='{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/example"}}' \
  recall-credential token | jq '.status | del(.token)'
```

A working command prints the rest of `status`. A command built on the `command` helper package refuses to print to a terminal, so it only answers through a pipe like this one.

## The contract as Go types

A command written in Go can import the object instead of copying its shape:

```go
import credentialsv1 "github.com/micahhausler/recall/api/credentials/v1"
```

`credentialsv1.Credential` is the object above, with `Spec`, `Status`, and `CommitIdentity` beside it; `APIVersion` and `Kind` are the two strings to check and send, and `ExecInfoEnv` is the variable's name.
The package imports only the standard library, and recall's own side of the contract is built on it, so the two can't spell a field differently.
A `Status` prints without its token under `%v`, `%+v`, `%s`, and `%#v`; only `encoding/json` writes it.

## What recall does with the credential

- **Caching.** recall keeps each credential in memory until one minute before `expirationTimestamp`, or for the run when there is none. The publisher, the review check, and the evidence scan share it.
- **The token stays in the environment.** `gh` gets it as `GH_TOKEN`. `git push` gets it through a credential helper set for that one process, which reads the token from the environment. The token is never written to disk, an argument list, a remote URL, or `.git/config`.
- **The push goes over HTTPS.** A token can't authenticate an SSH remote, so the push names `https://<host>/<owner>/<repo>.git` outright, whatever the checkout's origin is. The machine's own credential helpers are turned off for that push, so none is offered your login or told to store the token, and the URL is pinned so a rewrite rule in your git config can't send it elsewhere. Fetches still use origin as the checkout has it.
- **Commits.** With a `commitIdentity`, each proposal's commit is authored and committed as it, and isn't signed: your signing key would vouch for an author it isn't.
- **Review request.** With an `owner`, recall asks them to review each pull request it opens, so the change lands in your review queue with a notification. A request GitHub refuses is a warning; the pull request stays open.
- **Evidence.** The `github` evidence source runs `gh` with the token too. A review comment counts as yours only when its author is `owner`; without `owner`, none does.
- **No fallback.** With a command configured, a repository it can't cover fails, named. recall never falls back to the machine's own login, which would open the pull request as the wrong author without saying so.

## Permissions the token needs

The token needs to push a branch and open and read pull requests.
For the `github` evidence source it also needs to read check runs and commit statuses.
For a GitHub App, that is Contents and Pull requests at read and write, plus Checks and Commit statuses at read-only for the evidence source.
