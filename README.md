# ledger (working name)

Where your time really goes when you work with AI agents: per project, **for whom** (office,
Blivius, self, automation), **agent-running vs your think time**, and how many agents ran **in
parallel**. P0 spike: it reads local transcripts. It is read-only: no daemon, no database,
nothing leaves the machine.

```sh
go install github.com/blivius/project-ledger@latest   # or: go run . <command>

go run . week                       # last 7 days
go run . week --since 2026-11-18 --until 2026-11-25 --idle 15m
go run . day                        # today as a 24h strip, then the table
go run . day 2026-11-22
go run . why tydbase                # the sessions behind a project's numbers
go run . week --json | jq .         # machine-readable
go run . week --rules my-rules.json # your own "for whom" rules
```

`day` draws one character per 15 minutes:

```
        0   1   2   3   4 …  12  13  14 …
 agents ..11…………………………………2121111111..1112521…   most agents working at once
    you ..█……………………………………▄████▄███....▄█▄███…   thinking: ▄ some, █ ≥5 min
   whom ..ss…………………………………oooooooooo..ooooooob…   o office, b blivius, s self, a automation, ? unknown
```

## What it measures

**Sources**

| Agent | Transcripts read |
|---|---|
| Claude Code | `~/.claude/projects/*/*.jsonl` |
| Claude Code subagents | `~/.claude/projects/*/<session>/subagents/*.jsonl`. Every line is agent time; the parent agent writes their "user" lines |
| Codex | `~/.codex/sessions/**/rollout-*.jsonl`, sub-sessions included |
| Antigravity | `~/.gemini/antigravity*/brain/<id>/.system_generated/logs/transcript.jsonl`. You are `USER_EXPLICIT`. The workspace comes from the CLI's `cache/conversation_metadata.json` |

**How each gap is classified**

Each gap between two consecutive events counts as whatever ends it:

| Gap ends in… | Counts as |
|---|---|
| your prompt | **think** (reading, reviewing, typing) |
| agent output or a tool result | **agent-run** |
| anything, but the gap is longer than `--idle` | nobody's time |

**Report columns**

| Column | Meaning |
|---|---|
| with agents | wall-clock covered by any session |
| agent-run | summed across sessions, so agents running in parallel add up |
| peak ∥ | the most agents working at once |

**What counts as a prompt**

User lines that arrive from the system don't count as prompts:
- tool results
- task notifications
- command output
- resume summaries
- `<environment_context>`

## For whom

`rules.json` is ordered, and the first match wins.

1. A git remote substring (`tulip-tech/` → office) is checked before path prefixes. A TulipTech
   repo cloned under `~/Personal` still counts as office.
2. Anything with no matching rule shows as `unknown`. It is never guessed.

## Known limits (P0)

| Limit | Effect |
|---|---|
| Waiting on a permission prompt counts as agent-run | P1 hooks split it out |
| Antigravity IDE conversations have no workspace on disk | They show as `(no workspace)` / unknown |
| Cursor and Gemini CLI are not read | Their time is missing |
| Only time inside agent sessions is seen | Editor, browser and meetings are missing until P1 capture |
| Transcript formats are undocumented and can change | Unparseable lines are skipped, never guessed |
| Claude Code deletes transcripts after 30 days unless `cleanupPeriodDays` is raised | Older history is lost |
