# recall

_Turn your agent's history into memory._

`recall` captures transcripts from coding-agent harnesses, archives them, and learns from them, so your context files (`CLAUDE.md`, `AGENTS.md`, skills) gain the rules you keep repeating and lose the ones that stopped earning their place.
It does memory management and nothing else.

## Tenets

* **Never say it twice.**
  Correcting an agent once teaches it something.
  Having to correct it a second time means its memory failed.
* **Rules are earned.**
  A rule or skill stays only while your sessions keep showing it's needed.
  Every line costs context in every session, so one that stops earning its place goes.
* **You own your context.**
  You decide how much it changes on its own.
* **Rules graduate to code.**
  If a script can check or enforce a rule, it should, and the prose rule retires.
  A rule the agent can't break beats one it has to remember.
* **Memory, and nothing else.**
  recall captures sessions, archives them, and learns from them.
  A feature that isn't about what your agent should remember belongs elsewhere.

## Requirements

- Plain and auditable: no daemon, no self-update, no telemetry, and no network beyond the store you configure and what you schedule.
  The binary does nothing unless you run it or schedule it.
- Go (current stable) to build; `cron` on Linux or `launchd` on macOS to schedule.
- An S3 bucket (or a local directory) for the archive store.

## What it does

- `recall recur` installs recurring jobs you declare in config into cron or launchd, and nothing else on the machine.
- `recall archive-chats` uploads each week's transcripts as one zstd tarball per machine per ISO week, at `<harness>/<host>/<year>/W<week>.tar.zst`.
- `recall events` reads harness transcripts (on disk or inside an archive tarball) and prints them as normalized JSONL events.
- `recall skills` reports which skills your sessions actually invoke, so unused ones can retire.
- `recall lint` finds paths, commands, and skill names your context files mention that don't exist on this machine.
- `recall learn` is the learning loop: a cached map pass observes each session, a rule ledger tracks when each rule was last relevant or violated, and a reduce pass proposes edits for your review ([design](docs/learning.md)).
- The `keep` skill is the loop's manual form: at the end of a session, propose up to three lessons and apply the ones you approve, without waiting for a scheduled run.
- `recall config` reads and writes the layered TOML config without you parsing TOML.

## Install

```sh
go install github.com/micahhausler/recall/cmd/recall@latest
```

Or download a release binary for darwin/linux on amd64/arm64.

To use the skills, install the repo as a Claude Code plugin:

```sh
claude plugin marketplace add micahhausler/recall
claude plugin install recall@recall
```

## Configure

Config is TOML, layered: `~/.config/recall/config.toml`, then every `~/.config/recall/config.d/*.toml` in lexical order.
A later file wins key by key; `enabled = false` in a later file turns off a job or harness an earlier one declared.
`$RECALL_CONFIG` overrides the base path.

Every key is documented in [`config.example.toml`](config.example.toml).

```sh
recall config show                 # the merged result, with the file and line each value came from
recall config get store.kind       # one value
recall config set store.kind s3    # writes only to config.d/90-recall-set.toml, never to a file you edited
```

## Layout

- `cmd/recall/`: the single binary.
- `internal/`: the packages behind each subcommand.
- `skills/`: Claude Code skills that drive the subcommands.
- `docs/`: design docs, including the [events schema](docs/events.md) and the [learning loop design](docs/learning.md).

## Inspirations

- [Lamis Mukta, Learning while you sleep: Beyond memory to dreaming](https://www.youtube.com/watch?v=tTcxVv8HHNw): dream over full archived sessions out of band, tool calls included, and propose memory changes with the evidence sessions attached.
- [Kieran Klaassen, compound engineering](https://creatoreconomy.so/p/how-to-make-claude-code-better-every-time-kieran-klaassen): write the lesson down at the end of each cycle so the context files compound.
- [Mitchell Hashimoto, My AI Adoption Journey](https://mitchellh.com/writing/my-ai-adoption-journey): every rule traces to an observed bad agent behavior, so rules are earned by evidence.
- [Teresa Torres on How I AI](https://www.chatprd.ai/how-i-ai/teresa-torres-claude-code-obsdian-task-management): ask at session end what should be documented, the manual form of the learning loop.
- [Boris Cherny, how he uses Claude Code](https://x.com/bcherny/status/2007179832300581177): a CLAUDE.md that is updated from review rather than written once.
- [Shrivu Shankar, How I Use Every Claude Code Feature](https://blog.sshh.io/i/177742847/custom-subagents): CLAUDE.md as guardrails plus pointers, which is why stale paths in context files are worth linting.
- [Garry Tan, gbrain](https://github.com/garrytan/gbrain): thin harness, fat skills, and graduating deterministic rules into scripts.
