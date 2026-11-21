# recall

_Automate turning your agentic coding session history into memory, skills, and tools._

`recall` captures transcripts from coding-agent harnesses, archives them, and learns from them, so your context files (`CLAUDE.md`, `AGENTS.md`, skills) gain the rules you keep repeating and lose the ones that stopped earning their place.
It does memory management and nothing else.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/img/recall-loop-dark@2x.png">
  <img alt="The recall learning loop: sessions write transcripts to local disk; scheduled jobs on every host archive them and map them to observations in the store; one host reduces observations into proposals and applies them as pull requests; you review, and merged changes update the live context the next session reads." src="docs/img/recall-loop@2x.png">
</picture>

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
* **Native memory is an input, not a competitor.**
  Your harness's own memory notes are evidence recall reads, lints, and promotes from.
  What recurs there earns a place in a reviewed context file that travels to every machine.
* **Memory, and nothing else.**
  recall captures sessions, archives them, and learns from them.
  A feature that isn't about what your agent should remember belongs elsewhere.

## Supported harnesses

recall supports Claude Code and Kiro CLI.
Everything harness-specific sits behind interfaces, so adding Codex or another agent means adding an implementation of each, not changing the loop:

- `harness.Harness` (`internal/harness`) finds a harness's transcripts, parses them into the normalized [events](docs/events.md), lists the skills and commands it offers, and names the context files it loads.
- `model.Runner` (`internal/model`) runs the map and reduce prompts through the harness's own headless CLI, so its existing auth works unchanged.

Archive, events, skills, lint, and learn all work from those two interfaces, and each harness registers under its own `[harness.<name>]` config table.

| Harness | Transcripts | Context files | Runner |
|---|---|---|---|
| `claude` (on by default) | `~/.claude/projects` | `~/.claude/CLAUDE.md` and its `@` imports | `claude -p` |
| `kiro` | `~/.kiro/sessions`, both the V2 and v3 engine layouts | `~/.kiro/steering/**/*.md`, including `AGENTS.md`, and the Markdown files they link | `kiro-cli chat --no-interactive` |

Turn Kiro on with `recall config set harness.kiro.enabled true`, or an empty `[harness.kiro]` table in a drop-in.
With Kiro on, `learn.targets` defaults to include `~/.kiro/steering/AGENTS.md`, the global `AGENTS.md` Kiro loads in every session.
Set `learn.runner = "kiro"` to run the map and reduce through Kiro; the models then default to `auto`, and `kiro-cli chat --list-models` lists the IDs `learn.map_model` and `learn.reduce_model` accept.
`learn apply` and `learn run` refuse the Kiro runner: Kiro trusts a tool on any path, so it can't keep edits inside the apply worktrees, and apply needs `learn.runner = "claude"`.
Kiro saves every headless run as a session, so recall deletes each one it starts, and its cost is reported in credits.

## Requirements

- Plain and auditable: no daemon, no self-update, no telemetry, and no network beyond the store you configure and what you schedule.
  The binary does nothing unless you run it, schedule it, or turn on a step of its plugin's session hooks in config.
- Go (current stable) to build.
- Nothing else for the default path: the store is a local directory, proposals wait on a local git branch, and the plugin's session hook runs the loop.
  An S3 bucket, `cron` or `launchd`, and `gh` are there when you want an offsite archive, a fixed schedule, or review by pull request.

## What it does

- `recall init` sets a new machine up in one command, with no bucket, scheduler, git remote, or TOML to write: a local store, review on a local branch, and the session hook on.
  Nothing runs until you run init, and it only sets keys you haven't.
  It turns `hooks.learn_every` on only when no other machine already runs the reduce on the shared store: the first init on a store records its host under `learn/reducer` there, and a later init on another machine sees the claim and leaves the reduce off, so one store gets one reducer and no duplicate proposals.
  `recall learn reducer` says which host holds that claim, and `recall learn reducer --take` moves it to this machine, for when the holder is retired, reimaged, or renamed.
  Pin the `host` config key in a `config.d` drop-in on every machine before its first init: the claim and the store's records key on that name, and macOS hostnames drift after a name clash on the network.
  On a machine that already has config, review stays on pull requests, the default; init prints the command that moves it to a local branch instead of switching it for you.
  Every target has to sit in a git checkout, since review stages edits on a branch of it; init names any that doesn't and prints the `git init` step without running it.
- The plugin's session hook is the default way to run the loop, with no scheduler: a session's end starts the archive, the map over sessions that have gone quiet, and a weekly reduce and apply in the background, each only when its key is on (`hooks.archive`, `hooks.map`, and `hooks.learn_every`, the days between reduces).
  Nothing recall installs runs at session start, so recall can never delay one; a machine whose sessions never end cleanly is covered by a `recur` job instead.
  `recall init` warns when the binary isn't reachable from the PATH a GUI-launched harness gets.
  The first worker run maps back over the last 30 days of transcripts, the window `recall init` prints, and a reduce that read nothing doesn't count as the week's run, so your first proposal arrives in days.
- `recall recur` is the opt-in alternative for a fixed schedule: it installs recurring jobs you declare in config into cron or launchd, and nothing else on the machine.
  A step never runs twice: a hook step is skipped when an enabled job's command already runs it (`recall archive-chats run`, `recall learn map`, `recall learn run`), judged by the step's command, and `recall init` applies the same test when it decides which hook keys to set.
- `recall archive-chats` uploads each week's transcripts as one zstd tarball per machine per ISO week, at `<harness>/<host>/<year>/W<week>.tar.zst`.
  With `archive-chats.redact = true`, secret-shaped tokens in the transcripts are replaced with `[REDACTED:<kind>]` as the tarball is built, and each week's line says how many.
  Redaction is pattern-based and lossy: it catches token shapes (cloud and API keys, private key blocks, bearer headers, JWTs), not every secret, and the archive no longer holds what it replaced.
  An S3 store needs IAM grants per key prefix, and learning and reducing need more than archiving; [docs/store.md](docs/store.md) lists them.
  That is why it is off by default.
- `recall events` reads harness transcripts (on disk or inside an archive tarball) and prints them as normalized JSONL events.
- `recall skills` reports which skills your sessions actually invoke, so unused ones can retire.
  The skill list is the harness's own, and `--taken` prints every skill and command name already in use.
- `recall lint` finds paths, commands, and skill names your context files mention that don't exist on this machine, following `@` imports and Markdown links to other local files.
  `recall lint --memory` does the same for your harness's own memory notes, and reports notes its index doesn't list and notes unchanged for longer than `learn.retire_days`.
  `recall learn map` reads those notes as observations with no model call; recall never edits them.
- `recall learn` is the learning loop: a cached map pass observes each session (`learn.map_workers` at once, default 4), a rule ledger tracks when each rule was last relevant or violated, and a reduce pass proposes edits for your review ([design](docs/learning.md)).
  The binary groups alike observations into counted candidates before the reduce reads them, and remembers which ones a proposal already used (`learn.candidates`); `recall learn candidates` lists them.
  The first run can map every session on disk (`--since all`); each machine maps its own sessions, and the reduce reads every machine's observations from the store.
  `recall learn map --archives` also maps archived weeks from the store: other hosts' sessions, and this host's own that are no longer on disk, while a session still on disk keeps reading from disk.
  The reduce reads observations from the last `learn.window_days` (default: `learn.retire_days`), and `recall learn reduce --window all` is a one-off pass over every observation; retirement always uses `learn.retire_days`, so a wider window retires nothing faster.
  The rule ledger is one per store: the reducer writes it to the store after each reduce and apply, every machine loads the newest copy before mapping so rule ids match everywhere, and a deleted local ledger file is restored from the store with its ids, history, and review verdicts.
  A reduce on a machine that isn't the store's reducer refuses, naming the claim holder and this host, since only the reducer assigns rule ids; `recall learn reduce --force` runs it anyway, saving locally and leaving the store ledger unwritten.
  Reviews a forced run records are written to a per-host store object, and the reducer folds them into the shared ledger at its next sync, so the published change stays tracked and isn't re-proposed.
- `recall learn apply` stages the proposals as edits in a fresh git worktree of each repo they touch, and sends each changed repo to review: a pull request (`learn.review = "pr"`, with `gh`) or a local branch (`"branch"`), which works with any git host or none.
  Each change carries one commit per proposal, so you can drop or edit one before merging; after a merge, a proposal whose text did not land is recorded as rejected.
  A pull request's author can't approve it, so `review.credentials.command` can name a command that supplies a bot's credential per repository: the bot opens the pull request, authors its commits, and requests your review, and you review as yourself ([docs/credentials.md](docs/credentials.md)).
  Your targets, `learn.skills`, and `learn.bin` can live in one repo or several; a repo under `keep.public` gets no proposal text in its pull request.
  A lesson every session of which ran in one repo lands in that repo's own context file when you list the checkout in `learn.repos`; `recall learn targets` shows every file a proposal may touch.
  recall proposes nothing for a repo you didn't list.
  A rule a script can enforce graduates into one, and with `learn.settings` set, into a harness hook or a deny/ask permission rule the agent can't skip; the binary makes that settings edit itself and never adds an allow rule.
  `learn.settings` may name any tracked JSON settings file; when a generator writes your `settings.json` (so it is ignored), point the key at the tracked layer the generator merges, like `settings.base.json`, and regenerate after merging — a graduate into a git-ignored settings file is held back.
  A rule that lives in a repo's own context file graduates into that repo's `.claude/settings.json` instead, with no `learn.settings` needed, and only when every cited session ran in that repo.
  The reduce sees what every rule costs in tokens per session, and with `learn.budget` set, a new rule in a full file must name the rule it displaces.
  A new rule or skill must rest on words you typed: the binary checks each quoted observation against the session, and holds back a proposal whose only evidence is what a tool or model reported (`learn.require_owner_evidence`).
  Repeats the binary counted — an agent opening the same files at the start of session after session, or the same tool failure, keyed by tool and normalized error text, recurring across sessions — are labeled tool-derived in proposals and pull requests; `learn.repeat_evidence_sessions = N` additionally lets a new rule rest on a repeat seen in at least N sessions, and is off (0) by default.
  Expected nonzero exits — a `grep` or `rg` that matched nothing, a `diff` that found differences, a false `test` — are dropped before the map reads a session and are never counted as failures.
  With `learn.evidence = ["github"]`, off by default, the map also reads review comments and failed checks on pull requests in the `learn.repos` origins whose commits carry the agent's co-author trailer, through `gh`; they are tool-derived evidence too, unless you wrote the comment.
  `learn.evidence_trailer` sets the trailer the source looks for, defaulting to `Co-Authored-By: Claude`; set it when your harness writes a different trailer or attribution is off.
  With `learn.replay = true`, off by default, the reduce replays the moment that earned each new rule, re-running the session up to your correction once with your context file as it is and once with the rule added, and holds back an add whose two runs end the same way, since the rule changed nothing; replays run headless on the map model, at most 8 moments per run, and a failed replay blocks nothing.
  The same switch retires what newer models outgrew: a rule earned only on models no recent session ran is replayed without it, and when the current model avoids the mistake in every replay, the reduce proposes the retire, citing the replays, for the same review as any other change.
  `recall learn run` is rules sync, reduce, and apply in one command, for one machine's schedule.
  Your verdicts feed back: a pull request you close is a rejection the next reduce reads, one still open holds its proposals back from a duplicate, and `recall learn reviews` lists them.
- The `keep` skill is the loop's manual form: at the end of a session, propose up to three lessons and apply the ones you approve, without waiting for a scheduled run.
- The `report-issue` skill drafts a GitHub issue on this repo — recall version, OS, harness, what happened, what was expected, and nothing personal — shows you the full title and body, and files it with `gh` only after you say yes.
- `recall config` reads and writes the layered TOML config without you parsing TOML.
- `recall extension NAME` prints your extension files for a skill or the reduce prompt; see [Extend](#extend).

## Install

With Homebrew, on macOS or Linux; the tap is this repo:

```sh
brew tap micahhausler/recall https://github.com/micahhausler/recall
brew install --cask recall
```

With Go:

```sh
go install github.com/micahhausler/recall/cmd/recall@latest
```

Or download a release archive for macOS or Linux on amd64 or arm64 from the releases page; `checksums.txt` beside them has each archive's SHA-256.

To use the skills, install the repo as a Claude Code plugin (the skills run in Claude Code; the binary reads every enabled harness):

```sh
claude plugin marketplace add micahhausler/recall
claude plugin install recall@recall
```

The skills run in Claude Code (not the Claude desktop app) and drive the `recall` binary.
`recall init` is the short path for a new machine; the `recall-setup` skill walks one through every choice, including an S3 store and scheduled jobs.
The plugin also installs the session-end hook, which does nothing until `[hooks]` in your config turns a step on.

## Configure

Config is TOML, layered: `~/.config/recall/config.toml`, then every `~/.config/recall/config.d/*.toml` in lexical order.
A later file wins key by key; `enabled = false` in a later file turns off a job or harness an earlier one declared.
`$RECALL_CONFIG` overrides the base path.

Every key is documented in [`config.example.toml`](config.example.toml).

```sh
recall config show                 # the merged result, with the file and line each value came from
recall config get store.kind       # one value
recall config set store.kind s3    # writes only to config.d/90-set.local.toml, never to a file you edited
```

## Extend

Add your own steps to a recall skill or the reduce and apply prompts without forking them.
List files under `[extensions]`, keyed by skill name (`keep`, `recur`, `archive-chats`, `report-issue`), `learn-reduce`, or `learn-apply`:

```toml
[extensions]
keep = ["~/.config/recall/keep.md"]
```

Each skill ends with an "Owner additions" section that loads `recall extension <name>` when the skill starts, and the reduce and apply append `learn-reduce` and `learn-apply` to their input as owner instructions.
The files are instructions with the skill's permissions, so keep them in your own dotfiles.
For tools of your own, keep a separate private repo or plugin and point these files at it.

## Layout

- `cmd/recall/`: the single binary.
- `internal/`: the packages behind each subcommand.
- `skills/`: Claude Code skills that drive the subcommands.
- `docs/`: design docs, including the [events schema](docs/events.md) the [learning loop design](docs/learning.md), and the [store's keys and IAM permissions](docs/store.md).

## Inspirations

- [Lamis Mukta, Learning while you sleep: Beyond memory to dreaming](https://www.youtube.com/watch?v=tTcxVv8HHNw): dream over full archived sessions out of band, tool calls included, and propose memory changes with the evidence sessions attached.
- [Kieran Klaassen, compound engineering](https://creatoreconomy.so/p/how-to-make-claude-code-better-every-time-kieran-klaassen): write the lesson down at the end of each cycle so the context files compound.
- [Mitchell Hashimoto, My AI Adoption Journey](https://mitchellh.com/writing/my-ai-adoption-journey): every rule traces to an observed bad agent behavior, so rules are earned by evidence.
- [Teresa Torres on How I AI](https://www.chatprd.ai/how-i-ai/teresa-torres-claude-code-obsdian-task-management): ask at session end what should be documented, the manual form of the learning loop.
- [Boris Cherny, how he uses Claude Code](https://x.com/bcherny/status/2007179832300581177): a CLAUDE.md that is updated from review rather than written once.
- [Shrivu Shankar, How I Use Every Claude Code Feature](https://blog.sshh.io/i/177742847/custom-subagents): CLAUDE.md as guardrails plus pointers, which is why stale paths in context files are worth linting.
- [Garry Tan, gbrain](https://github.com/garrytan/gbrain): thin harness, fat skills, and graduating deterministic rules into scripts.
