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
  `harness.go` holds the table of what recall knows about each harness before any config: its CLI, paths, trailer, models, and whether its session-end hook can run the loop.
  `prices.json` is the only place a model price lives: per provider, its pricing page and the date checked, and per model name recall passes to the CLI, USD per million tokens or Kiro credits per call; `hack/prices` refreshes it.
  A new harness is a constant and an entry there, plus an adapter in `internal/harness/<name>` listed in `internal/harness/adapters` and a runner in `internal/model/<name>` listed in `internal/model/runners`, both under that constant.
- `internal/store`: the archive store interface, with `fs` and `s3` backends, an in-memory fake, and `Pending`, which keeps observe's `analysis/` writes on disk until `Upload`.
- `internal/recur`: recurring jobs reconciled into cron (Linux) or LaunchAgents (macOS).
- `internal/archive`: the weekly transcript archive.
- `internal/redact`: the token-shape redaction the archive applies when `archive-chats.redact` is on.
- `internal/harness`: the `Harness` interface and what every adapter shares: week file selection and archive reading.
  Each coding agent's transcript format and native memory is a sub-package: `claude` and `kiro`, which also installs recall's Kiro power.
  `stored` reads recall's own normalized events, such as an import's stored sessions.
  `internal/harness/adapters` lists every adapter and opens one by config name; a harness recall doesn't know by name sets `format` to read through one of them under its own name, or `events` to read recall's own events.
- `internal/evidence`: evidence sources outside the chat, read behind `learn.evidence`; `github` (review comments and failed checks on agent-authored pull requests, through `gh`) is the only one today.
- `internal/events`: recall's side of the normalized event schema (`docs/events.md`), aliases of `api/events/v1` plus the JSONL writer.
- `internal/clock`: the injected clock; nothing calls `time.Now` directly where a week boundary matters.
- `internal/model`: the headless harness-CLI runner model calls go through; never an API client.
  It holds the `Runner` and `Editor` interfaces, their fakes, and the scratch-dir exec every runner shares.
  Each harness's runner is a sub-package: `claude`, the only one with an `Editor`, and `kiro`.
  `internal/model/runners` lists every runner and opens one by `learn.runner` name.
  Only the apply step's `Editor` gets tools, and only file tools confined to its worktrees.
- `internal/learn`: the learning loop (`docs/learning.md`): render, map with cache, rule hits, ledger, aging, candidates, scope, evidence checks, reduce, apply.
  The binary, not a model, makes every check that holds a proposal back, and every edit to a harness settings file.
- `internal/review`: staging an edit for review: a git worktree per repo, one commit per proposal, and the publishers (`pr`, `branch`) that hand it over.
- `internal/credential`: the credential a configured command supplies per repository (`docs/pr-credentials.md`), for the `pr` publisher and the `github` evidence source to act as; recall never knows where a token comes from.
- `api/credentials/v1`: the credential contract's object as Go types, so a credential command can import it.
  A change to its JSON is a change to commands other people wrote: add optional fields, never rename or remove one.
- `api/credentials/v1/command`: the command's side of the contract, for a credential command written in Go: parse the request, refuse to print a token to a terminal, write the answer.
  It, the types, `api/events/v1`, and `api/imports/v1` are the only packages outside `internal/`, and code other people wrote calls them: add to their exported names, never rename or remove one.
  Nothing about where a token comes from, or about any host or provider, goes in it, and nothing recall's binary is built from imports it.
- `internal/imports`: running an import plugin (`docs/import.md`): the `Source` interface, `Exec` and `Fake`, validation, redaction, storing, state, and the lock.
  The binary validates every file a plugin writes; nothing in the output runs.
- `api/imports/v1` and `api/imports/v1/command`: the import contract's types, and the plugin's side of it for a plugin written in Go: parse the request, write session files within the limits, print the answer.
  Like the credential packages, add to their JSON and exported names, never rename or remove one, and nothing recall's binary is built from imports `command`.
- `api/events/v1`: the event schema as a contract (`recall.events/v1`): `Event`, the `Kind` constants, `Check`, `CheckSession`, and `Summarize`, standard library only.
  Events grow by optional fields and new kinds, never a rename or a removal.
- `cmd/recall-github-app`: `main` for `recall-github-app`, the credential command that mints a GitHub App installation token per repository (`docs/github-app.md`); GoReleaser ships it as its own archive and cask.
- `internal/githubapp`: everything `recall-github-app` is built from; nothing outside it and `cmd/recall-github-app` imports it, so `recall` links none of it.
  `cli` holds flags and dispatch in `cli.go`, one file per command beside it, and `token.go` supplies only the token to `command.Run`; a new flag goes in `flagsFor`, and tests run as `testName`, so a name hardcoded outside `Name` fails them.
  `app` is the App (JWT, installation, token, bot user), with `apptest.Fake`, the in-memory GitHub every test uses; `ghapi` is one REST call with its retries; `appkey` parses a key and computes GitHub's fingerprint; `manifest` is GitHub's manifest flow.
  `keys` turns a `<kind>:<path>` reference into a signer: `file`, `tpm` (only `device_linux.go` touches a real TPM; tests use a simulator), and `ssh-agent` (`agenttest.Fake` is an in-process agent); `config` is the per-machine config file.
- `examples/import-example`: an example import plugin built on `api/imports/v1/command`, which maps a synthetic source file of flat turns and of a message tree; `cmd/recall`'s end-to-end import test builds and runs it.
  CI builds and tests it, and GoReleaser never ships it.
- `hack/gendocs`: renders `docs/commands.md` from the built binary's help and `docs/config.md` from its template and the schema's defaults; prose lives only in its templates.
  `go generate ./hack/gendocs` rewrites both, and its test fails when either differs from what it renders.
- `hack/prices`: refreshes `internal/config/prices.json` from each provider's pricing page with one model call through `internal/model`, writing only an answer that passes the table's checks; its tests use a fake runner.
  GoReleaser ships neither `hack` command.
- `internal/extension`: the owner's extension files that skills and the reduce append at load time.
- `internal/hook`: what the plugin's session hooks need: the hook input, the recursion guard, and the worker's lock and schedule.
- `hooks/hooks.json`: the plugin's SessionStart and SessionEnd hooks, which run `recall hook`; session start only reads the repo note for the session's repo (`learn.repo_notes`), and never calls a model or the store.
- `plugin.json`: the Agent Plugins manifest, which installs the same skills as a Kiro power; `.claude-plugin/plugin.json` is Claude Code's.
- `embed.go`: package `recall` at the module root, which embeds `plugin.json`, `skills/keep`, and `skills/recall-setup` as the Kiro power `recall init` installs through `kiro-cli powers install`; only `cmd/recall` imports it.
  Kiro rejects a skill whose frontmatter isn't strict YAML, so a skill the power ships quotes any value that holds `: `.
- `skills/<name>/SKILL.md`: one skill per subcommand, named like it, ending in the "Owner additions" block that loads `recall extension <name>` (except recall-setup, which runs before the binary exists).
- `config.reference.toml`: the reference, every config key commented out with its default or a marked example, decisions first and "Advanced" last; `config.example.toml`: only what `recall init` writes, with the runner and its models commented out at their defaults.
  `internal/config/examples_test.go` holds both to the schema and to `Fresh`, and lists the keys the reference leaves out, each named for the change that removes it.

## Rules

- Standard library first.
  Allowed dependencies: `pelletier/go-toml/v2`, AWS SDK for Go v2 (config, sts, s3), `klauspost/compress/zstd`, `micahhausler/aws-iam-policy` (typed IAM policy documents, for the policies recall prints and the bucket policy it sets and verifies), `spf13/pflag`, and any `golang.org/x/` package.
  `api/credentials/v1/command` uses `golang.org/x/term` to refuse to print a token to a terminal: a file's mode can't tell a terminal from the null device, which is a character device too.
  `internal/githubapp` alone also uses `google/go-tpm` (the TPM) and `google/go-tpm-tools` (its simulator, tests only).
  `internal/githubapp` uses `golang.org/x/crypto` for the SSH agent protocol.
  Anything else needs a written reason.
- Every pluggable part is a Go interface with a small registry keyed by the config name, and an in-memory fake in its package.
  Don't add an interface with only one plausible implementation.
- Skills read and write config through `recall config get|set`, never by parsing TOML.
- No test calls a model, AWS, or GitHub, or touches the real crontab, LaunchAgents, `$HOME`, a real TPM, or a real SSH agent; tests use fakes, simulators, and temp dirs.
  The opt-in exceptions are `TestRealDevice`, which runs only with `RECALL_TPM_TEST=1`, and `TestRealAgent`, which runs only with `RECALL_AGENT_TEST` and `RECALL_AGENT_PEM` set; `TestOpenSSHAgentSignsRS256` starts its own throwaway `ssh-agent` and never talks to yours.
- A new config key needs a reason a constant or a derived value won't do; say it in the pull request.
  Every key is in the schema and in `config.reference.toml`: a decision most owners make gets a comment of at most three lines, and everything else goes under "Advanced" with one line.
  The README names a key only when a new owner must choose its value in the first week.
  Mechanics belong in `docs/`, never in a config comment.
  Removing a key means adding a row for it, with the text that replaces it, to `removedKeys` in `internal/config/schema.go`; a key the schema doesn't know warns and is ignored.
  A config key never changes between a value and a table; a new shape takes a new key name.
- A command meant for people goes in `recall --help`, and a command called by the timer, skills, hooks, or scripts goes in `recall --help --all`.
  Leaving a command out of `--help` removes it from the README and top-level help, never from the binary, and a hidden command stays callable across releases.
- User-facing text says observe, propose, proposals, and "the proposing machine"; map, reduce, and reducer are design-doc and code terms.
- Memory management and nothing else: a feature that isn't about capturing, archiving, or learning from sessions belongs elsewhere.
  Enforcing the owner's style preferences, for example, is out of scope even when a script could.
  The rule's scope is the `recall` binary: `recall-github-app` is an optional companion, a separate binary and cask for anyone who wants a bot to open recall's pull requests.

## GitHub App command

- A token never reaches a log, an error, a test name, or a terminal; `status` proves a token mints without showing one.
  To keep a token out of output, parse the JSON and drop the field, never a regex over the text.
- `token` only reads: it never writes the config or caches a token on disk.
- A key file is never replaced or deleted, except the download `import-key` destroys last; a TPM blob may be the only copy of a key.
- What can't be done twice goes last: ask GitHub everything and check every place a file must go before keeping a key GitHub hands over once or destroying a download; `keep` in `importkey.go` is the one order both commands use.
- Nothing adds a key to an SSH agent; a key in an agent is picked by its fingerprint, and its signature is verified before use.
- No fallback identities, and a token GitHub scoped wider than asked is dropped.
- A POST or DELETE GitHub may have seen is never retried, and the manifest exchange is attempted once.

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
CGO_ENABLED=0 go build ./...   # releases build without cgo; only the TPM simulator tests use it
gofmt -l .        # must print nothing
gitleaks dir . --redact
claude plugin validate .
```
