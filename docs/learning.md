# Learning: how sessions become reviewed rules

This doc is the design of `recall learn`, the command that reads sessions and stages rule changes for review.
Throughout, the owner is the person whose sessions and context files recall works on.

The prompts a person types carry their corrections.
The rest of the evidence is in what the agent did, such as tool failures it repeated, skills it never used, and rules it broke mid-session.
The loop reads the full normalized event stream (`docs/events.md`), splits the work into a cheap incremental map and a weekly reduce, and keeps every rule on a ledger so it can age out.

## Shape of the loop

```
events (disk + archive) -> map (per session, cached) -> observations + rule hits
observations -> candidates (grouped and counted by the binary, inside the window)
candidates + rules + ledger -> reduce (weekly) -> proposed edits + ledger update
proposed edits -> apply (worktree per repo) -> a pull request or branch per repo -> owner review
```

- **Model calls go through the harness's own CLI.** recall runs it headless with no tools, text in on stdin and text out on stdout, as `claude -p --disallowedTools '*'` or `kiro-cli chat --no-interactive --trust-tools=`. No API client lives in the binary, so existing auth, billing, and model selection work unchanged. The session Kiro saves for such a run is deleted.
- **Only the apply step gets file tools.** It runs the same CLI with `Read Edit Write Glob Grep` and nothing else, confined to the apply step's worktrees.
  Kiro can't confine write tools to directories, so through kiro the apply gets no tools either, and the binary writes the files the model returns (text mode, under [Apply](#apply)).
- **Every change reaches the owner as a pull request or a branch.** `learn.review` picks which. `pr` pushes and opens a pull request with `gh`, and `branch` leaves the branch in place, which works with any git host or none. With `review.credentials.command` set, `pr` and the `github` evidence source act as the credential that command prints per repository, in place of the machine's own login ([pr-credentials.md](pr-credentials.md)). `review.assignees` lists the logins `pr` assigns each pull request to once it is open; a login the forge won't assign is a warning, and the pull request stays open.

The loop borrows from muse (github.com/ellistarn/muse), which runs the same observe-then-fold shape over conversation history.
The structured output lines, the fingerprint cache, the compression before the model call, and conservative folding here all started there.

## Six prompts are embedded in the binary

The prompts live in `internal/learn/prompts` and are compiled into the binary, so each release pins the wording every step runs on.
Each prompt describes its input and fixes the shape of its output, which the binary parses.
`apply-text.md` is a variant of `apply.md`, so the table counts it with apply. Changing `map.md` or either rule-hit prompt changes every session's fingerprint, so the next map reads each session again.

| Prompt | Step | What it asks for | What comes back |
| --- | --- | --- | --- |
| [`map.md`](../internal/learn/prompts/map.md) | Map | Read one compressed session and report only corrections, context, procedures, tool failures, and skill misses. It says most sessions are routine and the expected answer is `NONE`. | `Quote:` and `Observation[category]:` lines, or `NONE` |
| [`hits.md`](../internal/learn/prompts/hits.md) | Map | Judge the session against the rules listed under CURRENT RULES. It is appended to `map.md` the first time a session is read, so one call observes and judges. | `Rule[id]:` lines marked relevant, followed, or violated |
| [`hits-only.md`](../internal/learn/prompts/hits-only.md) | Map | The same judgment for a session already observed, when a rule it wasn't shown needs judging. It goes ahead of `hits.md` in place of `map.md`. | `Rule[id]:` lines, or `NONE` |
| [`reduce.md`](../internal/learn/prompts/reduce.md) | Reduce | Fold the window's candidates into proposals, starting from the rules as they are. An add needs two sessions, or one where the owner said "always" or "never". | A `## Proposals` section with one block per add, reword, retire, skill, or graduate |
| [`apply.md`](../internal/learn/prompts/apply.md) | Apply | Turn each checked proposal into an edit in a git worktree, changing only the paths the binary routed it to. | One `Applied[kind]:` or `Skipped[kind]:` line per proposal |
| [`apply-text.md`](../internal/learn/prompts/apply-text.md) | Apply, text mode | The same edits with no tools, for a runner that can't confine file edits: the input carries the files, and the model returns each changed file whole. | One block per changed file between marker lines, then the same outcome lines |
| [`replay.md`](../internal/learn/prompts/replay.md) | Replay | Act as the agent at one past moment, decide the next action, then judge that action against the recorded mistake. | A `Next:` line and a `Verdict: repeats` or `Verdict: avoids` line |

- **Empty is an explicit answer.** The map prompt expects `NONE` for most sessions, and the reduce prompt says zero proposals is a fine outcome. Models can't reliably emit empty output, so every output line has a fixed prefix and anything else is discarded.
- **Quotes make evidence checkable.** The map must put the owner's exact words, at least three of them, before every correction, context, and procedure. The binary checks each quote against the session, and a checked quote is what makes an observation owner-typed.
- **The reduce is told what the binary will reject.** Its prompt names the checks on targets, budget, scope, and pending reviews, so the one model call goes to proposals that can land.
- **The replay decides before it judges.** The prompt has the model pick its next action from the rules and the session alone. The recorded mistake is the yardstick for the verdict and never an instruction.
- **Personal details stay out.** The map, reduce, and apply prompts each forbid copying secrets, hostnames, account IDs, ARNs, internal URLs, or names of people.

## Map: one cheap pass per session

Before the model call, `recall` compresses the session mechanically.
Owner messages stay whole and assistant prose is truncated, except just ahead of a short correction, where the text that provoked it is the signal.

A small model then reads the session and records corrections, repeated context, repeated procedures, tool failures, skill misses, and rule hits.

- **Sonnet is the map default.** On a test of real sessions, haiku returned `NONE` on a session full of corrections, and sonnet caught them all.
- **Rule hits are stored apart from observations,** so a rule change never re-observes a session. A session is judged only against rules in force when it ran.
- **Every machine maps its own sessions.** The reduce reads every host's observations from the shared store, so transcripts never move.
- **A refused session is mapped as far as the model accepts,** and the same content is never retried on another model.

## Reduce: one weekly pass

Once a week, one model call reads the window's candidates, the targets, and the ledger.
It proposes new or reworded rules, skill changes, retirements, and graduations, each citing the candidates behind it.

The reduce folds conservatively.
The current rules are the prior, new observations have to justify a change, and volume is not importance.
The binary checks the output and holds back a proposal aimed outside `learn.targets` and `learn.repos`.
`extensions.learn-reduce` files join the input as owner instructions, so the owner can steer the reduce without editing its prompt.

## Native memory: an input, not a competitor

Some harnesses keep notes the agent writes for itself.
Those notes stay on one machine and nobody reviews them.
`recall` reads them as evidence and never writes them.

- **A note is never owner-typed.** It is the agent's paraphrase, so it supports a proposal and can't be its only evidence.
- **What recurs gets promoted** into a context file, where it is reviewed, versioned, and the same on every machine.

## Repeats: what the agent keeps doing

A successful tool call renders as its name alone, so the map model never notices an agent that opens the same files at the start of every session.
The binary counts that instead, with no model call.

- **The threshold.** A repo has a repeat when at least three sessions, and at least half of those the owner typed into, began with the same action.
- **Repeats are tool-derived.** They support a proposal, such as a pointer in the repo's context file, and never count as owner-typed. `learn.repeat_evidence_sessions` lets a new rule rest on repeats alone, and is off by default.

## Outside evidence: review comments and CI

A review comment or a failed check on an agent-authored change is a correction no transcript holds.
With `learn.evidence = ["github"]`, recall reads them through the `gh` CLI.
The key is empty by default.

- **Only the agent's pull requests count,** identified by the co-author trailer on their commits (`learn.evidence_trailer`). It defaults to the primary harness's: `Co-Authored-By: Claude`, or `Co-Authored-By: Kiro` with `harness.claude` off. Kiro writes no trailer of its own, so its owner adds a steering rule telling it to.
- **Other people's text is tool-derived.** It is always redacted and never counts as owner-typed, except a comment the owner wrote.

## Scope: where a rule lands

A rule in a global file costs context in every session, so a lesson that holds in one repo belongs in that repo's own context file.

- **Only listed repos.** `learn.targets` are the global files, and `learn.repos` lists the checkouts a proposal may also target. A pull request on a repo may be read by other people, so recall never finds a repo on its own.
- **Narrowest target wins.** The reduce places each rule, and the binary checks it. A repo-scoped proposal must cite only sessions that ran in that repo, and a global rule whose every session ran in one repo is held back.
- **Pull requests open on origin,** so in a fork the change never lands upstream.

## Budget: what a rule costs

The loop learns from what went wrong, so left alone its rules only accumulate.
`learn.budget` caps what one context file costs in tokens, and is off by default.

An add that would put its file over budget must name a rule in that file to remove in its place.
The apply makes both edits in one change, so the owner reviews the trade as one diff.

## Owner-typed evidence

An observation can become a rule loaded into every session, so text a tool printed could end up steering every later session.
The pull request is one gate.
The binary enforces a second.

- **The binary checks every quote.** The map quotes the owner's words behind each observation, and the binary looks for them in the prompts the owner typed. A quote it can't find is removed.
- **New rules need owner-typed evidence.** With `learn.require_owner_evidence` on, the default, a new rule, a new skill, or a hook must cite at least one checked quote.
- **The check proves the words were typed.** Whether the lesson follows from them is the reviewer's call.

## Candidates

The reduce never reads every observation ever mapped.
The binary first groups the window's observations (`learn.window_days`) into counted candidates, with no model call.

- **Grouping favors precision.** Observations join by shared wording, so a count is a floor.
- **Evidence is used once.** A candidate behind a pending proposal is held back, and one the owner merged or rejected returns only with evidence from later sessions.
- **The candidate ledger holds observation text,** so by default it stays on the machine that runs the reduce.

## Apply

`recall learn apply` stages one reduce run's proposals for review.
Nothing it does writes a live file.

- **The binary routes each proposal before a model sees it** and holds back one that is off target, reuses a taken name, or lands outside a git checkout.
- **Each repo gets a fresh worktree** on a `learn-<date>` branch.
- **One model run with file tools makes the edits,** limited to the exact paths the binary named.
- **A runner that can't confine file edits applies in text mode.** Kiro trusts a tool on any path, so it can't keep writes inside the worktrees, and it gets no tools at all.
  The input carries the current contents of every file a proposal may change, plus each worktree's `AGENTS.md` or `CLAUDE.md`, and the model answers with each changed file whole between marker lines.
- **In text mode `recall` itself makes the edits.** It writes a returned file only when its cleaned path is one the binary listed as editable for the run, inside that run's worktree, with no symlink at the file and no existing parent resolving outside the worktree.
  It refuses anything else, including a file returned twice, reports why, and doesn't apply a proposal whose outcome names a refused file.
  An answer with an unterminated block or no outcome lines fails the apply, like a failed editor run.
  A proposal whose files would push the input past 256 KiB is held back before the model runs.
- **Public repos get generalized edits.** A repo under `keep.public` has anything personal generalized, and its pull request carries no proposal text.

## Review feedback

A proposal the owner closes is a judgment, and the loop keeps it.
`recall learn reviews` lists every verdict.

- **One commit per proposal,** so the owner can drop one before merging and each proposal gets its own verdict.
- **A merge doesn't prove every proposal landed.** The binary checks each proposal against the merged base and records the ones that aren't there as rejected. A rule the owner reworded by hand still counts as landed.
- **A rejection sticks.** A rejected proposal can come back only reworded and on evidence from later sessions.
- **A slow review never gets a duplicate.** Apply holds back a proposal that matches a pending review.

## Session hooks: the default way to run the loop

The plugin carries one harness hook, on session end, so a machine needs no cron job.
It does nothing until `recall init` turns its steps on under `[hooks]`.
`recall recur` installs scheduled jobs in its place.

- **Nothing runs at session start.** A hook there could delay the session.
- **Session end returns at once.** It starts a detached worker that archives at most once a day, maps sessions quiet for six hours, and runs the reduce and apply every `hooks.learn_every` days.
- **A failing step waits a day,** so a reduce that keeps failing can't become a paid model call per session.
- **No recursion.** Every harness run recall starts carries `RECALL_HOOK=1`, and recall's hooks do nothing when they see it.
- **One reducer per store.** A reduce on every machine would send duplicate proposals, so the first `recall init` claims the store. `recall learn reducer --take` moves the claim.
- **Kiro's session-end hook can't run the loop.** It fires when an interactive session exits, but Kiro kills every process the hook started, detached or not, so the worker never finishes. With `harness.claude` off, `recall init` declares the same three steps as recur jobs, and copies its `PATH` into `recur.env` so the scheduler finds `recall` and `kiro-cli`.

## Why apply is a separate stage

The reduce proposes and the apply edits, and they stay two stages behind one command.

- `proposals.md` is the contract. The owner can read it, `--dry-run` stops at it, and a failed apply retries from it without re-running the reduce.
- The model that decides what to change never holds the pen. The reduce runs with no tools, and the apply edits only staged  git worktrees.
- Through kiro, the apply model has no pen either: Kiro can't confine write tools, so it gets none, and the binary writes the files it returns, after checking each path against the ones it listed.

## Rule ledger

The ledger is a separate file, `~/.local/state/recall/ledger.json` by default, never comments inside the context files, because comments cost context in every session.
It has one entry per rule.

```json
{
  "id": "r-0012",
  "target": "base.md",
  "text": "…",
  "text_hash": "…",
  "source_sessions": ["…"],
  "created": "2026-11-11",
  "since": "2027-01-04",
  "last_relevant": "2027-01-10",
  "last_violated": null,
  "supersedes": [{"text_hash": "…", "text": "…", "until": "2027-01-04"}]
}
```

- **Ids are stable.** `recall learn rules sync` reconciles the ledger with the bullets in the targets. A rule keeps its id and dates across a move, a reword, or a hand edit, and a deleted rule's id is never reused.
- **Hits move the dates everywhere.** The reduce updates `last_relevant` and `last_violated` from every host's rule hits.
- **The store's copy is authoritative.** The reducer writes each new version to the store, never overwriting an old one. Every other host treats its local file as a cache, and only the reducer assigns rule ids.

## Aging

- Relevant and not violated: the rule stays.
- Violated as it reads now: the rule isn't working as written; propose a rewording, citing the violations. A violation of a wording the rule has since replaced doesn't count against the new one.
- No relevance for about 90 days: propose retirement. The threshold is `learn.retire_days`, and retirement is always a proposal; the owner's review level gates every deletion.

## The model that earned a rule

A rule exists because a model needed it. A rule a newer model follows unprompted reads "relevant, followed" forever, so aging never retires it.
The ledger records the models behind each rule, and `recall learn age` flags one earned only on models no recent session ran.
That marks a rule worth a trial without it, never a retirement on that alone.

## Replay

A replay asks a model what the agent would do next at one past moment.
The model gets the session up to the prompt the agent was answering when it made the mistake, as compressed text, and answers with one next action and a verdict of `repeats` or `avoids`.
It is one call to the map model with no tools, so nothing is executed.
`learn.replay` is off by default, since every replay costs model calls.

- **A new rule is replayed before review,** once without the rule and once with it. Only a replay where the rule turns `repeats` into `avoids` lets the add through.
- **The binary compares the two verdicts.** A model never decides what is held back.
- **The same switch tests rules a new model may have outgrown.** When the agent avoids the mistake in every replay without the rule, the binary proposes retiring it.

## Graduation

A rule a script could check or enforce becomes a proposed script, and the prose rule retires when the script lands.
A rule the agent can't break beats one it has to remember.

By default the script is one the agent still has to choose to run.
With `learn.settings` set, a graduate can instead become a permission rule, such as `ask Bash(git push:*)`, or a hook script that exits 2 to block a call.
Neither leaves prose behind, so the rule stops costing context.

- **The loop can narrow what the agent may do and can't widen it.** The binary never adds an `allow` rule, even through a reviewed change.
- **The model never edits a settings file.** The binary makes that edit itself.
- **A hook script may only block.** One that could approve a call or steer the agent stops the run.

## Separation: each config keeps its own store and model

Each recall config names its own store and its own model runner.
Data from one environment never reaches another's store or model.
The map for a session runs where its transcripts are, and observations land only in that environment's store.
Hosts that must stay separate use separate stores.

## Other harnesses

An adapter emits the schema in `docs/events.md`, and the map and reduce never see raw transcripts.
Claude Code reads `CLAUDE.md` and skills, and Kiro reads steering files under `~/.kiro/steering/`.
Rules in both ride the same ledger, and the targets stay per harness so each file keeps its native conventions.

## Tune the loop once it runs

These keys are off until you set them:

| Key | What it adds |
| --- | --- |
| `learn.budget` | A [token cap](#budget-what-a-rule-costs) per context file. |
| `learn.settings` | Graduation into a [hook or permission rule](#graduation) the agent can't skip. |
| `learn.replay` | A [replay](#replay) of the moment that earned a new rule, to hold back a rule that changes nothing. |
| `learn.evidence = ["github"]` | [Review comments and failed checks](#outside-evidence-review-comments-and-ci) on the agent's pull requests, as evidence. |
| `learn.repeat_evidence_sessions` | New rules that rest on [repeats the binary counted](#repeats-what-the-agent-keeps-doing), such as a recurring tool failure. |
| `review.credentials.command` | [A bot that opens each pull request](pr-credentials.md), so you can approve it as yourself. |
| `review.assignees` | Logins assigned to each pull request recall opens. |
