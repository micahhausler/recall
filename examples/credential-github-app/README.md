# credential-github-app

An example recall credential command (`docs/pr-credentials.md`) that gives recall a GitHub App installation token.
With it, the App's bot opens the pull requests recall proposes, and you review and approve them as yourself.

It is built on `api/credentials/v1/command`.
`command.Run` reads the request from `RECALL_EXEC_INFO`, refuses to write a token to a terminal, and writes the answer.
The one function the example writes itself is `grant`, which says where the token comes from.

## What it does

For each repository recall asks about, it:

1. Reads the App's private key from a PEM file.
2. Signs an RS256 JWT with the standard library, issued by the App's client ID.
3. Finds the App's installation on the repository.
4. Mints an installation token for that one repository, with Contents and Pull requests at write and Checks and Commit statuses at read.
5. Refuses the token when GitHub scoped it to anything else.
6. Answers with the token, its expiry, the App's bot as the commit identity, and your login from `--owner`.

It covers repositories on github.com only.

## Set it up

Register a GitHub App with the permissions above, install it on the repositories recall works in, and download a private key from the App's settings page.
Keep the key file readable by you alone.

Build the command:

```sh
go build -o ~/bin/credential-github-app ./examples/credential-github-app
```

Point `review.credentials.command` at it, with your App's client ID, key path, and login.
recall runs the command without a shell, so the key path is absolute:

```sh
recall config set review.credentials.command '["credential-github-app", "--client-id", "Iv1.example", "--key", "/home/you/.config/recall/app.pem", "--owner", "you"]'
```

## Keep the key somewhere better

A key file on disk is the simplest place for the App's key, and the easiest one to copy off the machine.
Only `grant` reads the key, so moving it changes that one function and nothing else.

- **A TPM** can hold the key so it never leaves the chip, and the TPM signs the JWT.
- **A 1Password vault** can hold the PEM, and `grant` fetches it with `op read` each time it needs a token, so the key never touches the disk.

## Try it

The command refuses to print to a terminal, so pipe it through `jq` and drop the token:

```sh
RECALL_EXEC_INFO='{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{"repo":"github.com/you/example"}}' \
  credential-github-app --client-id Iv1.example --key ~/.config/recall/app.pem --owner you | jq '.status | del(.token)'
```

## Tests

The tests pass `run` a request string and a buffer, sign JWTs with a key they generate, and answer GitHub's calls from an `httptest` server, so they never reach the network.

```sh
go test ./examples/credential-github-app
```
