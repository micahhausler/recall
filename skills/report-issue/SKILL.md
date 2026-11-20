---
name: report-issue
description: Draft a GitHub issue on the recall repo (github.com/micahhausler/recall) and file it only after I approve the exact text. Use when I say "report this to recall", "file a recall issue", or hit a recall bug or missing feature worth reporting. It gathers the recall version, OS, harness, what happened, and what was expected, and never files anything without my explicit yes.
allowed-tools: Bash(recall extension report-issue), Bash(recall version), Bash(uname -sr), Bash(gh issue create:*)
---

Issues go to `github.com/micahhausler/recall`; the bug and feature forms in its `.github/ISSUE_TEMPLATE/` show the shape to follow.

1. **Gather.** Run `recall version` and `uname -sr`.
   Ask which harness was involved (for example Claude Code), what happened, and what was expected instead, unless this session already makes that clear; confirm your reading with me before drafting.
2. **Keep it clean.** The issue carries only those five facts in my words: version, OS, harness, what happened, what was expected.
   Never include session text, transcript excerpts, hostnames, usernames, account IDs, ARNs, internal URLs, or paths that name a person; replace a needed path with a placeholder like `~/project`.
3. **Draft.** Write a short imperative title and a body with those five facts as sections, matching the bug or feature form.
4. **Show and ask.** Show me the full title and the full body, exactly as they will be filed, and ask yes or no.
   On a no or a tweak, edit and show the whole text again; never file a draft I haven't seen in its final form.
5. **File.** Only after my explicit yes, run `gh issue create --repo micahhausler/recall --title "<title>" --body "<body>"`, then show the issue URL it prints.
   If `gh` is missing or not authenticated, print that same command with the drafted title and body for me to run myself, and don't try another way to file it.

Never run `gh issue create` before step 4's yes, and never file more than the one issue I approved.

## Owner additions

The owner's own steps for this skill, from `extensions.report-issue` in recall config; follow them where they don't conflict with the steps above.

!`recall extension report-issue`

If the line above still shows the command rather than its output, run `recall extension report-issue` and follow what it prints.
