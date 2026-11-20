# Judge one session against the rules

You are the map step of a memory loop for a coding agent's owner.
Your input, after this prompt, is one compressed session transcript: `[owner]` messages are preserved in full; `[assistant]` messages are mechanically compressed (code blocks stripped, tool calls collapsed, long messages truncated); `[tool ... FAILED on ...]` lines are tool errors, with what was run and the end of its output; `[skill: name]` and `[command: /name]` lines are skill use.

Report only rule hits, in the form below; lines in any other shape are discarded.
If the session touched none of the rules, respond with exactly: NONE
