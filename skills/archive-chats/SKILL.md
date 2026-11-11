---
name: archive-chats
description: Set up, check, or run recall archive-chats, which uploads coding-agent transcripts to S3 once a week, one zstd tarball per machine per week, for later analysis. Use when I say "set up chat archive", "archive my transcripts", "back up my chats", or ask whether this machine's uploads are working or what they cost. With "setup", walk through first-time setup.
---

`recall archive-chats` does the work; `recall archive-chats -h` and `config.example.toml` document every command and key.
Objects land at `s3://<bucket>/<harness>/<host>/<year>/W<week>.tar.zst`.
Granting AWS permissions is out of scope: show me the policies, and I attach them.

**Setup**, run with `setup` or the first time on a machine. Ask each question with a recommended option, and wait for answers before running anything that writes.

1. **Store.** `recall config set store.kind s3` for S3 (recommended), or `fs` with `store.path` for a local directory, which needs none of the AWS steps.
2. **AWS.** Offer the profiles from `aws configure list-profiles`, then `recall config set store.s3.profile <p>` and `store.s3.region <r>`.
3. **Bucket.** A new bucket in my account regional namespace (recommended; `store.s3.bucket_prefix`, default `chat-archive`), or an existing bucket's name in `store.s3.bucket`.
4. **Retention.** How long S3 keeps each week: 1 year, 3 years, or 10 years (recommended), as `store.s3.retention_days`.
5. **Check.** `recall archive-chats check --setup`. For each FAIL, quote the fix it prints. Show `recall archive-chats policy setup` and `policy upload`, and say which one each machine needs.
6. **Estimate.** Show `recall archive-chats estimate` as is: local size, compressed size, and the monthly S3 cost over the retention period. If it warns about `cleanupPeriodDays`, offer the edit to my Claude Code settings; don't make it unasked.
7. **Create.** Show `recall archive-chats setup --dry-run`, ask, then run `recall archive-chats setup`. It's done only when every `verify` line says ok.
8. **Upload access.** `recall archive-chats check` proves list and write access; it writes one empty `<harness>/<host>/.write-check` object. Ask first.
9. **Schedule.** Use the recur skill to add a job: `recall config set recur.jobs.archive-chats.schedule '0 12 * * 1'` and `recur.jobs.archive-chats.command 'recall archive-chats run'`, then `recall recur check` and `recall recur sync`. Offer a first upload now with `recall recur run archive-chats`, then show `recall archive-chats info`.

On another machine with the same shared config, skip 1 to 4 and 7.

**Status.** `recall archive-chats info` lists this host's uploads; `~/.local/state/recall/recur/archive-chats.log` has the run history. A missed week is caught up by the next run, within `archive-chats.lookback` weeks.

Never create or change IAM users, roles, or policies; never turn off the bucket's public access block, TLS-only policy, or encryption; never delete objects.
