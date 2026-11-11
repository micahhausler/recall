# Events: one schema for every harness

`recall events` turns each harness's transcripts into one normalized JSONL stream, on disk or from inside a week's `.tar.zst` archive.
Everything recall learns from sessions reads this stream, never a harness's raw format, so a new agent needs one adapter and nothing downstream changes.

## The schema

One JSON object per event:

| Field | Meaning |
|---|---|
| `harness` | The adapter that emitted it: `claude` today. |
| `host` | The machine the session ran on; empty when reading a downloaded archive, whose key path names the host. |
| `session` | The session id: the transcript file name without `.jsonl`. |
| `cwd` | The working directory of the turn. |
| `timestamp` | RFC 3339, as the harness recorded it. |
| `kind` | What happened; the list below. |
| `role` | `user` or `assistant`, when the harness says. |
| `text` | The prose: a prompt, the model's text, a tool result, a refusal category. |
| `tool` | The tool a `tool_call` invoked; on a `tool_result`, the tool that produced it. |
| `ok` | On `tool_result`: false when the harness marked it an error. |
| `skill` | The skill a `skill_invoked` or `slash_command` names, or the command a `builtin_command` names, without the slash. |
| `skill_dir` | On `slash_command` and `skill_invoked`: the directory the harness loaded the skill from, when the transcript records it. |
| `model` | The model behind an assistant event. |
| `sidechain` | True on subagent turns: the orchestrating agent wrote the prompt, not a person. |

Empty fields are omitted.

## Kinds

- `user_prompt`: a prompt typed into the interactive CLI.
  What counts as typed follows the old distill extractor: a user turn with text, on the `cli` entrypoint, not meta, not a compaction summary, not wrapped in `<...>` tags.
  A `sdk-cli` session (a scripted `claude -p` run) emits no user prompts.
- `assistant_text`: prose the model wrote.
- `tool_call`: the model invoking a tool; `tool` names it.
- `tool_result`: the tool's answer; `ok = false` when the result carried `is_error`.
- `skill_invoked`: a `Skill` tool call; `skill` carries the skill name.
- `slash_command`: a user turn wrapped in `<command-name>`; `skill` carries the command name and `text` its arguments.
- `builtin_command`: a `<command-name>` turn the harness ran itself, like `/clear`; never a skill.
  The harness answers a built-in with local command output (`<local-command-stdout>` or a `local_command` system line), and expands a skill into a meta turn starting `Base directory for this skill:`, which sets `skill_dir`.
  A command followed by neither stays a `slash_command`.
- `refusal`: the model or API declined; `text` carries the category when known.
- `compact`: a compaction summary standing in for earlier turns.

## Decisions

- Metadata only: kind decisions never read message prose, matching the old extractor.
  They read harness-written wrappers and markers, such as `<command-name>`, `<local-command-stdout>`, and the skill expansion's `Base directory` line, never what a person or model wrote.
- The stream is faithful: gated or refused sessions still emit their events, and filtering is the consumer's job (distill's drop-gated-chats rule was policy, not schema).
- Meta turns (`isMeta`) and `<...>`-wrapped wrappers other than `<command-name>` emit nothing; they're harness bookkeeping.
- A torn last line (an active session mid-write) is skipped, not an error.
- Tool names on results come from correlating `tool_use_id` with the earlier call in the same session.

## Adapters

An adapter implements `harness.Harness`: name the archive prefix, list sessions, say which files a week's archive includes, and read one session stream into events.
`claude` reads `~/.claude/projects/*/*.jsonl`.
Codex, Kiro, and others plug in beside it later; `docs/learning.md` says what they must emit.
