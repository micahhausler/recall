---
name: recur
description: Declare, change, remove, sync, or debug recurring jobs, which recall recur installs into cron on Linux or launchd on macOS from [recur.jobs] tables in the recall config. Use when I say "add a cron job", "schedule this", "run this every week", or ask whether a scheduled job is installed or why it didn't run. With "setup" ("set up recurring jobs"), walk through first-time setup.
---

`recall recur` makes this machine's crontab block or LaunchAgents match the `[recur.jobs.<name>]` tables in the recall config, every `config.d/*.toml` drop-in included.
`recall recur -h` and `config.example.toml` document every key.

**Setup**, run with `setup` or the first time on a machine:

- Set `recur.env` to a `PATH` that includes recall's own directory, since neither scheduler has my shell's: `recall config set recur.env '["PATH=$HOME/go/bin:/usr/local/bin:/usr/bin:/bin"]'`.
- On Linux, ask for my timezone and `recall config set recur.cron_tz <Area/City>`, so schedules don't depend on the server's.
- Run `recall recur check`, then `recall recur sync`; with no jobs declared yet, both just say so.

**Changing jobs:**

1. **Where.** Shared jobs go in a config file my dotfiles share; jobs for one machine go in a drop-in only it has. `recall recur list` shows each job and the file it comes from, and `recall config set recur.jobs.<name>.schedule '0 12 * * 1'` plus `...command 'recall archive-chats run'` writes to the tool-owned drop-in.
2. **Disable, don't delete.** `recall config set recur.jobs.<name>.enabled false` turns off a shared job on one machine. For a laptop that sleeps, pick a time it's usually awake; launchd runs a missed job on wake, cron doesn't.
3. **Check, then sync.** `recall recur check` shows each job as installed and the changes a sync would make. Show me that, then run `recall recur sync` and `recall recur check` again; it should end with `up to date`.
4. **Try it.** `recall recur run <name>` runs a job exactly as the scheduler would. Its output is in `~/.local/state/recall/recur/<name>.log`; launchd's own errors are in `<name>.launchd.log` next to it.

Never edit the generated crontab block or the `<prefix>.*.plist` files by hand; change the config and sync.
Ask before removing a job from a file other machines share.
