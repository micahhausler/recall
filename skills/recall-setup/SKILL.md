---
name: recall-setup
description: "First-time recall setup on a machine, start to finish: the binary, the config, the store, the weekly archive job, and the learning loop. Use when I say \"set up recall\", \"install recall\", or ask why recall isn't working on this machine. Safe to re-run; every step checks before it changes anything."
---

Walk these in order, ask before anything that writes, and skip steps that already check out.

The plugin's session hook is the default way to run the loop, and `recur` jobs are the opt-in for a fixed schedule or a machine whose sessions never end cleanly.
For a machine that needs none of the choices below, offer the short path first: `recall init --dry-run` shows what it would set (a local store, review on a local branch, and the session hook running the loop, with no bucket, scheduler, or git remote), and `recall init` sets it.
Init turns the weekly reduce (`hooks.learn_every`) on only for the first machine on a store; a later machine sees the first's claim under `learn/reducer` in the store and leaves the reduce to it.
Pin the `host` key in a drop-in under `~/.config/recall/config.d/` on every machine before its first init: the reducer claim and every archive key use that name, and macOS hostnames drift after a name clash on the network.
If the claim names a machine that is retired, reimaged, or renamed, `recall learn reducer --take` moves it to this one.
If I take the short path, skip to step 5's first map, and review with `recall learn reviews` and a merge of each `learn-<date>` branch.

1. **Binary.** Run `recall version`.
   If it's missing, offer, in order: Homebrew when `brew` is installed (`brew tap micahhausler/recall https://github.com/micahhausler/recall`, then `brew install --cask recall`); `go install github.com/micahhausler/recall/cmd/recall@latest` when Go is installed; otherwise the release archive for this OS and architecture from the repo's releases page, checked against `checksums.txt`, with `recall` put on PATH.
   Verify with `recall version` before going on.
2. **Config.** `recall config show` prints the merged config and where each value came from; with nothing configured yet, defaults apply.
   If `~/.kiro/sessions` exists, offer to turn on the Kiro harness (`recall config set harness.kiro.enabled true`), and ask whether the learning loop should run through Kiro (`recall config set learn.runner kiro`); say that `learn apply` then runs in text mode: Kiro can't confine write tools to the worktrees, so it gets none, and recall writes the files the model returns, only at the paths it listed.
   If this machine has no Claude Code, offer `recall config set harness.claude.enabled false`: the runner, the ledger, and `learn.skills` then default to Kiro's, and `recall init` declares recur jobs for the loop instead of the session hook, since Kiro's session-end hook kills the worker it starts; `recall recur sync` installs them.
   With the Kiro harness on, `recall init` installs the `keep` and `recall-setup` skills into Kiro as a power, from files built into the binary; rerunning it after an upgrade updates them.
   Shared settings belong in a dotfiles-managed drop-in under `~/.config/recall/config.d/`; machine-only values go through `recall config set`.
3. **Store.** Ask which store:
   - `fs` (recommended without AWS): `recall config set store.kind fs` and `store.path` to a folder; a cloud-synced folder gives offsite backup with no AWS, at the cost of transcripts living in that account.
   - `s3`: follow the archive-chats skill's setup, which handles the bucket, policies, and verification.
4. **Archive.** Back up everything on disk now, since Claude Code deletes old transcripts (`cleanupPeriodDays`, 30 days by default).
   Count the weeks back to the oldest transcript under `~/.claude/projects` (and `~/.kiro/sessions` with Kiro on), then `recall archive-chats run --lookback <weeks> --dry-run` to preview and, once I say yes, the same without `--dry-run`.
   Then keep the weekly upload running: the session hook does it once `recall init` sets `hooks.archive`, or a fixed schedule through the recur skill (`recur.jobs.archive-chats`, Monday noon) is the alternative.
   Never both: the hook skips a step an enabled recur job's command already runs, so a job wins where one exists.
   `recall archive-chats info` should list this host's uploads.
5. **Learning.** Confirm `learn.targets` points at my context files (`recall config get learn.targets`), then `recall learn rules sync` to put their bullets on the ledger.
   Offer a first map over every session on disk, which needs no download: `recall learn map --since all --dry-run` counts the sessions that would call the model; at about $0.03 each with the default sonnet map model (through Kiro, about 0.1 credits each), say the estimate and ask.
   After it runs, show the cost it printed, then offer `recall learn reduce` for a first set of proposals, and say where they landed.
   Each machine maps its own sessions; the reduce reads every machine's observations from the store, and warns about a machine whose archives have none.
   `recall learn map --archives` also maps archived weeks from here, other machines' and this one's weeks whose transcripts are gone from disk, at the cost of downloading them; offer it when a machine can't run the map itself or history needs recomputing.
   The session hook runs the map after each session once `recall init` sets `hooks.map`; a weekly `recall learn map` job through the recur skill is the alternative, and the reduce is better run by hand until its proposals have earned trust.
   Ask which repos may get their own rules: `recall config set learn.repos '["~/src/project"]'` lets a lesson seen only in that checkout land in its own context file, by a pull request on its origin, and `recall learn targets` lists every file a proposal may touch.
   Leave out any repo whose pull requests other people read unless I say to include it.
   To apply a reduce's proposals, confirm `learn.targets`, `learn.skills`, and `learn.bin` each sit in a git checkout, then offer `recall learn apply --dry-run`, and say it opens one pull request per changed repo (`learn.review = "branch"` keeps them local).
   Once the proposals have earned trust, one machine can schedule `recall learn run` (rules sync, reduce, apply) after the others' weekly map.
6. **Check.** Finish with `recall recur check`, `recall skills --since 90d`, and `recall lint`; show me anything they flag.

On a machine that shares config through dotfiles, steps 2 and 3 are usually already done; start at 4.
