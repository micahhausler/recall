# recall

One Go binary, `recall`, that archives coding-agent transcripts and learns from them.
The README covers what each subcommand does; `docs/` holds the designs.

## Nothing personal, ever

This repo is public.
Never add anything about a specific person: names, handles, emails, hostnames, account IDs, ARNs, internal URLs, or employers.
Personal values belong in `~/.config/recall/`, never here.
Examples and tests use placeholders like `you`, `example.com`, and `laptop-a`.

## Layout

- `cmd/recall/main.go`: flag parsing and subcommand dispatch only; logic lives in `internal/`.
- `internal/config`: the layered TOML loader and the `config get|set|show` writer.
- `internal/store`: the archive store interface, with `fs` and `s3` backends and an in-memory fake.
- `internal/recur`: recurring jobs reconciled into cron (Linux) or LaunchAgents (macOS).
- `internal/archive`: the weekly transcript archive.
- `internal/redact`: the token-shape redaction the archive applies when `archive-chats.redact` is on.
- `internal/harness`: one adapter per coding agent's transcript format and native memory: `claude` and `kiro`.
- `internal/evidence`: evidence sources outside the chat, read behind `learn.evidence`; `github` (review comments and failed checks on agent-authored pull requests, through `gh`) is the only one today.
- `internal/events`: the normalized event schema (`docs/events.md`).
- `internal/clock`: the injected clock; nothing calls `time.Now` directly where a week boundary matters.
- `internal/model`: the headless harness-CLI runner model calls go through; never an API client.
  Only the apply step's `Editor` gets tools, and only file tools confined to its worktrees.
- `internal/learn`: the learning loop (`docs/learning.md`): render, map with cache, rule hits, ledger, aging, candidates, scope, evidence checks, reduce, apply.
  The binary, not a model, makes every check that holds a proposal back, and every edit to a harness settings file.
- `internal/review`: staging an edit for review: a git worktree per repo, one commit per proposal, and the publishers (`pr`, `branch`) that hand it over.
- `internal/credential`: the credential a configured command supplies per repository (`docs/pr-credentials.md`), for the `pr` publisher and the `github` evidence source to act as; recall never knows where a token comes from.
- `api/credentials/v1`: the credential contract's object as Go types, so a credential command can import it.
  A change to its JSON is a change to commands other people wrote: add optional fields, never rename or remove one.
- `api/credentials/v1/command`: the command's side of the contract, for a credential command written in Go: parse the request, refuse to print a token to a terminal, write the answer.
  It and the types are the only packages outside `internal/`, and commands other people wrote call them: add to their exported names, never rename or remove one.
  Nothing about where a token comes from, or about any host or provider, goes in it, and nothing recall's binary is built from imports it.
- `examples/credential-github-app`: an example credential command built on `api/credentials/v1/command`, which mints a GitHub App installation token per repository.
  It is a `main` package in recall's module, so CI builds and tests it against the current contract, and GoReleaser, which builds only `cmd/recall`, never ships it.
- `internal/extension`: the owner's extension files that skills and the reduce append at load time.
- `internal/hook`: what the plugin's session hooks need: the hook input, the recursion guard, and the worker's lock and schedule.
- `hooks/hooks.json`: the plugin's SessionEnd hook, which runs `recall hook`; nothing runs at session start.
- `plugin.json`: the Agent Plugins manifest, which installs the same skills as a Kiro power; `.claude-plugin/plugin.json` is Claude Code's.
- `skills/<name>/SKILL.md`: one skill per subcommand, named like it, ending in the "Owner additions" block that loads `recall extension <name>` (except recall-setup, which runs before the binary exists).
- `config.example.toml`: every config key, with a comment.

## Rules

- Standard library first.
  Allowed dependencies: `pelletier/go-toml/v2`, AWS SDK for Go v2 (config, sts, s3), `klauspost/compress/zstd`, `micahhausler/aws-iam-policy` (typed IAM policy documents, for the policies recall prints and the bucket policy it sets and verifies), `golang.org/x/term` (in `api/credentials/v1/command` only, to refuse to print a token to a terminal: a file's mode can't tell a terminal from the null device, which is a character device too).
  Anything else needs a written reason.
- Every pluggable part is a Go interface with a small registry keyed by the config name, and an in-memory fake in its package.
  Don't add an interface with only one plausible implementation.
- Skills read and write config through `recall config get|set`, never by parsing TOML.
- No test calls a model or AWS or touches the real crontab, LaunchAgents, or `$HOME`; tests use fakes and temp dirs.
- A new or changed config key goes in three places: the code, `config.example.toml`, and the README or skill that uses it.
- Memory management and nothing else: a feature that isn't about capturing, archiving, or learning from sessions belongs elsewhere.
  Enforcing the owner's style preferences, for example, is out of scope even when a script could.

## Writing

- In Markdown, put one sentence per line.
- Use American spelling: judgment, behavior, color.
- State claims plainly, without hedges.

## Releasing

A release is a tag `vX.Y.Z` on main; the release workflow runs GoReleaser and updates the Homebrew cask.
The workflow refuses a tag that doesn't match `version` in `.claude-plugin/plugin.json` and `plugin.json`, so bump both in the same change and tag the commit that carries it.
Order: bump both plugin manifests, push main, wait for test and gitleaks to pass, then tag that commit and push the tag.

## Checks

Run these before saying a change is done; CI runs the same set:

```sh
go vet ./...
go test ./...
gofmt -l .        # must print nothing
gitleaks dir . --redact
claude plugin validate .
```
