# Contributing

recall is one Go binary plus the Claude Code skills that drive it.
Bug reports, fixes, and new harness adapters are all welcome.

## What belongs in recall

recall captures agent sessions, archives them, and learns from them.
A feature that isn't about what your agent should remember belongs in another project.
For anything larger than a bug fix, open an issue first so the design is agreed before you write the code.

## Build and test

You need a current stable Go.
Nothing else is required to build or to run the tests.

```bash
git clone https://github.com/micahhausler/recall
cd recall
go build ./cmd/recall
go test ./...
```

CI runs the checks below, so run them before you push.
The last two need gitleaks and the Claude Code CLI installed.

```bash
go vet ./...
go test ./...
gofmt -l .                  # must print nothing
gitleaks dir . --redact
claude plugin validate .
```

## Layout

- `cmd/recall/`: the single binary.
- `internal/`: the packages behind each subcommand.
- `skills/`: Claude Code skills that drive the subcommands.
- `docs/`: design docs, including the [events schema](docs/events.md), the [learning loop design](docs/learning.md), and the [store's keys and IAM permissions](docs/store.md).
- `api/credentials/v1/`: the public credential contract and its Go helper, the only packages another project can import.
- `hooks/hooks.json`: the plugin's session-end hook.
- `config.example.toml`: every config key, with a comment.

## Conventions

- **Logic lives in `internal/`.** `cmd/recall/main.go` only parses flags and dispatches subcommands.
- **Standard library first.** A new dependency needs a written reason in the pull request.
- **Pluggable parts are interfaces with a fake.** A harness, store, evidence source, or publisher is a Go interface with a small registry keyed by its config name and an in-memory fake in the same package. Don't add an interface that has only one plausible implementation.
- **Tests stay off the network and out of your home directory.** They never call a model or AWS, and never touch the real crontab, LaunchAgents, or `$HOME`. Use the fakes and temp dirs.
- **A config key changes in three places.** Update the code, `config.example.toml`, and the README or skill that uses the key.
- **Skills go through the binary.** A skill reads and writes config with `recall config get` and `recall config set`, never by parsing TOML.
- **The credential contract only grows.** `api/credentials/v1` is public: add optional fields, and never rename or remove a field or an exported name.

## Common changes

- **A harness adapter.** Implement `harness.Harness` in a new `internal/harness/<name>` package and list its `New` in `internal/harness/adapters` under its config name. It must emit the schema in `docs/events.md`, and nothing downstream should need to change. Test it against fixture transcripts in a temp dir, then add it to the README's harness table and `config.example.toml`.
- **An evidence source.** Add it in `internal/evidence` beside `github`. It is turned on by name in `learn.evidence`.
- **A store backend.** Implement the interface in `internal/store` beside `fs` and `s3`, and say in `docs/store.md` what permissions it needs.
- **A skill.** Each skill is `skills/<name>/SKILL.md` and ends with an "Owner additions" block. Run `claude plugin validate .` after changing one.
- **The learning loop.** Read `docs/learning.md` first. A change to what the loop does updates that doc in the same pull request.

## Keep personal details out of the repo

This is a public repo for a tool that reads private transcripts.
Never commit a real name, handle, email, hostname, account ID, ARN, internal URL, or employer.
Use placeholders such as `you`, `example.com`, and `laptop-a` in examples and tests.
Don't paste a real session transcript into a test fixture or an issue; write a minimal one by hand.
`gitleaks` runs in CI and fails a change that carries a secret.

## Writing docs

- Put one sentence per line in Markdown, so a diff shows the sentence that changed.
- Use American spelling.
- State claims plainly, without hedges.
- The docs in `docs/` describe what is built. Change them in the same pull request as the behavior.

## Pull requests

- Keep each pull request to one change, with its tests.
- Say what changed and why in the description, and link the issue when there is one.
- Make sure the checks under Build and test pass.
- Contributions are licensed under Apache 2.0, the project's license.

## Report a bug or a vulnerability

For a bug, open an issue with your recall version, OS, harness, the command you ran, and its output. Redact anything that came from a transcript first.

For a vulnerability, don't open a public issue. Report it privately through the repository's Security tab.

## Releasing

A release is a `vX.Y.Z` tag on `main`. The release workflow runs GoReleaser and updates the Homebrew cask, and it rejects a tag that doesn't match `version` in `.claude-plugin/plugin.json` and `plugin.json`.

1. Bump `version` in `.claude-plugin/plugin.json` and `plugin.json`, and push `main`.
2. Wait for the test and gitleaks checks to pass.
3. Tag that commit and push the tag.
