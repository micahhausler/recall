# Learning: the loop that replaces distill

The map step, rule ledger, aging, reduce, apply, and review feedback are built (`recall learn`).

distill read only the prompts a person typed, once a week, and asked one model call to turn them into context-file edits.
That caught corrections but missed most of the evidence: tool failures the agent repeated, skills it never used, rules it broke mid-session.
This design reads the full normalized event stream (`docs/events.md`), splits the work into a cheap incremental map and a weekly reduce, and keeps every rule on a ledger so it can age out.

## Shape

```
events (disk + archive) -> map (per session, cached) -> observations
observations + rules + ledger -> reduce (weekly) -> proposed edits + ledger update
proposed edits -> apply (worktree per repo) -> a pull request or branch per repo -> owner review
```

Three interfaces carry it:

- **`model.Runner`** (`internal/model`): runs a harness CLI headless (`claude -p --disallowedTools '*'`, stdin in, stdout out).
  No API client lives in the binary, so existing auth, billing, and model selection work unchanged.
- **`model.Editor`** (`internal/model`): the same CLI with file tools only (`Read Edit Write Glob Grep`), confined to the apply step's worktrees.
- **`review.Publisher`** (`internal/review`): hands a committed change to the owner, keyed by `learn.review`: `pr` pushes and opens a pull request with `gh`, `branch` leaves the branch in place.

Targets are still plain paths in `learn.targets`; a Target type with a line budget waits until a budget is needed.

Borrowed from muse (github.com/ellistarn/muse), which runs the same observe-then-fold shape over conversation history:

- Structured prefixes over "return nothing": models can't reliably emit empty output, so `Observation[...]:` lines are the contract and everything else is discarded at parse time, with `NONE` as the explicit empty.
- The cache fingerprint covers the rendered session, the prompt, the model, and the rule ids shown, so changing any of them re-observes and nothing needs a manual flush.
- Mechanical compression before the model call: owner messages whole, code blocks collapsed, assistant prose truncated, except ahead of a short correction, where the text that provoked it is the signal.
- Conservative folding: the current rules are the prior, new observations must justify changes, and volume is not importance.

## Map: one cheap pass per session

A small model reads one session's events and writes structured observations:

- **Corrections**: the user pushed back on output ("no, shorter", "use X not Y").
- **Repeated context**: background typed into more than one prompt.
- **Repeated procedures**: step lists written out more than once; skill candidates.
- **Tool failures**: the same tool failing the same way more than once (`tool_result` with `ok = false`).
- **Skill misses**: a session that did by hand what an existing skill does, without invoking it.
- **Rule hits**: moments where an existing rule was relevant, followed, or broken, keyed by rule id.

Observations are JSON in the store under `analysis/<harness>/<session id>-<content hash>.json`.
The content hash makes reruns incremental: a session already mapped at that hash is skipped, and a session that grew (append-only transcripts) is mapped again under the new hash.
The map prompt gets the current rule list (ids and text) so rule hits are grounded, but it proposes nothing; it only observes.
Sessions map `learn.map_workers` at once, each an unsaved headless run, so recall never reads its own calls back as sessions.
A session with no owner prompts is cached without a model call.
A refused session is mapped as far as the model accepts (see Refusal handling below).
Superseded observations stay in the store; the reduce reads only the newest per session.

## Reduce: one weekly pass

The reduce gathers the window's observations, the targets, the ledger, and the names already taken, and runs one model call that proposes:

- new or reworded rules, each citing the observations (and so the sessions) that earned it;
- skill changes, when a procedure repeats or a skill's description doesn't match how it's asked for;
- retirements, from the aging policy below;
- graduations, below.

`extensions.learn-reduce` files, when set, join the input as OWNER INSTRUCTIONS, so the owner can steer the reduce without editing its prompt.
The binary checks the output: an add, reword, or retire whose target isn't in `learn.targets` lands under "Off-target proposals", and a new skill whose name is taken lands under "Name collisions".

## Apply

`recall learn apply` reads one reduce run's `proposals.md` (the newest by default) and stages it for review; nothing it does writes a live file.

- The binary routes each proposal before a model sees it: an add, reword, or retire to its target; a new skill to `learn.skills/<name>/SKILL.md`; a changed skill to the file it names; a graduate's script to `learn.bin/<name>`, with the targets for the pointer that replaces the prose.
  It holds back a proposal that is off target, reuses a taken name, or lands outside any git checkout, and says why.
- The repos are whatever checkouts hold those paths, so the rules and the skills can share one repo or use several.
  Each gets a fresh worktree on a `learn-<date>` branch from its origin's default branch; an existing branch of that name is an earlier apply waiting for review, and stops the run.
- One `model.Editor` run (`learn.apply_model`) gets every worktree and the exact path each proposal may change, and reports one `Applied[...]` or `Skipped[...]` line per proposal.
  `extensions.learn-apply` files join its input as OWNER INSTRUCTIONS.
- Each changed worktree runs `learn.check`, when set, on its staged diff, then commits and goes to the publisher; an unchanged one is removed with its branch.
  A repo under `keep.public` is marked public: its edits generalize anything personal, and its pull request carries no proposal text.
- The run directory keeps `apply-input.md`, the model's `apply.md`, and `apply.json`, which records each change and every outcome and marks the run as applied, so a scheduled rerun doesn't open a second pull request.

`recall learn run` is rules sync, reduce, and apply in one command, for the one machine that runs the reduce.

## Review feedback

A proposal the owner closes is a judgment, and the loop keeps it.

- Each proposal an apply publishes goes on the ledger as a pending review, with where it landed: a pull request URL, or a branch and its commit.
- Before each reduce and apply, the publisher reports what became of each pending review: `gh pr view` for a pull request, where a squash or rebase merge counts as merged; for a branch, its commit reachable from the base is merged, and the branch deleted without that is closed.
  A publisher that can't answer leaves its reviews pending, with a warning.
- The reduce input lists pending reviews and rejections within `learn.retire_days` under REVIEWS, and dates each observation, so a rejected proposal returns only with evidence observed after the rejection, reworded.
- Apply holds back a proposal that matches a pending review, by title or rule text, so a slow review never gets a duplicate; and one whose rule text matches a rejection exactly.
- A merged review needs nothing more: `rules sync` puts its bullets on the ledger, and its skill's name becomes taken.

`recall learn reviews` runs the same check and lists every review with its verdict.
Deletions get the same review as additions: a retire is an edit on a branch like any other.

## Why apply is a separate stage

The reduce proposes and the apply edits, and they stay two stages behind one command because the seam between them carries weight.

- `proposals.md` is the contract: the artifact the owner can read, the thing `--dry-run` stops at, and the checkpoint a failed apply retries from without re-running the reduce.
- The model that decides what to change never holds the pen.
  The reduce runs with no tools, the apply edits only staged worktrees, and the binary, not a model, checks targets and names.
- The owner's review level (`learn.review`) plugs in at the seam, between deciding and landing.

## Rule ledger

A sidecar file next to the context files (for example `.recall-ledger.json`), never comments inside them, because comments cost context in every session.
One entry per rule:

```json
{
  "id": "r-2026-0012",
  "target": "base.md",
  "text_hash": "…",
  "source_sessions": ["…"],
  "created": "2026-11-11",
  "last_relevant": "2027-01-10",
  "last_violated": null
}
```

The reduce updates `last_relevant` and `last_violated` from rule-hit observations.
Rules are matched to ledger entries by id, with the text hash catching hand edits (a hand-edited rule gets a fresh ledger entry and keeps its history by reference).

## Aging

- Relevant and not violated: the rule stays.
- Violated: the rule isn't working as written; propose a rewording, citing the violations.
- No relevance for about 90 days: propose retirement.
  The threshold is config, and retirement is always a proposal; the owner's review level gates every deletion.

## Graduation

A rule a script could check or enforce becomes a proposed script (a lint, a hook, a config default), and the prose rule retires when the script lands.
The reduce marks candidates ("this correction is about formatting, time math, or which account is active; a command can check it"); the proposal includes the script sketch.
A rule the agent can't break beats one it has to remember.

## Separation

Each recall config names its own store and its own model runner.
Data from one environment never reaches another's store or model: the map for a session runs in the environment that holds that session's transcripts, and observations land only in that environment's store.
Shared learning across environments, if ever, is a deliberate export, not a default.
`learn map --archives` reads other hosts' archives only from this config's store, so it stays inside one environment.

## Other harnesses

An adapter must emit the schema in `docs/events.md`; the map and reduce never see raw transcripts.
What each harness reads as context decides where proposals land:

- Claude Code reads `CLAUDE.md` (with `@` imports) and skills; built and tested.
- Codex reads `AGENTS.md`; adapter unwritten, format unverified.
- Kiro reads steering files under `.kiro/steering/`; adapter unwritten, format unverified.

A rule that holds everywhere can be proposed against every harness's targets at once; the targets stay per-harness so each file keeps its native conventions.

## Open questions

- ~~Map model and cost.~~ Measured on real sessions: haiku returned NONE on a session full of corrections; sonnet caught them all at about $0.05 to $0.08 per long session, so sonnet is the map default.
- **Observation schema.** Fixed fields (above) or free-form with a validator; fixed is easier to reduce, free-form loses less.
- ~~Cross-host reduce.~~ Each machine maps its own sessions and caches the observations in the store; the reduce reads every host's observations, so a rule repeated across machines adds up without moving transcripts.
  `learn map --archives` maps other hosts' archived weeks from one machine, opt-in because it downloads them.
  Map and reduce warn when a host has archives in the last 14 days but no observations.
  Hosts that must stay separate use separate stores.
- **Candidate rules.** distill's "said once, promote when said again" lifecycle; does the ledger subsume it (a rule with one source session is a candidate) or is a separate candidate state still useful?
- ~~Refusal handling.~~ When the map model refuses a session, the map reads the longest prefix it accepts, counted in owner prompts.
  The search starts below the session's own first refusal, less the prompt that drew it, then binary-searches down.
  The result records how much was read (`partial`) and is cached, so later runs don't pay to be refused again.
  The same content is never retried on another model.
- **Ledger merge.** Two machines proposing ledger updates to one repo will conflict; one reducer per repo (like one distiller per side) is the simple answer.
