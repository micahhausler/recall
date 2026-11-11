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
- `internal/harness`: one package per coding agent's transcript format; `claude` is the only one today.
- `internal/events`: the normalized event schema (`docs/events.md`).
- `internal/clock`: the injected clock; nothing calls `time.Now` directly where a week boundary matters.
- `internal/model`: the headless harness-CLI runner model calls go through; never an API client.
- `internal/learn`: the learning loop (`docs/learning.md`): render, map with cache, ledger, aging, reduce.
- `internal/extension`: the owner's extension files that skills and the reduce append at load time.
- `skills/<name>/SKILL.md`: one skill per subcommand, named like it, ending in the "Owner additions" block that loads `recall extension <name>` (except recall-setup, which runs before the binary exists).
- `config.example.toml`: every config key, with a comment.

## Rules

- Standard library first.
  Allowed dependencies: `pelletier/go-toml/v2`, AWS SDK for Go v2 (config, sts, s3), `klauspost/compress/zstd`.
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
The workflow refuses a tag that doesn't match `version` in `.claude-plugin/plugin.json`, so bump the plugin version in the same change and tag the commit that carries it.
Order: bump plugin.json, push main, wait for test and gitleaks to pass, then tag that commit and push the tag.

## Checks

Run these before saying a change is done; CI runs the same set:

```sh
go vet ./...
go test ./...
gofmt -l .        # must print nothing
gitleaks dir . --redact
claude plugin validate .
```
