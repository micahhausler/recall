# Harnesses recall has no adapter for

A `[harness.<name>]` table recall doesn't know by name is a harness of its own, read through a `format` recall does know.
recall archives and maps its sessions under its own name, and gives it no `learn.targets` entry and no runner.

## A harness run from another home reads a known harness's format

A harness CLI run with its home somewhere else, such as Claude Code under `CLAUDE_CONFIG_DIR` or an app that runs a harness CLI with its own config directory, sets `format` to the known harness whose transcripts it writes:

```toml
[harness.claude-work]
format = "claude"
source = "~/.claude-work/projects"
```

## The events format reads recall's own normalized events

`format = "events"` reads files of recall's normalized events ([events.md](events.md)), one session per `<id>.jsonl` directly in `source`:

```toml
[harness.chat]
format = "events"
```

- Its `source` defaults to `~/.local/state/recall/import/<name>/sessions`.
- Its sessions ran on another machine, so the map never looks their paths up here, and it has no context files, skills, or native memory.
- A harness name is a store prefix, so `analysis`, `hits`, `reviews`, `ledger`, `learn`, and any name starting `evidence-` are refused.

## An import plugin fetches sessions from systems recall can't read in place

An import plugin is a program that fetches chat history from somewhere recall has no adapter for, such as an internal agent platform or a product's data export, and writes it as recall's normalized events.
recall runs the plugin, validates and redacts what it wrote, and stores each session under the events harness's `source`, where it archives and maps like any other harness's.
[import.md](import.md) is the contract a plugin follows, and [`examples/import-example`](../examples/import-example) is a plugin built on it.

```toml
[harness.chat]
format = "events"

[harness.chat.import]
command = ["chat-history", "--workspace", "all"]
timeout = "10m"
```

- `import.command` is required: the program, then its arguments, run without a shell. A bare name is looked up on `PATH`, a path runs that file, and a leading `~` expands.
- `import.timeout` bounds one run, 10 minutes by default.
- `recall import chat` runs one import, and `recall import` runs every enabled one. `--full` asks for everything, `--replace` accepts a stored session's shorter copy, and `--dry-run` reports what would change and stores nothing.
- A failed plugin run stores nothing, and the next run asks from the same point. A session file that fails a check is rejected on its own.
- Imported text and tool input are always redacted, whatever `archive-chats.redact` says.

## An import runs on one machine

- `import.command` belongs in that machine's config drop-in, and the `format` line in config every machine reads, since the reducer reads only the harnesses its own config declares.
- A plugin that fetches from a service runs from a recur job, whose `PATH` is `recur.env`'s, so a bare-named plugin must be on it.
- A plugin that reads a file the owner downloads runs by hand after each download.
- Two machines importing one source count each session twice, so `recall import` warns when another host has archived the same harness recently.
