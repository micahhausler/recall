# import-example

An example recall import plugin (`docs/import.md`) that maps a synthetic source file into recall's normalized events.
It is the end-to-end fixture for `recall import`, and a starting point for a plugin of your own.

It is built on `api/imports/v1/command`.
`command.Run` reads the request from `RECALL_IMPORT_INFO`, checks every event and limit as the plugin writes it, and prints the answer.
The one function the example writes itself is `fetch`, which says where sessions come from.

## What it maps

`testdata/source.json` holds two common source shapes:

- An agent platform's flat turns: a prompt, responses, and tool messages that each hold one call with its input and output. A tool message becomes a `tool_call` at its start and a `tool_result` at its end, named by its structured name or its kind, never its display title. A desktop workspace's sessions are left out and counted as filtered, since their transcripts are already local.
- A chat product's conversations, whose messages form a tree. Every branch is kept, with `message_id` and `parent_id` on each event, so recall sees an edited prompt as an edit. Injected context, reasoning, and banners emit nothing, and a comment sent from an artifact keeps only the comment.

`testdata/expected/` holds the events each session maps to.

## Run it

Build the plugin and name it by path in the importing machine's config:

```sh
go build -o ~/bin/import-example ./examples/import-example
```

```toml
[harness.example]
format = "events"

[harness.example.import]
command = ["~/bin/import-example", "--source", "/path/to/source.json"]
```

Then `recall import example` runs it, and `recall events --harness example` prints what it stored.
