# Events: one schema for every harness

`recall events` turns each harness's transcripts into one normalized JSONL stream, on disk or from inside a week's `.tar.zst` archive.
Everything recall learns from sessions reads this stream, never a harness's raw format, so a new agent needs one adapter and nothing downstream changes.

## The schema

One JSON object per event:

| Field | Meaning |
| --- | --- |
| `harness` | The harness the session belongs to: `claude`, `kiro`, or the name of a `[harness.<name>]` table read through a `format`. |
| `host` | The machine the session ran on; empty when reading a downloaded archive, whose key path names the host. |
| `session` | The session id: the transcript file name without `.jsonl`. |
| `cwd` | The working directory of the turn. |
| `repo` | The origin of the repository the turn ran in, as host/path like `example.com/you/example`, for a source that knows it without a checkout on this machine. Scope uses it as the session's remote. Local adapters leave it empty and resolve the checkout from `cwd`. |
| `timestamp` | RFC 3339, as the harness recorded it. |
| `message_id` | The source's id for the message the event came from; every event from one message carries the same one. At most 128 bytes. |
| `parent_id` | The `message_id` of the message this one follows, empty at the root. At most 128 bytes. |
| `kind` | What happened; the list below. |
| `role` | `user` or `assistant`, when the harness says. |
| `text` | The prose: a prompt, the model's text, a tool result, a refusal category. |
| `tool` | The tool a `tool_call` invoked; on a `tool_result`, the tool that produced it. |
| `input` | On `tool_call`: a one-line summary of the tool's input, such as a shell command or a file path, at most 200 characters, with secret-shaped tokens redacted. On `tool_result`: the input of the call it answers. |
| `ok` | On `tool_result`: false when the harness marked it an error. |
| `skill` | The skill a `skill_invoked` or `slash_command` names, or the command a `builtin_command` names, without the slash. |
| `skill_dir` | On `slash_command` and `skill_invoked`: the directory the harness loaded the skill from, when the transcript records it. |
| `model` | The model behind an assistant event. |
| `sidechain` | True on subagent turns: the orchestrating agent wrote the prompt, not a person. |

Empty fields are omitted.

For example, a typed prompt and the tool call that followed it:

```json
{"harness":"claude","host":"laptop-a","session":"5f0c2a9e-1b7d-4c3a-9e2f-8a6b4d1c0e7f","cwd":"/home/you/src/example","timestamp":"2026-03-02T12:00:00Z","kind":"user_prompt","role":"user","text":"run the tests and fix what fails"}
{"harness":"claude","host":"laptop-a","session":"5f0c2a9e-1b7d-4c3a-9e2f-8a6b4d1c0e7f","cwd":"/home/you/src/example","timestamp":"2026-03-02T12:00:04Z","kind":"tool_call","role":"assistant","tool":"Bash","input":"go test ./...","model":"claude-sonnet-4-5"}
```

## Kinds

The transcript details in this section and the next are Claude Code's.
The Kiro section below says how its transcripts map onto the same kinds.

- `user_prompt`: text a person typed into the harness's interface, the interactive CLI or a product's chat window. For Claude Code, what counts as typed: a user turn with text, on the `cli` entrypoint, not meta, not a compaction summary, not wrapped in `<...>` tags. A `sdk-cli` session (a scripted `claude -p` run) emits no user prompts.
- `assistant_text`: prose the model wrote.
- `tool_call`: the model invoking a tool; `tool` names it.
- `tool_result`: the tool's answer; `ok = false` when the result carried `is_error`.
- `skill_invoked`: a `Skill` tool call; `skill` carries the skill name.
- `slash_command`: a user turn wrapped in `<command-name>`; `skill` carries the command name and `text` its arguments.
- `builtin_command`: a `<command-name>` turn the harness ran itself, like `/clear`; never a skill. The harness answers a built-in with local command output (`<local-command-stdout>` or a `local_command` system line), and expands a skill into a meta turn starting `Base directory for this skill:`, which sets `skill_dir`. A command followed by neither stays a `slash_command`.
- `refusal`: the model or API declined; `text` carries the category when known.
- `compact`: a compaction summary standing in for earlier turns.

## Branches

A source whose messages form a tree, such as a chat product where the owner edits a prompt, sets `message_id` and `parent_id` on every event.
A flat source leaves both empty, and so do the local adapters today.

- A message's events come after its parent's, and siblings come in the order they were created.
- A `user_prompt` beside an earlier user sibling is an edit of it, and an assistant message beside an earlier assistant sibling is a regenerated answer. The binary derives both; no event claims an edit.
- The map's render walks the tree depth first: the shared prefix once, the older branch marked abandoned, then the newer branch marked as replacing it. A session with no fork renders as a flat one does.

## Decisions

- Metadata only: kind decisions never read message prose. They read harness-written wrappers and markers, such as `<command-name>`, `<local-command-stdout>`, and the skill expansion's `Base directory` line, never what a person or model wrote.
- The stream is faithful: gated or refused sessions still emit their events, and filtering is the consumer's job.
- Meta turns (`isMeta`) and `<...>`-wrapped wrappers other than `<command-name>` emit nothing; they're harness bookkeeping.
- A torn last line (an active session mid-write) is skipped, not an error.
- Tool names and inputs on results come from correlating `tool_use_id` with the earlier call in the same session.
- A tool's input is summarized by the one field that says what the call did (`command`, `file_path`, `pattern`, `url`, `query`, `description`); a tool with no such field keeps its input as compact JSON. The summary passes through the same token-shape redaction the archive offers, since a command can carry a credential.

## The schema is a contract

The schema is `recall.events/v1`, and its Go types live in `api/events/v1`, outside `internal/`, so an import plugin builds on the same types recall reads.

- It grows only by optional fields and new kinds, never a rename or a removal.
- A reader drops an event of a kind it doesn't know and counts it, so a newer writer works with an older recall.
- `Check` is the per-event validation recall applies to events it didn't write: a known kind, an RFC 3339 `timestamp`, a `role` that is empty, `user`, or `assistant`, `ok` only on `tool_result`, and bounded message ids.
- `CheckSession` adds the session's rules: one session id, at least one dated event, and every `parent_id` naming an earlier message.
- `Summarize` is the one-line input summary above, before redaction and the 200-character cut, so every adapter and plugin summarizes a tool's input the same way.

## Adapters

An adapter implements `harness.Harness`: name the archive prefix, list sessions, say which files a week's archive includes, and read one session stream into events.
`claude` reads `~/.claude/projects/*/*.jsonl`. Codex and others plug in beside them later.
A harness with `format = "events"` reads files of these events as they are, one session per `<id>.jsonl` in its source; recall writes `harness`, `host`, and `session` itself, and drops a line that fails `Check`.
Each must emit the schema above; `docs/learning.md` says where each harness's proposals land.

### Kiro

`kiro` reads both layouts Kiro CLI writes under `~/.kiro/sessions`:

- v2 engine: `cli/<id>.jsonl`, lines of `{"kind", "data"}`, with the working directory in `cli/<id>.json`. Only a `Prompt` carries a time (epoch seconds), so the turns after it take the prompt's time.
- v3 engine: `<hash>/sess_<id>/messages.jsonl`, lines of `{"timestamp", "payload"}`, with the working directory in `session.json`; the session id is the `sess_<id>` directory.

The mapping:

- A user turn is a `user_prompt` unless Kiro or an add-on wrote it: a synthetic turn, a `<...>` wrapper, or a bracketed tag like `[AGENT SYSTEM PROMPT]` or `[RUNTIME]`. A `[LIVE STEERING - New message from user]` turn is a message typed mid-turn, so it's a prompt with the header removed.
- Assistant prose (`Say` in v3, `text` in v2) is `assistant_text`; reasoning emits nothing but names the model.
- A tool call is `tool_call`, once per call id, though v3 writes a call again as its status moves; a v3 `load_resource` of `skill::<name>` is `skill_invoked`. Kiro transcripts don't record a one-line input summary, so `input` stays empty on its events.
- A tool result is `tool_result`, failed when its status isn't `success`.
- A turn that ends with stop reason `refusal` is `refusal`.

Kiro offers its skills and agents as slash commands, which its transcripts don't record as such, so `slash_command` and `builtin_command` never come from Kiro.
