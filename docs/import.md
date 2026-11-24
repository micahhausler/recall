# Import plugins: the contract

An import plugin is an executable that fetches chat history from a system recall has no adapter for, and writes it as recall's normalized events.
recall runs the plugin, validates what it wrote, and stores each session as a file in an events harness's source, where it archives and maps like any other harness's.
The plugin owns everything about its source system, including its login and its API; recall owns the contract, the validation, and everything after the files land.
[harnesses.md](harnesses.md) covers the config, and [`examples/import-example`](../examples/import-example) is a working plugin.

## recall runs the plugin with the request in its environment

| What | Value |
| --- | --- |
| `RECALL_IMPORT_INFO` | The request, one JSON object. |
| `RECALL_HOOK` | `1`, so a harness CLI the plugin starts doesn't run recall's hooks. |
| Program and arguments | `import.command`, run without a shell; recall adds no arguments. |
| Working directory | The output directory. |
| stdin | Empty. |
| stdout | The answer, one JSON object. |
| stderr | Diagnostics, streamed to recall's stderr with the harness name before each line; the last 64 KiB go in the error recall reports. |
| Timeout | `import.timeout`; at the deadline recall kills the plugin's process group. |

```json
{
  "apiVersion": "recall.import/v1",
  "kind": "Import",
  "spec": {
    "name": "chat",
    "outputDir": "/home/you/.local/state/recall/import/chat/run-7f3c1a",
    "since": "2026-03-01T05:00:00Z",
    "limits": {"sessions": 10000, "sessionBytes": 67108864, "lineBytes": 4194304, "totalBytes": 1073741824}
  }
}
```

- `spec.name` is the harness name, so one plugin can serve two harnesses.
- `spec.outputDir` is an empty directory, mode 0700, for this run only.
- `spec.since` is one hour before the start of the last run that finished complete, and empty on the first run and with `--full`. The plugin returns every session that changed at or after it; one that can't tell returns everything, and recall stores only what changed.
- `spec.limits` are what recall enforces, so a plugin can stop early rather than be rejected.

## The plugin writes one file per session and answers on stdout

Every regular `*.jsonl` file directly in `outputDir` is one session, one event per line in the schema of [events.md](events.md).
recall names the stored file by the session id inside it, not by the file's name.

```json
{"apiVersion": "recall.import/v1", "kind": "Import", "status": {"sessions": 12, "filtered": 3, "partial": false}}
```

- `status.sessions` is required: how many session files the plugin wrote. recall warns when it finds a different number.
- `status.filtered` is how many sessions the plugin left out on purpose, such as ones a local harness already records.
- `status.partial` is true when the plugin couldn't fetch everything; recall stores what arrived and asks from the same `since` next run.
- Both sides ignore fields they don't know.

## The exit status decides whether anything is stored

| Outcome | What recall does |
| --- | --- |
| Exit 0 with a valid answer | Validates and stores each session; `since` moves to this run's start unless `partial` is set. |
| Exit 0 with no valid answer | Stores nothing. |
| A non-zero exit, a signal, or the timeout | Stores nothing, and reports the tail of stderr. |

A failed run never touches what an earlier run stored, and `recall import` exits non-zero when any import failed.

## recall checks every file before it stores one

A failed check rejects that one session file, and the rest of the run still stores.

- Only regular `*.jsonl` files count; symlinks, FIFOs, devices, and subdirectories are skipped with a warning.
- A session id matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` with no `..`, and every event in a file carries the same one. Two files with one id reject both.
- Every event passes `Check`: a known `kind`, an RFC 3339 `timestamp`, a `role` that is empty, `user`, or `assistant`, and `ok` only on `tool_result`. An event of an unknown kind is dropped and counted, so a newer plugin works with an older recall.
- At least one event is dated, and every `parent_id` names a message earlier in the file.
- A line over `lineBytes` or a file over `sessionBytes` rejects the session; more than `sessions` files or `totalBytes` in all fails the run.
- recall writes `harness` and `host` itself and drops fields it doesn't know.
- `text` and `input` always pass through token-shape redaction, whatever `archive-chats.redact` says, and `input` is cut to 200 characters.
- recall parses JSON and nothing else: it runs no file and opens no path an event names.

## Stored sessions only change, and only grow

- Sessions live in `~/.local/state/recall/import/<name>/sessions/<id>.jsonl`, beside the import's `state.json` and its lock.
- A session identical to the stored copy isn't written, so its modification time stays and nothing downstream sees a change.
- A changed session replaces the stored one in one rename.
- A copy with fewer events than the stored one is held and reported until `recall import --replace`: a source with branches emits its whole tree, so a session only grows.
- Nothing is deleted automatically, including a session the plugin stops returning.
- `host` is the importing machine; `recall import` warns when another host archived the same harness recently, since two machines importing one source count each session twice.

## A plugin maps its source by what the source wrote, not by what was said

- `user_prompt` is text a person typed into the product. A turn the product composed emits nothing, and a product-written frame around typed text is removed.
- Hidden blocks such as reasoning, injected context, and banners emit nothing.
- A tool call is a `tool_call` and a `tool_result` sharing `tool` and `input`, with `ok = false` when the source marks it failed.
- `tool` is a structured tool name, or the call's kind when the source offers only a display title.
- A source whose messages form a tree emits every branch, parents before children, with `message_id` and `parent_id` on each event; recall finds edits and regenerations itself.
- `model` is set when the source names one, as the source spells it, and `cwd` and `repo` only when the source records them. Nothing is guessed.
- A plugin leaves out every session a local harness already records, and counts it in `status.filtered`; recall doesn't dedupe.

## The Go packages are the contract

| Package | What it holds |
| --- | --- |
| `api/events/v1` | `Event`, the `Kind` constants, `Check`, `CheckSession`, and `Summarize`. |
| `api/imports/v1` | `Import`, `Spec`, `Limits`, `Status`, `InfoEnv`, and `ValidSessionID`. |
| `api/imports/v1/command` | The plugin's side: `ParseRequest`, and `Run`, which hands the plugin an `Output` and prints the answer. |

They import only the standard library.
`recall.import/v1` and `recall.events/v1` grow only by optional fields and new kinds; a breaking change is a new version.

```go
func main() {
	err := command.Run(context.Background(), os.Getenv(importsv1.InfoEnv), os.Stdout,
		func(ctx context.Context, req command.Request, out *command.Output) error {
			for _, conv := range fetch(ctx, req.Since) {
				s, err := out.Session(conv.ID)
				if err != nil {
					return err
				}
				for _, e := range toEvents(conv) {
					if err := s.Add(e); err != nil {
						return err
					}
				}
				if err := s.Close(); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- `out.Session` refuses an id recall would refuse, and `s.Add` checks each event and the limits, so a plugin fails at the event that's wrong.
- `s.Close` discards a session with no events, no dated event, or a refused event, and says why.
- `out.Filtered(n)` and `out.Partial()` set the answer's optional fields.
- `Run` prints the answer only when the function returns nil.

A plugin in any other language reads `RECALL_IMPORT_INFO`, writes JSONL files into `outputDir`, and prints the answer.

## The plugin is trusted code

- The plugin holds its credentials; recall passes it no token and reads back only files and the answer.
- It runs as the owner, with the owner's environment, and decides which turns count as typed by the owner, so a plugin belongs in config only when its author is trusted with both.
- recall runs only the command `import.command` names; no program name is derived from a harness name.
