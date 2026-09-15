# AI transcripts

Required by the assignment: every prompt and every visible AI response,
including sessions whose output was not used.

## Index

| # | File | Tool / model | Date (UTC) | Topic | Output used? |
|---|------|--------------|-----------|-------|--------------|
| 1 | `01-claude-build-and-observability.md` | Claude (Anthropic) | Opus-5| Go build failures, Dockerfile size reduction, compose + monitoring stack, CI workflow | Yes — with modifications, see NOTES.md §7 |

Add a row per session. Chronological order.

## How to capture this session

The web interface has no export button, so copy the visible conversation
manually — prompts and responses, in order, nothing omitted. Sessions whose
output you discarded still belong here; the instructions are explicit about
that, and an obviously curated transcript reads worse than a messy complete one.

Suggested structure per file:

```markdown
# Session 1 — <topic>

- Tool: Claude (Anthropic)
- Model: Opus-5
- Date: 2026-09-14
- Outcome: used / partially used / rejected

---

## Prompt 1
<verbatim>

## Response 1
<verbatim>

...
```

## Redaction

Replace only sensitive values, keeping the surrounding text readable:

`[REDACTED: GHCR token]`, `[REDACTED: absolute home path]`

Before committing:

```bash
grep -rniE 'ghp_|github_pat_|AKIA|BEGIN [A-Z ]*PRIVATE KEY|password|secret' \
  deploy/ai-transcripts/
```

Nothing in the session as conducted contained a real credential — the workflow
uses the run-scoped `GITHUB_TOKEN` and never echoes it — but check rather than
assume.

## Note on completeness

