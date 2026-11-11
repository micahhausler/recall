---
name: recall-setup
description: First-time recall setup on a machine, start to finish: the binary, the config, the store, the weekly archive job, and the learning loop. Use when I say "set up recall", "install recall", or ask why recall isn't working on this machine. Safe to re-run; every step checks before it changes anything.
---

Walk these in order, ask before anything that writes, and skip steps that already check out.

1. **Binary.** Run `recall version`.
   If it's missing, offer: `go install github.com/micahhausler/recall/cmd/recall@latest` when Go is installed, otherwise download the release binary for this OS and architecture from the repo's releases page and put it on PATH.
   Verify with `recall version` before going on.
2. **Config.** `recall config show` prints the merged config and where each value came from; with nothing configured yet, defaults apply.
   Shared settings belong in a dotfiles-managed drop-in under `~/.config/recall/config.d/`; machine-only values go through `recall config set`.
3. **Store.** Ask which store:
   - `fs` (recommended without AWS): `recall config set store.kind fs` and `store.path` to a folder; a cloud-synced folder gives offsite backup with no AWS, at the cost of transcripts living in that account.
   - `s3`: follow the archive-chats skill's setup, which handles the bucket, policies, and verification.
4. **Archive.** `recall archive-chats run --dry-run` to preview, then schedule the weekly upload through the recur skill (`recur.jobs.archive-chats`, Monday noon).
   `recall archive-chats info` should list this host's uploads after the first run.
5. **Learning.** Confirm `learn.targets` points at my context files (`recall config get learn.targets`), then `recall learn rules sync` to put their bullets on the ledger.
   Offer a first `recall learn map --since 7d --limit 5` (it calls a model; say the cost it prints), then `recall learn reduce --dry-run` to show what a weekly reduce would see.
   Offer to schedule `recall learn map` weekly through the recur skill; the reduce is better run by hand until its proposals have earned trust.
6. **Check.** Finish with `recall recur check`, `recall skills --since 90d`, and `recall lint`; show me anything they flag.

On a machine that shares config through dotfiles, steps 2 and 3 are usually already done; start at 4.
