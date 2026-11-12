---
name: recall-setup
description: First-time recall setup on a machine, start to finish: the binary, the config, the store, the weekly archive job, and the learning loop. Use when I say "set up recall", "install recall", or ask why recall isn't working on this machine. Safe to re-run; every step checks before it changes anything.
---

Walk these in order, ask before anything that writes, and skip steps that already check out.

1. **Binary.** Run `recall version`.
   If it's missing, offer, in order: Homebrew when `brew` is installed (`brew tap micahhausler/recall https://github.com/micahhausler/recall`, then `brew install --cask recall`); `go install github.com/micahhausler/recall/cmd/recall@latest` when Go is installed; otherwise the release archive for this OS and architecture from the repo's releases page, checked against `checksums.txt`, with `recall` put on PATH.
   Verify with `recall version` before going on.
2. **Config.** `recall config show` prints the merged config and where each value came from; with nothing configured yet, defaults apply.
   Shared settings belong in a dotfiles-managed drop-in under `~/.config/recall/config.d/`; machine-only values go through `recall config set`.
3. **Store.** Ask which store:
   - `fs` (recommended without AWS): `recall config set store.kind fs` and `store.path` to a folder; a cloud-synced folder gives offsite backup with no AWS, at the cost of transcripts living in that account.
   - `s3`: follow the archive-chats skill's setup, which handles the bucket, policies, and verification.
4. **Archive.** Back up everything on disk now, since Claude Code deletes old transcripts (`cleanupPeriodDays`, 30 days by default).
   Count the weeks back to the oldest transcript under `~/.claude/projects`, then `recall archive-chats run --lookback <weeks> --dry-run` to preview and, once I say yes, the same without `--dry-run`.
   Then schedule the weekly upload through the recur skill (`recur.jobs.archive-chats`, Monday noon).
   `recall archive-chats info` should list this host's uploads.
5. **Learning.** Confirm `learn.targets` points at my context files (`recall config get learn.targets`), then `recall learn rules sync` to put their bullets on the ledger.
   Offer a first map over every session on disk, which needs no download: `recall learn map --since all --dry-run` counts the sessions that would call the model; at about $0.03 each with the default sonnet map model, say the estimate and ask.
   After it runs, show the cost it printed, then offer `recall learn reduce` for a first set of proposals, and say where they landed.
   Each machine maps its own sessions; the reduce reads every machine's observations from the store, and warns about a machine whose archives have none.
   `recall learn map --archives` maps other machines' archived weeks from here instead, at the cost of downloading them; offer it only when a machine can't run the map itself.
   Offer to schedule `recall learn map` weekly through the recur skill; the reduce is better run by hand until its proposals have earned trust.
   To apply a reduce's proposals, confirm `learn.targets`, `learn.skills`, and `learn.bin` each sit in a git checkout, then offer `recall learn apply --dry-run`, and say it opens one pull request per changed repo (`learn.review = "branch"` keeps them local).
   Once the proposals have earned trust, one machine can schedule `recall learn run` (rules sync, reduce, apply) after the others' weekly map.
6. **Check.** Finish with `recall recur check`, `recall skills --since 90d`, and `recall lint`; show me anything they flag.

On a machine that shares config through dotfiles, steps 2 and 3 are usually already done; start at 4.
