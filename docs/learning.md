# Learning: the loop that replaces distill

The map step, rule ledger, aging, and the reduce plumbing are built (`recall learn`); applying proposals to live files and the PR flow are not yet.

distill read only the prompts a person typed, once a week, and asked one model call to turn them into context-file edits.
That caught corrections but missed most of the evidence: tool failures the agent repeated, skills it never used, rules it broke mid-session.
This design reads the full normalized event stream (`docs/events.md`), splits the work into a cheap incremental map and a weekly reduce, and keeps every rule on a ledger so it can age out.

## Shape

```
events (disk + archive) -> map (per session, cached) -> observations
observations + rules + ledger -> reduce (weekly) -> proposed edits + ledger update
```

Two interfaces carry it:

- **`model.Runner`** (built, `internal/model`): runs a harness CLI headless (`claude -p --disallowedTools '*'`, stdin in, stdout out).
  No API client lives in the binary, so existing auth, billing, and model selection work unchanged.
- **`context.Target`** (still just paths in `learn.targets`): one file the loop proposes edits to.
  A real Target type (path, line budget, staged copies the model may edit) comes with the apply step.

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

## Reduce: one weekly pass

The reduce gathers the window's observations, the current targets, and the ledger, and runs one model call that proposes:

- new or reworded rules, each citing the observations (and so the sessions) that earned it;
- skill changes, when a procedure repeats or a skill's description doesn't match how it's asked for;
- retirements, from the aging policy below;
- graduations, below.

Edits are staged file copies, reviewed and applied by the binary, never written live by the model.
The owner's chosen review level decides what happens next: open a PR, write a pending note, or apply directly; never delete without the owner's chosen level of review.

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
- **Cross-host reduce.** The archive holds every machine's weeks; should the reduce read other hosts' archived sessions, and does that break the separation rule when hosts share a store?
- **Candidate rules.** distill's "said once, promote when said again" lifecycle; does the ledger subsume it (a rule with one source session is a candidate) or is a separate candidate state still useful?
- **Refusal handling.** distill dropped gated chats and salvaged around refusals; the map sees refusal events, and the policy for what it may read from such sessions needs an owner decision.
- **Ledger merge.** Two machines proposing ledger updates to one repo will conflict; one reducer per repo (like one distiller per side) is the simple answer.
