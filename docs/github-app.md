# recall-github-app: pull requests from a GitHub App's bot

`recall-github-app` is a recall credential command that mints a GitHub App installation token for each repository recall asks about.
recall's proposals then arrive as pull requests opened by the App's bot, so you review and approve them as yourself.
It is an optional companion to `recall`: a separate binary and Homebrew cask, built from the same release.

recall stays credential-agnostic.
It runs the configured command and uses the token that comes back, as [`docs/pr-credentials.md`](pr-credentials.md) describes.
Everything about the App, its private key, and the bot's identity lives in `recall-github-app`.

Each run mints a token that expires within an hour and reaches one repository.
The App's private key signs the request for it, and that key can stay wrapped by the machine's TPM or held by an SSH agent.

## Install

```sh
brew tap micahhausler/recall https://github.com/micahhausler/recall
brew install --cask recall-github-app
```

With Go:

```sh
go install github.com/micahhausler/recall/cmd/recall-github-app@latest
```

Release archives named `recall-github-app_<version>_<os>_<arch>.tar.gz` sit beside recall's own on the releases page.

## Commands

| Command | What it does |
| --- | --- |
| `recall-github-app token` | Prints a credential for the request in `RECALL_EXEC_INFO`. recall runs this. |
| `recall-github-app status` | Shows the App, its key and fingerprint, and where it is installed, and mints a test token for each repository, then revokes it. Exits 1 on any problem. |
| `recall-github-app create` | Registers a new App through GitHub's manifest flow, stores its key, and writes the config. |
| `recall-github-app import-key PEM` | Stores a key downloaded from the App's settings page, for an existing App, rotation, or another machine. `PEM` is a file, or `-` for stdin. |
| `recall-github-app --version` | Prints the release tag. |

Flags: `--config PATH`, `--permission NAME=LEVEL` (`token`, repeatable), `--key KIND:PATH` (`create`, `import-key`; `create` takes `tpm` or `file`), `--client-id ID` (`import-key`), and `--name APP`, `--description TEXT`, `--homepage URL`, and `--port N` (`create`).
`create` requires `--name`, because GitHub App names are unique across GitHub.
A flag a command doesn't take is a usage error.
`--key` defaults to a `tpm:` key on Linux and has no default elsewhere, and its path is absolute or starts with `~/`.

## The contract

recall runs `token` once per repository per run, sets `RECALL_EXEC_INFO` to the request, and reads the answer from stdout.
The object's Go type is recall's own `api/credentials/v1`, and its `command` package reads the request and writes the answer, so the two sides can't spell a field differently.

```toml
# recall config, on a machine that publishes
[review.credentials]
command = ["recall-github-app", "token"]
timeout = "30s"
```

What `token` prints:

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

- `token` always sends all of `status`, and echoes `spec` back with the fields it honored.
- `status.owner` is the login of the account that owns the App, which recall asks to review each pull request.
- A failure exits 1 with the reason on stderr and nothing on stdout.
- A request for a repository the App isn't installed on fails and says where to install it.
  Nothing falls back to another identity.
- Only `github.com` repositories are answered.

## The token

| Scope | Value |
| --- | --- |
| Repositories | The one in `spec.repo` |
| Contents | write: push the proposal branch |
| Pull requests | write: open the pull request and read its state |
| Checks, Commit statuses | read: recall's evidence scan reports failed checks |
| Lifetime | One hour, set by GitHub |

A caller other than recall, such as a wrapper that files issues, adds a permission with `--permission`, e.g. `--permission issues=write`.
`token` asks for one only when the config's `extra_permissions` holds it at that level or higher.
`token` checks what GitHub says it granted against what it asked for, and drops a token that reaches another repository, more than one, or any other permission.
`token` writes a token only to stdout, never caches one, and refuses to print one to a terminal.
recall holds it in memory for the run.

## Setup for a new App

`create` runs on the machine that will publish.
From another machine, an SSH tunnel to the port first lets GitHub's redirect to `localhost` land on the publishing machine:

```sh
ssh -L 8765:localhost:8765 laptop-a
recall-github-app create --name recall-you
```

1. Open `http://localhost:8765` in a browser signed in to GitHub, and click through to **Create GitHub App**.
2. GitHub sends the browser back with a one-time code; `create` trades it for the App's key, stores the key, signs with it once, and writes the config.
3. Optionally, upload [`img/github-app-logo.png`](img/github-app-logo.png) as the App's logo under **Display information** on the App's settings page.
   GitHub's manifest flow takes no logo, and neither `gh` nor the REST API sets one, so this step is manual.
4. Install the App on only the repositories recall opens pull requests on, from the link `create` prints.
5. Run `recall-github-app status`.
6. Add `[review.credentials]` to that machine's recall config.

`create` checks that the key and the config can both be written before it sends the browser to GitHub, because GitHub hands the key over exactly once.
The moment the key arrives it is stored and the config written; the bot user's ID is looked up afterward, and a failed lookup is a warning.
The local page answers only a browser that asked for `localhost`, and a code GitHub refuses leaves `create` waiting for the real one.
With the default `tpm:` key, the key goes from GitHub's answer into the TPM from memory and never touches disk in the clear.

The App `create` registers is installable by your account only, has no webhook, and holds Contents and Pull requests at write and Checks and Commit statuses at read, plus the Metadata read every App has.

## Setup for an App that already exists

For an App registered by hand, or a second machine, `import-key` takes a key downloaded from the App's settings page:

```sh
recall-github-app import-key --client-id Iv23liEXAMPLE ~/Downloads/recall-you.private-key.pem
```

`import-key` asks GitHub whether the key is the App's and what the config records about the App, all with the downloaded key.
Then it stores the key, signs with the stored key once, and writes the config.
Only after that does it overwrite and remove the download, so a failure at any step leaves the download where it was.
Piping the key over SSH keeps the plaintext off the publishing machine's disk entirely:

```sh
ssh laptop-a recall-github-app import-key --client-id Iv23liEXAMPLE - < ~/Downloads/recall-you.private-key.pem
```

`import-key` can't reach a download on another machine, so that one is yours to delete.

## Setup on a Mac, with the key in 1Password

A Mac has no TPM this command can use, and its Secure Enclave holds no RSA keys.
1Password's SSH agent holds an RSA key and signs with it, so the key never reaches this command.

1. Generate a new private key on the App's settings page.
2. Add the download to 1Password as an SSH Key item, and turn on 1Password's SSH agent.
3. Import it, naming the agent's socket:

   ```sh
   recall-github-app import-key --key "ssh-agent:~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock" --client-id Iv23liEXAMPLE ~/Downloads/recall-you.private-key.pem
   ```

4. Delete the download once the key is saved in 1Password.
5. Run `recall-github-app status`.
6. Add `[review.credentials]` to that machine's recall config, with a `timeout` long enough to approve the agent's prompt.

`import-key` asks GitHub whether the key is the App's, has the agent sign once with it, and writes the config with the key's fingerprint.
It adds nothing to the agent and leaves the download alone: it can't tell whether an agent keeps its keys after a restart, so the download may be the only lasting copy.
`create` refuses an `ssh-agent:` key, because GitHub hands a new App's key over once and nothing here can put it into an agent.

## Config

```toml
# ~/.config/recall-github-app/config.toml (per machine)
client_id = "Iv23liEXAMPLE"
app_id = 123456
slug = "recall-you"
bot_user_id = 234567
owner = "you"
key = "tpm:~/.local/state/recall-github-app/app-key.blob"
```

Only `client_id` and `key` are required, and `key_fingerprint` too when the key is in an SSH agent:

```toml
key = "ssh-agent:~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
key_fingerprint = "SHA256:..."   # as the App's settings page lists it
```

`key_fingerprint` picks the key out of the agent, which holds others.
For any kind, a key that doesn't match it is an error; `create` and `import-key` write it.
A key the file doesn't define, such as a misspelled one, is an error too.
`extra_permissions` names what the App holds on purpose beyond the token table, for `token --permission` to ask for:

```toml
extra_permissions = { issues = "write" }
```

`status` expects the App to hold each of them, and reports neither as more than a token uses.
The rest is what GitHub says about the App, kept so `token` doesn't ask every time; `token` asks when one is missing, and `status` fails when one disagrees with GitHub.
Nothing in the file is secret.

## Keys

| Kind | Where the key lives | Use |
| --- | --- | --- |
| `tpm:<path>` | Wrapped by this machine's TPM; the file is a blob only that TPM can load. Linux only. | A machine that publishes unattended. |
| `ssh-agent:<socket>` | In the SSH agent listening at the socket, such as 1Password's. The agent signs; the key never reaches this command. | A machine with no TPM and a person there to approve, like a laptop. |
| `file:<path>` | A PEM file, mode 0600; one that others can read is refused. | Testing, or a machine with neither. The key is as safe as the file. |

- **One key per machine, for `tpm` and `file`.** An App holds up to 25 keys, each revoked on its own, so a lost machine costs one key and needs no backup.
- **The TPM key's parent** is the TCG reference RSA storage root key in the owner hierarchy, made on demand from its template.
  Nothing is provisioned and no persistent handle is used.
  A blob `tpm2_import` wrote under that same parent loads too: the file is its public part followed by its private part.
- **TPM access** takes membership in the group that owns `/dev/tpmrm0`, usually `tss`.
- **Speed.** A TPM takes a few seconds to answer `token`, nearly all of it remaking the storage root key.
  The TPM signs once per `token` run: one JWT serves both API calls.
  recall keeps each answer in memory until a minute before it expires, so every `gh` and `git` call after the first reuses the token.
- **An agent key is named, never guessed.**
  It is found by `key_fingerprint`, even when it is the only RSA key in the agent.
  The agent is asked for an `rsa-sha2-256` signature, which is RS256, and the answer is verified against the public key before it is used.
- **An agent may ask a person.**
  Each `token` run asks the agent for one signature, and the agent decides whether that needs approval.
  With no one there, or the agent locked, `token` fails, and recall publishes nothing as anyone else.
- **A live key belongs in a store that signs.**
  A key in a dotfiles repo, an environment variable, or a password manager that hands the key over (`op read`) leaves the key in the clear on every read.

## What each key kind doesn't protect against

- **The TPM stops the key being copied, not used.**
  The wrapped key has no PIN and no policy, so any process running as the user, with access to the TPM device, can ask it for a signature.
- **A key in 1Password is per account, not per machine.**
  The item syncs to every device signed in to the account, and whoever can open the item can export the key.
  Any process that reaches the agent's socket can ask for a signature, and the agent's prompt doesn't show what is being signed.
- **A file key is as safe as the file.**
  Anything that can read the file holds the App.
- **One signature is the whole App for ten minutes.**
  A signed JWT can mint a token for any repository the App is installed on, with every permission the App holds.
  One repository and the table's permissions is what `token` asks for, not a limit on whoever can sign.
  So the App is installed on selected repositories only and holds only those permissions and its `extra_permissions`; `status` reports anything more as a problem.
- **Approval is only required where a branch rule requires it.**
  A token can push to any branch no rule protects, and merge a pull request no rule holds for review.

## Rotation

1. Generate a new private key on the App's settings page.
2. Run `recall-github-app import-key --key tpm:~/.local/state/recall-github-app/app-key-2.blob <download>`.
   The config moves to the new key; the old blob is left alone.
3. Run `recall-github-app status`.
4. Delete the old key on the settings page, matching the fingerprint, and remove the old blob.

For a key in an SSH agent, add the new key to the agent, run `import-key` with the same `ssh-agent:` socket, and delete the old key from the agent in step 4.
