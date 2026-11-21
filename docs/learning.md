# Learning: the loop that replaces distill

The map step, rule ledger, aging, candidates, reduce, apply, review feedback, outside evidence from review and CI, and replay are built (`recall learn`).

distill read only the prompts a person typed, once a week, and asked one model call to turn them into context-file edits.
That caught corrections but missed most of the evidence: tool failures the agent repeated, skills it never used, rules it broke mid-session.
This design reads the full normalized event stream (`docs/events.md`), splits the work into a cheap incremental map and a weekly reduce, and keeps every rule on a ledger so it can age out.

## Shape

```
events (disk + archive) -> map (per session, cached) -> observations + rule hits
observations -> candidates (grouped and counted by the binary, inside the window)
candidates + rules + ledger -> reduce (weekly) -> proposed edits + ledger update
proposed edits -> apply (worktree per repo) -> a pull request or branch per repo -> owner review
```

Three interfaces carry it:

- **`model.Runner`** (`internal/model`): runs a harness CLI headless, stdin in, stdout out: `claude -p --disallowedTools '*'`, or `kiro-cli chat --no-interactive --trust-tools=`, which deletes the session Kiro saves for the run.
  No API client lives in the binary, so existing auth, billing, and model selection work unchanged.
- **`model.Editor`** (`internal/model`): the same CLI with file tools only (`Read Edit Write Glob Grep`), confined to the apply step's worktrees.
- **`review.Publisher`** (`internal/review`): hands a staged change to the owner, keyed by `learn.review`: `pr` pushes and opens a pull request with `gh`, and `branch` leaves the branch in place, which works with any git host or none.


Borrowed from muse (github.com/ellistarn/muse), which runs the same observe-then-fold shape over conversation history:

- Structured prefixes over "return nothing": models can't reliably emit empty output, so `Observation[...]:` lines are the contract and everything else is discarded at parse time, with `NONE` as the explicit empty.
- The cache fingerprint covers the rendered session, the prompt, and the model, so changing any of them re-observes and nothing needs a manual flush.
- Mechanical compression before the model call: owner messages whole, code blocks collapsed, assistant prose truncated, except ahead of a short correction, where the text that provoked it is the signal.
- Conservative folding: the current rules are the prior, new observations must justify changes, and volume is not importance.

## Map: one cheap pass per session

A small model reads one session's events and writes structured observations:

- **Corrections**: the user pushed back on output ("no, shorter", "use X not Y").
- **Repeated context**: background typed into more than one prompt.
- **Repeated procedures**: step lists written out more than once; skill candidates.
- **Tool failures**: the same tool failing the same way more than once (`tool_result` with `ok = false`).
  An expected nonzero exit — a `grep` or `rg` that matched nothing, a `diff` that found differences, a false `test`, judged from the command and its output — isn't rendered as a failure, so the model never judges that noise.
- **Skill misses**: a session that did by hand what an existing skill does, without invoking it.
- **Rule hits**: moments where an existing rule was relevant, followed, or broken, keyed by rule id.

Observations are JSON in the store under `analysis/<harness>/<session id>-<fingerprint>.json`.
The fingerprint is the rendered session, the map prompt, and the map model, which makes reruns incremental: a session already mapped at that fingerprint is skipped, and a session that grew (append-only transcripts) is mapped again under the new one.
With `archive-chats.redact` on, observation text and quotes pass through the same redactor as the archives before they are written.
The map proposes nothing; it only observes.

Rule hits are a second result with their own record, under `analysis/hits/<harness>/<session id>-<fingerprint>.json`, so a rule change never re-observes a session.
A session read for the first time is observed and judged in one model call, and the two results are stored apart.
Each hits record lists the rule wordings the model was shown (id and text hash), so a later run judges only what is missing:

- A session is judged only against rules in force when it ran: a rule's `since` on or before the session's last day, or a rule whose wording predates the ledger.
  A session can't follow or break a rule that wasn't written yet, so adding a bullet calls the model for no session that ended before it.
  A session whose events carry no readable time is skipped for hits and judged against no rule, since nothing says which rules were in force when it ran.
- A rule in force that an earlier judgment of the same content wasn't shown, for example one mapped on a machine whose ledger was behind, is judged alone, with a hits-only prompt.
- Every judgment of a session reaches the ledger, and a hit counts only for a rule that has read the way the model was shown it.

Sessions map `learn.map_workers` at once, each an unsaved headless run, so recall never reads its own calls back as sessions.
A session with no owner prompts is cached without a model call.
A refused session is mapped as far as the model accepts (see Refusal handling below).
Superseded observations stay in the store; the reduce reads only the newest per session.
Each record carries the session's own first and last event times beside the time it was mapped.
Evidence and rule hits are dated by the day the session last ran, so mapping an old session today is not new evidence and moves no rule's dates to today.
A record written before that has only its map time; the map and reduce warn how many, and `recall learn map --since all` reads them again.
Every run recomputes the ledger's relevance and violation dates from all stored hits, instead of folding new hits into the old values, so re-reading those sessions moves a date stamped at map time back to the day the session ran.

Each record also carries the session's scope: the directory most of its turns ran in, the git checkout that holds it, and that checkout's origin as `host/path`.
Paths are written relative to the home directory, so no user name reaches the store, and a session in a git worktree belongs to the worktree's main checkout.
The map reads `.git` on the machine the session ran on and runs nothing; a session mapped from another host's archive keeps only its directory.
A checkout rooted at the home directory, such as a dotfiles repo, claims no session: the lookup stops at home, so `~/.git` never claims everything under it.
A candidate names the repo its sessions ran in, or says how many, which is what lets a lesson from one repo land in that repo's own context file.

## Reduce: one weekly pass

The reduce gathers the window's candidates, the targets, the ledger, and the names already taken, and runs one model call that proposes:

- new or reworded rules, each citing the candidates (and so the sessions) that earned it;
- skill changes, when a procedure repeats or a skill's description doesn't match how it's asked for;
- retirements, from the aging policy below;
- graduations, below.

`extensions.learn-reduce` files, when set, join the input as OWNER INSTRUCTIONS, so the owner can steer the reduce without editing its prompt.
The binary checks the output: an add, reword, or retire whose target isn't in `learn.targets` or a `learn.repos` checkout lands under "Off-target proposals", and a new skill whose name is taken lands under "Name collisions".

## Native memory: an input, not a competitor

Harnesses now keep memory of their own: notes the agent writes for itself while it works.
Those notes stay on the machine that wrote them, nobody reviews them, and nothing checks whether one is still used.
recall reads them as evidence and governs them with the rules context files get; it never writes them.

- `harness.Harness.Memories` lists a harness's notes on this machine.
  The `claude` adapter reads Claude Code's auto memory, `<project>/memory/*.md` beside the transcripts, with each note's frontmatter (`type`, `description`, the session it came from, when it changed).
  Codex memories aren't read yet; the format is unverified.
- `recall learn map` stores one observation record per note, with no model call: category `memory`, the note's description, dated by when the note last changed, and placed in the repo its project ran in.
  A changed note is a new record, and the newest wins.
- A note and the session it says it came from count as one session of evidence, so a lesson the map observed and the agent also noted isn't counted twice.
- A `memory` observation is the agent's paraphrase, so it is never owner-typed: it supports a proposal and can't be its only evidence.
- What recurs in notes and sessions gets promoted by the reduce into a context file, where it is reviewed, versioned, and the same on every machine.
- `recall lint --memory` lints the notes: stale paths, commands, and skill names, a note the index doesn't list, an index line that points at a missing note, and a note unchanged for longer than `learn.retire_days`.

## Repeats: what the agent keeps doing

The map model reads what the owner typed and what failed.
A successful tool call renders as its name alone, so the model never sees an agent that opens the same files at the start of every session to find its way around.
The binary counts that instead, with no model call.

- Each observation record keeps the session's first 15 look-around actions, from the tool-input summaries in `docs/events.md`: files read, and globs, greps, and listing or searching shell commands (`ls`, `find`, `rg`, and the like).
  A chained shell line is split into its parts, and a file printed with `cat` or `head` is the same action as one opened with the Read tool.
  The session's own directory is written as `.` and a home directory as `~`, so the same action in another worktree or on another machine reads the same.
  A session with no owner prompts is cached without a model call and still carries its actions.
- At reduce time, a repo has a repeat when at least three sessions, and at least half of the sessions the owner typed into, began with the same action.
  Three, because two can be one task split over two sittings.
- A repeat becomes one `repeat` observation on each session that took part, naming up to eight actions and how many sessions shared each, so it reaches the reduce as one counted candidate for that repo.
  It carries the source `repeat` and is tool-derived evidence: it supports a proposal, such as a pointer in the repo's context file, and never counts as owner-typed.
  The reduce input, the proposals, and the pull requests that carry them label repeat evidence tool-derived, so a reviewer sees that no person reported it.
- With the key above 0, the binary also counts tool failures across sessions, keyed by the tool and its normalized error text, with paths made placeless so the same failure reads the same in every worktree and on every machine.
  A failure recurring in at least that many sessions becomes a `repeat` observation on each of its sessions, tool-derived like any repeat, so a failure the agent recovers from each time can still earn a rule.
  Expected nonzero exits — a no-match `grep` or `rg`, a `diff` that found differences, a false `test` — are never counted, and the map never sees them as failures.
- With `learn.repeat_evidence_sessions` set above 0, a new rule may rest on repeats alone when the binary counted the repeat in at least that many sessions; the key is off (0) by default, so the owner-typed gate stands unchanged until the owner turns it on.
  Nothing but an add ever rests on repeats: a new skill or a hook still needs owner-typed evidence.

## Outside evidence: review comments and CI

A review comment or a failed check on an agent-authored change is a correction no transcript holds.
An evidence source (`internal/evidence`) reads them, beside the harnesses, behind `learn.evidence`, which is empty by default.

- The `github` source reads each `learn.repos` origin's recent pull requests through the `gh` CLI, never an API client, so existing gh auth works unchanged.
  One batched GraphQL query per repository fetches the owner's login and the pull requests with their commits, comments, reviews, and checks, so a scan costs one `gh` call per repo.
  Only a pull request whose commits carry the agent's co-author trailer counts: those are the agent's changes, and feedback on them is feedback on the agent.
- Issue comments, review bodies, and the comments left on the diff become observations with source `review`; failed checks and statuses become observations with source `ci`.
  A bot's comment is skipped: it is machine output wearing a login, and the failed checks already carry the CI side.
- A pull request stands in for a session: its comments and failures are one record under `analysis/`, dated by their own times and scoped to the repo, so a lesson from review can land in that repo's own context file.
  `recall learn map` reads the sources after the sessions, with no model call, and the reduce reads the records like any other observation.
- The text is other people's, so it always passes through the redactor, and a `review` or `ci` observation is tool-derived: it supports a proposal and never counts as owner-typed.
  The one exception is a comment the owner wrote, matched against the login gh is authenticated as, which carries the owner source the way a verified quote does.

## Scope: where a rule lands

A rule in a global file costs context in every session, so a lesson that holds in one repo belongs in that repo's own context file.

- **Targets.** `learn.targets` are the global files.
  `learn.repos` lists the checkouts whose own files a proposal may also target; for each, the binary finds `CLAUDE.md`, `.claude/CLAUDE.md`, or `AGENTS.md` at the root, path-scoped rules under `.claude/rules` with the paths their frontmatter names, and a directory's own `CLAUDE.md` or `AGENTS.md` up to three levels down.
  Only files git tracks count: review stages edits in a worktree, where an untracked or ignored file doesn't exist, so a checkout at `~` doesn't pull in every context file under the home directory.
  A listed repo with no root file offers a new `CLAUDE.md`.
  `recall learn targets` prints the list, and `rules sync` puts every existing file's bullets on the ledger.
- **Only listed repos.** Proposals land only in `learn.targets` and the checkouts listed in `learn.repos`; recall never finds a repo on its own.
- **Never an unknown repo.** A pull request on a repo may be read by other people, so recall proposes nothing for a repo it didn't get from the owner's list or the owner's own sessions, and `learn.repos` is empty by default.
  The reduce names a repo-only lesson with no home under "Repo lessons without a home" and leaves it out of the proposals.
- **Placement is the reduce's output.** The reduce input groups the targets by scope, and each candidate names the repo its sessions ran in.
  The reduce picks the narrowest target that holds the rule.
- **The binary checks it.** A proposal scoped to a repo — a new or reworded rule in its context file, a skill inside the checkout, or a graduate of one of its rules — must cite only sessions that ran in that repo, by the run's `evidence.json`.
  The reduce lists one that doesn't under "Scope problems" and the apply holds it back.
  A new rule aimed at a global file whose every session ran in one repo is held back the same way, with its reason in the same section: the rule belongs in that repo's own context file, where only its sessions pay for it.
- **Rule hits follow scope.** A repo's own rule is judged only against sessions that ran in that repo, matched by origin, or by path on the machine that ran them.
- **Pull requests open on origin.** The `pr` publisher passes origin to `gh` as the repo, so in a fork the change never lands on the repo the fork came from.

## Budget: what a rule costs

The loop learns from what went wrong, so left alone its rules only accumulate.
A budget is the counterweight the binary can enforce.

- The reduce input says what each target costs, in tokens estimated at four characters each, and what each rule costs: its tokens in every one of the window's sessions that load its file, all of them for a global file and a repo's own for that repo's files.
- `learn.budget` caps one target's cost; 0, the default, is no cap.
- A new rule that would put its target over the budget must name, with `- displaces:`, a rule in that same target to remove in its place.
  The reduce lists an add that doesn't under "Over budget", and the apply holds it back.
  A reword that grows its rule counts the growth against the budget the same way.
  A run's adds are counted together, in order: several that each fit alone can't all land in a file with room for one, and one rule can't be displaced twice.
  The apply model gets the displaced rule's text and removes it in the same change, so the owner reviews the trade as one diff.
- What `- displaces:` names is validated whenever it is set, even when the target has room: the rule must be on the ledger, live in the same target, and not be displaced twice in one run, since the apply removes it either way.
  After the apply model finishes, the binary confirms the displaced text is gone from the staged file, and publishes nothing otherwise.
  The displaced rule goes on the ledger as retired with the add's review, so it keeps the same history a retire gets.

## Owner-typed evidence

Tool output and assistant prose reach the map, and an observation becomes a rule loaded into every session, so what a tool printed could steer the agent for good.
The pull request is the gate, and the loop adds a second one the binary enforces.

- The map quotes the owner's words behind each correction, context, and procedure.
- The binary checks every quote against the prompts the owner typed in that session, ignoring case, wrapping, and typographic quotes.
  A quote it finds marks the observation **owner-typed**; one it can't find is removed, so nothing a model wrote is shown as the owner's words.
- The reduce input labels each observation, and a reduce run writes `evidence.json` beside `proposals.md`: for each candidate, its sessions, their repos, and whether each holds an owner-typed observation.
- A quote counts only at three words or more, and each piece an ellipsis leaves must hold three of its own and sit in the prompt in the quote's order, on word boundaries; a word or two, or a few scattered short pieces, is in most prompts.
  The check proves the owner typed those words in that session; whether the observation follows from them is the reviewer's call, with the quote beside the proposal.
- With `learn.require_owner_evidence` on, the default, a new rule, a new skill, or a graduate enforced by a hook must cite a candidate with at least one owner-typed observation; a hook runs code on every matching event, so it is held to the same bar as a new rule.
  The reduce lists the ones that don't under "Unsupported proposals", and the apply holds them back, reading `evidence.json`, not the proposal's own text.
  The one exception is opt-in: `learn.repeat_evidence_sessions` lets a new rule rest on repeats the binary counted in enough sessions (see Repeats), and is off by default.
  A reword, retire, or other graduate changes a rule the owner already reviewed, and isn't held to it.
  For that reason a reword, retire, or graduate must name, with `- replaces:`, a rule on the ledger that lives in a target it edits; text or enforcement that replaces nothing is a new rule under another name, and the apply holds it back.
- A proposal's kind and target are the model's text, so neither is a way round a check: a changed skill's target must be an existing `SKILL.md`, and a graduated script's name must be a plain file name.

## Candidates

The reduce never reads every observation ever mapped.
The binary turns the window's observations into candidates first, with no model call, and `recall learn candidates` prints them as the reduce will read them.

- **Window.** Only observations from sessions that last ran within the reduce window count: `learn.window_days` when set, `learn.retire_days` otherwise.
  `recall learn reduce --window all` is a one-off pass over every observation.
  Retirement always uses `learn.retire_days`, so a wider window surfaces old lessons without retiring rules faster.
- **Grouping.** A new observation joins the candidate whose words are most like its own, weighted so a rare shared word (a tool name, a file name) counts for more than a common one, and otherwise starts a new candidate.
  New observations are taken oldest session first, so the same window always gives the same candidates.
  The ledger stores each assignment, and a later run keeps it: an observation stays with the candidate it first joined, even when a closer candidate has appeared since, so evidence behind a proposed or rejected candidate is consumed and never seeds or supports another.
- **Counting.** Each candidate carries the number of distinct sessions behind it and the days they span.
  The threshold favors precision: on real observations every pair it joined was one lesson, and many true repeats, worded differently by the map, stayed apart.
  A count is a floor, and the reduce cites every candidate a proposal rests on.
- **Used.** The binary records which candidates each proposal cited.
  One whose proposal is waiting for review is held back.
  One the owner merged or rejected returns only with evidence from sessions after that verdict, with a note saying what was decided and how many sessions since.
  A proposal that never reached review doesn't use its candidates up.
- **Bound.** The reduce reads at most 200 candidates, most recurring first, and four observations of each; the rest are counted in the input, not shown.

The candidate ledger (`learn.candidates`) holds each candidate's id, its first observation's wording, the proposals that cited it, and the candidate each observation joined.
Counts are recomputed from the store on every run; membership is not.
It holds observation text, so it defaults to local state on the machine that runs the reduce.
In `proposals.md` the binary follows each cited candidate id with the sessions behind it.

## Apply

`recall learn apply` reads one reduce run's `proposals.md` (the newest by default) and stages it for review; nothing it does writes a live file.

- The binary routes each proposal before a model sees it: an add, reword, or retire to its target; a new skill to `learn.skills/<name>/SKILL.md`; a changed skill to the file it names; a graduate's script to `learn.bin/<name>`, with the targets for the pointer that replaces the prose.
  It holds back a proposal that is off target, reuses a taken name, or lands outside any git checkout, and says why.
- The repos are whatever checkouts hold those paths, so the rules and the skills can share one repo or use several.
  Each gets a fresh worktree on a `learn-<date>` branch from its origin's default branch; an existing branch of that name is an earlier apply waiting for review, and stops the run.
- One `model.Editor` run (`learn.apply_model`) gets every worktree and the exact path each proposal may change, and reports one `Applied[...]` or `Skipped[...]` line per proposal.
  `extensions.learn-apply` files join its input as OWNER INSTRUCTIONS.
- The harness refuses a headless edit to any file under a `.claude` directory, whatever the permission mode or allow rules, and the default targets sit under `~/.claude/`.
  So every `.claude` directory the input names is copied to a `shadow` folder in the run directory, the model edits the copies, and the binary copies each changed or new file back into the worktree before anything is staged.
- Each changed worktree runs `learn.check`, when set, on its staged diff, then commits and goes to the publisher; an unchanged one is removed with its branch.
  A repo under `keep.public` is marked public: its edits generalize anything personal, and its pull request carries no proposal text.
- The run directory keeps `apply-input.md`, the model's `apply.md`, and `apply.json`, which records each change and every outcome and marks the run as applied, so a scheduled rerun doesn't open a second pull request.

`recall learn run` is rules sync, reduce, and apply in one command, for the one machine that runs the reduce.

## Review feedback

A proposal the owner closes is a judgment, and the loop keeps it.

- Each proposal an apply publishes goes on the ledger as a pending review, with where it landed: a pull request URL, or a branch and the proposal's own commit.
  A change is one commit per proposal, so the owner can drop one commit before merging and each proposal gets its own verdict.
  The review is linked back to its proposal by the plan's own route, carried through the apply, never by the title the apply model echoes, and it records the rule text as it landed in the staged diff, since the model may tighten wording as it lands a rule.
- Before each reduce and apply, the publisher reports what became of each pending review: `gh pr view` for a pull request, where a squash or rebase merge counts as merged; for a branch, its proposal's commit reachable from the base is merged, and the branch deleted without that is closed.
  A publisher that can't answer leaves its reviews pending, with a warning.
- A merge says the change landed, not that every proposal in it did.
  After a merge, the binary checks each add and reword against the merged base: a proposal whose own commit is an ancestor of the base landed, whatever the wording became; a squash or rebase merge loses that commit, so the fallback is the proposal's landed rule text, matched as an exact whole bullet line so a short rule can't match text that was already in the base.
  A proposal the owner reworded by hand before merging landed too: a top-level bullet in a file the proposal's commit changed, new since that commit's parent, whose words mostly match the proposed rule (Jaccard similarity of 0.5 or more).
  The bullet has to be new, so a rejected reword doesn't match the rule it would have replaced.
  A proposal that passes none of these is recorded as rejected: the owner dropped its commit, or took its text out, before merging.
  That check fetches the remote base first, so reading a verdict can make a network call; a remote that can't be read leaves the check to local refs, and never fails the run.
  The text fallback is on the exact text the ledger keyed the review to, so rewording a bullet before merging (when its commit didn't survive the merge) is recorded as a rejection of the original proposal, while the owner's own wording joins the ledger through `rules sync` like any hand-written rule.
- The reduce input lists pending reviews and rejections within `learn.retire_days` under REVIEWS, and a candidate a rejected proposal used returns only with evidence from sessions after the rejection, so the proposal can come back only reworded and on new evidence.
- Apply holds back a proposal that matches a pending review, by title or rule text, so a slow review never gets a duplicate; and one whose rule text matches a rejection exactly.
- A merged review needs nothing more: `rules sync` puts its bullets on the ledger, and its skill's name becomes taken.

`recall learn reviews` runs the same check and lists every review with its verdict.
Deletions get the same review as additions: a retire is an edit on a branch like any other.

## Session hooks: the default way to run the loop

The plugin carries one harness hook (`hooks/hooks.json`), on session end, so a machine needs no cron job or LaunchAgent; `recur` jobs stay the opt-in for a fixed schedule.
It does nothing until `[hooks]` in the config turns a step on, so nothing runs until `recall init`.
Nothing recall installs runs at session start: a hook there could delay the session, so there is none, and `recall init` warns instead when the binary isn't reachable from the PATH a GUI-launched harness gets (the system directories, plus Homebrew's on macOS), which reads no shell profile.
A machine whose sessions never end cleanly is covered by a `recur` job instead of the hook.
A step never runs twice: the worker skips a step an enabled `recur` job's command already runs (`recall archive-chats run`, `recall learn map`, `recall learn run`), judged by the step's command and never by whether any job exists, and `recall init` applies the same test when it decides which hook keys to set.

- **Session end** runs `recall hook session-end`, which starts a detached worker and returns at once; a hook can't hold a session open.
  The worker takes a lock, so one runs at a time, and writes to `~/.local/state/recall/hook.log`.
  It stops before its lock goes stale, and a step that fails is tried again a day later, not at the end of every session, so a reduce that keeps failing can't become a paid model call per session.
  It does what a scheduled machine would, in order: the weekly archive at most once a day (`hooks.archive`), the map over sessions quiet for six hours (`hooks.map`), and rules sync, reduce, and apply every `hooks.learn_every` days.
  Six hours, so a session picked up again the same day is mapped once, not each time it ends.
  The first worker run maps back over the transcripts still on disk, bounded by the same 30-day window `recall init` prints for the map; later runs carry on from where the last one left off.
  A reduce that had no candidates to read doesn't count as the period's run, so the first sessions a fresh install maps are reduced the next day, and the first proposal arrives in days, not a week.
- **No recursion.** Every harness run recall starts carries `RECALL_HOOK=1`, and recall's hooks do nothing when they see it.
  The harness fires session hooks for a headless run too; without the guard, the map's own model calls would each start another worker.
- **`recall init`** sets a new machine up for this path: review on a local branch, the hook steps on, and the store left at its default, a local directory.
  It sets only keys no config file sets, through the drop-in `recall config set` writes, and calls no model.
  Review moves to a local branch only on a machine with no config at all: one with config relies on `learn.review`'s default, pull requests, so init leaves the key alone and prints the command that changes it.
  Every target has to sit in a git checkout; init names one that doesn't and prints the `git init` step without running it.
- **One reducer per store.** A reduce on every machine would send duplicate proposals.
  Init turns `hooks.learn_every` on only when no other machine has claimed the store: the first init records its host under `learn/reducer` in the store, as a write that never overwrites, and a later init on another machine reads the claim and leaves the reduce off, saying which host runs it and which host this is.
  The reducer is also the only host that writes the store's rule ledger (see Rule ledger); a reduce run on an unclaimed store claims it first.
  A reduce on any other machine refuses, naming the claim holder, this host, and the handoff command, unless `--force` runs it the old way: a local save, a note, and no store write.
  A store that can't be read warns and runs, so an offline hook never wedges.
  `recall learn reducer` says which host holds the claim, and `recall learn reducer --take` overwrites it with this host — the one store object a command replaces — for when the holder is retired, reimaged, or renamed; the next reduce here then publishes the ledger normally.
  The claim and every store record key on the `host` config name, which defaults to the short hostname, so pin `host` in a `config.d` drop-in on every machine before its first init; macOS hostnames drift, for example after a name clash on the network.

## Why apply is a separate stage

The reduce proposes and the apply edits, and they stay two stages behind one command because the seam between them carries weight.

- `proposals.md` is the contract: the artifact the owner can read, the thing `--dry-run` stops at, and the checkpoint a failed apply retries from without re-running the reduce.
- The model that decides what to change never holds the pen.
  The reduce runs with no tools, the apply edits only staged worktrees, and the binary, not a model, checks targets and names.
- The owner's review level (`learn.review`) plugs in at the seam, between deciding and landing.

## Rule ledger

A separate ledger file beside the context files (for example `.recall-ledger.json`), never comments inside them, because comments cost context in every session.
One entry per rule:

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

`recall learn rules sync` reconciles the ledger with the top-level bullets in the targets.
A rule's id is stable across a reword: the rule keeps its id, its creation date, and its relevance and violation dates, and the wording it replaced goes under `supersedes`.
Sync matches a vanished rule to a new bullet in this order:

1. the same text in another target, which is the rule moved;
2. a reword the loop proposed for that rule id (`- replaces:` in the proposal), landed as proposed;
3. the most alike new bullet in the same target, by shared words, which catches a hand edit.

A bullet nothing matches is a new rule, and a rule nothing matches was deleted.
A target file that isn't on this machine says nothing about its rules, and they stay as they are.
A deleted rule's id is never given to a later rule.
`since` is the day the current wording took effect: the day a new bullet appeared, or the day of its last reword.
Sync stamps it from the target file's own modification time when that is earlier than the sync day, so a session between a hand edit and the next sync is still judged against the new wording.
It is empty for a bullet that was already in a file the first time the ledger read it, since nobody recorded when it was written.
A new rule the loop proposed carries the sessions its proposal cited as `source_sessions`.

The reduce updates `last_relevant` and `last_violated` from rule hits.
Hits are stored per host, and every run folds every host's hits, so a hit mapped on one machine moves the dates of the same rule everywhere.

The store holds the authoritative ledger for every machine that shares it.
After each reduce and apply, the reducer — the host recorded under `learn/reducer` in the store — writes the ledger there as a new version under `ledger/`, never overwriting an old one; the newest wins.
On S3, that write and the claim need grants a machine that only archives doesn't have; [store.md](store.md) lists every key and the IAM actions it needs.
Before a map, every host loads the newest store version, so the rule ids it shows the model are the reducer's, and a bullet not yet on the store ledger is not judged until the reducer syncs it.
Only the reducer assigns rule ids: on any other host the local ledger file is a cache of the store's copy, restored when the file is missing and replaced, with a one-line note, when it diverges.
A review such a host recorded that the store copy lacks — a forced reduce and apply there publishes a change but can't write the store ledger — survives the replacement on that host's cache, and the host also writes it to a per-host object under `analysis/reviews/<host>/`, the way hosts write their own observations and hits.
The reducer folds every host's review objects into the ledger when it syncs, deduped by the review's key, so the reduce sees the published change as pending and doesn't re-propose it, and its verdict is tracked like any review.
The fold is idempotent: a review already on the ledger, whatever its verdict, wins over the stored object, so an object never resurrects one.
The ledger remembers each object it folded, so a later sync skips reading it, and once the store's newest ledger version carries everything an object held, the reducer deletes the object and forgets it.
A store error reading the objects fails only a reduce or apply, where a missed pending review would re-propose or re-publish a change still open for review; a map or any other sync warns and continues with what it read.
On the reducer the local file is authoritative between writes, and a missing one is restored from the store with its ids, lineage, and reviews.
A local file older than the store's newest version — a lower highest rule number or fewer reviews, both of which only grow — is a restore from a backup: the sync adopts the store's version, with a one-line note naming both, so a reduce on a restored machine can't roll the shared ledger back.
The reducer's first reduce uploads its local ledger as the first store version, which carries a single-machine ledger into the store unchanged.

## Aging

- Relevant and not violated: the rule stays.
- Violated as it reads now: the rule isn't working as written; propose a rewording, citing the violations.
  A violation of a wording the rule has since replaced doesn't count against the new one.
- No relevance for about 90 days: propose retirement.
  The threshold is config, and retirement is always a proposal; the owner's review level gates every deletion.

## The model that earned a rule

A rule exists because a model needed it.
A rule a newer model follows unprompted reads "relevant, followed" forever, and aging never retires it.

- Each observation record lists the models that answered in its session, and a reduce run's `evidence.json` carries them per session.
- A proposal's review records the models behind the sessions it cited, and when its bullet lands, `rules sync` puts them on the rule as `models`.
  A reword the loop proposed replaces them with the models behind the new wording.
- The reduce input says what each rule was earned on.
- `recall learn age` lists, as `check`, each rule earned only on models no session in the last 30 days ran.
  That is a rule worth a trial without it, never a retirement on that alone: only running the moment that earned it again without the rule can say the current model doesn't need it, which is the replay's ablation (below).

## Replay

A proposed rule exists because one moment went wrong, and that moment can be run again (`learn.replay`, off by default, since each replay is a model call).

- Before an add goes to review, the reduce replays the moment that earned it: the session prefix up to the owner's correction, rendered the way the map renders sessions, run headless with no tools, like the reduce.
- The moment comes from the proposal's own evidence: the newest owner-typed observation whose quoted correction anchors a prompt in a session still on this machine.
  The prefix ends at the prompt the agent was answering when it made the mistake, so the replay re-decides that turn.
- One replay is two calls on the map model: once with the target file as it is, and once with the proposed rule added as a bullet.
  The replay prompt ends in one structured verdict line, `Verdict: repeats` or `Verdict: avoids`, judged against the observed mistake, and the binary compares the two verdicts; a model never decides what is held back.
- An add whose two arms end the same way changed nothing, and is held back with that reason, under "Replay" in the proposals and in the run's `replay.json`, which the apply reads; only a replay where the rule turns `repeats` into `avoids` lets the add through.
- A replay that fails, by error or by output with no verdict line, is logged and recorded and blocks nothing, and a proposal with no replayable moment on this machine isn't replayed.
- One run replays at most 8 moments, a constant, so a weekly run's replay bill stays near the map's.

The same switch runs the ablation on model release, the trial the ledger's models make possible.

- Each rule earned only on models no session in the last 30 days ran is replayed without it: its earning moments, from its source sessions' owner-typed observations, on the current model, with the rule's bullet removed from its file.
- When the agent avoids the mistake in every replay, the binary appends a retire proposal citing the replays, and it rides the same review as any other retire; one `repeats`, a failed replay, or no replayable moment proposes nothing, since ablation only removes on proof.

## Graduation

A rule a script could check or enforce becomes a proposed script, and the prose rule retires when the script lands.
The reduce marks candidates ("this correction is about formatting, time math, or which account is active; a command can check it"); the proposal includes the script sketch.
A rule the agent can't break beats one it has to remember.

A graduate turns an existing reviewed rule into enforcement, so it must name that rule with `- replaces:`, like a reword or retire.
A script in `learn.bin` with a prose pointer is still something the agent must remember to run.
When `learn.settings` names the harness's settings file, a graduate can ask for enforcement the agent can't skip, with `- enforce:`:

- **permission**: a `deny` or `ask` rule for a command pattern, like `ask Bash(git push:*)`.
  The harness refuses the call or asks first; there is no script.
- **hook**: the script goes in `learn.bin`, and a command hook on `PreToolUse`, `PostToolUse`, or `Stop` runs it; the script exits 2 to block a call.
- **script**: the default, as before.

The named rule's own file decides where the enforcement goes: a global rule's hook or permission rule lands in `learn.settings`, and a rule that lives in a repo's context file lands in that repo's own `.claude/settings.json`, which needs no `learn.settings`, so only sessions in that repo carry it.
A repo-scoped graduate must cite only sessions that ran in that repo, by the run's evidence record, the same bar as a new rule aimed at the repo's file; the apply holds back one that doesn't.
A hook or permission rule leaves no prose behind, so the rule stops costing context.
The binary makes the settings edit, in the worktree, after the apply model finishes and only for a proposal the model applied; the model never edits a settings file.
It adds exactly one hook entry or one rule, leaves every other byte of the file as written, and names the script from `$HOME` so one settings file works on every machine.
It never adds an `allow` rule: a loop that learns from sessions can narrow what the agent may do, and can't widen it, even through a reviewed change.
Three more guards hold that line:

- The binary tries each settings edit on the staged file before the apply model runs, and holds back a proposal whose edit it can't make, while nothing has removed the prose rule yet.
- After the model finishes, the binary puts the staged settings file back the way it was and makes only its own edits, so nothing the model wrote there survives.
- A hook script may only block a call, by exiting 2.
  One that names any of the harness's hook output fields — the permission decision, the older top-level decision and reason, or the fields that stop the session, rewrite tool input or output, or feed text back — could approve calls or steer the agent, so the run stops with nothing published.
  The check also catches a field built in an obvious way from fragments: string concatenation, printf of the key, character escapes, or base64.
  That is a tripwire for the obvious case, not a proof about what a script does; human review is the control, and the owner still reads the script in review.

## Separation

Each recall config names its own store and its own model runner.
Data from one environment never reaches another's store or model: the map for a session runs in the environment that holds that session's transcripts, and observations land only in that environment's store.
Shared learning across environments, if ever, is a deliberate export, not a default.
`learn map --archives` reads archives only from this config's store, so it stays inside one environment.

## Other harnesses

An adapter must emit the schema in `docs/events.md`; the map and reduce never see raw transcripts.
What each harness reads as context decides where proposals land:

- Claude Code reads `CLAUDE.md` (with `@` imports) and skills; built and tested.
- Kiro reads steering files under `~/.kiro/steering/`, including `AGENTS.md`, and the files they link; built and tested against Kiro CLI's V2 and v3 transcripts.
- Codex reads `AGENTS.md`; adapter unwritten, format unverified.

A rule in a Kiro steering file rides the same ledger as one in `CLAUDE.md`: with Kiro on, `learn.targets` includes the global `AGENTS.md` steering file, its bullets sync onto the ledger, and one apply can stage edits to both files in one change.
A steering file may open with YAML frontmatter between `---` lines (Kiro's inclusion modes: `always`, `fileMatch` with `fileMatchPattern`, `manual`, `auto`); the sync skips that block, so a pattern list never becomes a rule.
A rule that holds everywhere can be proposed against every harness's targets at once; the targets stay per-harness so each file keeps its native conventions.

## Open questions

- ~~Map model and cost.~~ Measured on real sessions: haiku returned NONE on a session full of corrections; sonnet caught them all at about $0.05 to $0.08 per long session, so sonnet is the map default.
- **Observation schema.** Fixed fields (above) or free-form with a validator; fixed is easier to reduce, free-form loses less.
- ~~Cross-host reduce.~~ Each machine maps its own sessions and caches the observations in the store; the reduce reads every host's observations, so a rule repeated across machines adds up without moving transcripts.
  `learn map --archives` maps archived weeks from one machine, opt-in because it downloads them: other hosts' sessions, and this host's own that are no longer on disk, while a session still on disk keeps reading from disk.
  Map and reduce warn when a host has archives in the last 14 days but no observations.
  Hosts that must stay separate use separate stores.
- ~~Candidate rules.~~ A separate candidate state, kept by the binary: see Candidates above.
- ~~Refusal handling.~~ When the map model refuses a session, the map reads the longest prefix it accepts, counted in owner prompts.
  The search starts below the session's own first refusal, less the prompt that drew it, then binary-searches down.
  The result records how much was read (`partial`) and is cached, so later runs don't pay to be refused again.
  The same content is never retried on another model.
- ~~Ledger merge.~~ One rule ledger per store, written only by the reducer: see Rule ledger above.
  A rule hit still carries the wording the model was shown, so a hit judged against a stale copy never lands on a different rule that shares the id.
  The ledger still records each target by its path on the machine that wrote it.
- ~~Replay.~~ Before proposing a rule, the moment that earned it runs again with and without the rule; on a new model, each outgrown rule's moment runs without it and the current model following it unprompted proposes the retire.
  Both are built, behind `learn.replay`: see Replay above.
- ~~Evidence outside the chat.~~ Review comments and failed checks on agent-authored pull requests are read through an evidence source beside `harness.Harness`: see Outside evidence above.
  Reverts aren't read yet, and `github` is the only source.
