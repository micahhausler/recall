---
name: keep
description: At the end of a session, propose 0 to 3 lessons worth keeping as edits to my context files, and apply the ones I approve. Use when I say "wrap up", "keep this", or "what should we keep". The manual, immediate form of the recall learning loop, for when I don't want to wait for a scheduled run.
---

A later review of my transcripts only sees what I typed.
You also saw what went wrong and what fixed it, so catch the lesson now while I still care.

1. **Look back.** Scan this session for corrections I made, mistakes you made and then fixed, and context I had to repeat. Ignore one-off task content.
2. **Pick at most 3.** Keep only lessons that would change what you do on a different task next week. Zero is a fine answer; say "nothing worth keeping" and stop.
3. **Find my context files.** Run `recall config get keep.targets`. Each line is `<path>: <what belongs there>`. Skip paths that don't exist on this machine. If nothing is configured, use `recall config get learn.targets`, then fall back to `~/.claude/CLAUDE.md` and the files it `@`-imports, and read each file's opening lines for its scope.
4. **Choose the home for each.**
   - The narrowest context file whose scope fits.
   - A skill's `SKILL.md`: only for a procedure I spelled out step by step.
   - A script in my bin directory (`recall config get keep.bin`, default `~/bin`): when a command could check or compute it (formatting, time math, which branch I'm on). Propose the script and a one-line pointer to it, not a rule asking you to be careful.
   - Specific to one repo: mention it, don't write it anywhere.
5. **Propose as a diff.** Read the target file first. Prefer rewording an existing bullet to adding one, and never duplicate. Use my wording, one short imperative line, one sentence per line. Show all proposals together, each with a one-line reason, and ask **yes / no / tweak** for each.
6. **Apply what I approve,** then show `git diff --stat` in each repo you edited, and run `recall learn rules sync` so the new or changed bullets land on the rule ledger with today's date. Don't commit or push unless I ask.

Never copy secrets, hostnames, account IDs, ARNs, internal URLs, ticket numbers, or colleague or customer names into these files.

Resolve symlinks before editing a skill or script. If it lives under a path from `recall config get keep.public`, it is shared publicly: keep anything about the owner out of it, and put the personal part in a context file instead.
