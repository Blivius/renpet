# renpet

A local time ledger for you and your AI agents. Named after the Egyptian goddess of the year
and renewal, whose name was also the Egyptian word for "year".

Where your time really goes when you work with AI agents: per project, **for whom** (office,
Blivius, self, automation), **agent-running vs your think time**, and how many agents ran **in
parallel**. P0 spike: it reads local transcripts. It is read-only: no daemon, no database,
nothing leaves the machine.

```sh
go install github.com/blivius/renpet@latest   # or: go run . <command>

go run . week                       # last 7 days
go run . week --since 2026-11-18 --until 2026-11-25 --idle 15m
go run . day                        # today, animated (←/→ other days, t today, q quit)
go run . day 2026-11-22 --static    # plain text + full table (also what pipes and NO_COLOR get)
go run . gaps                       # your prompt-gap histogram: helps pick --idle
go run . why tydbase                # the sessions behind a project's numbers
go run . week --json | jq .         # machine-readable
go run . week --rules my-rules.json # your own "for whom" rules
```

`day` animates the way [termheat](https://gitlab.com/moeen-mahmud/termheat) does:
- a left-to-right reveal on every load;
- lit cells breathe;
- quarter-hours where 3+ agents ran at once burn in a fire ramp;
- today gets a flickering `▲ now`.

Terminals narrower than 107 columns get 30-minute cells instead of 15. `--static` (and any
pipe) prints one character per 15 minutes:

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

## Picking `--idle`

`renpet gaps` shows how long you took to send each prompt after the agent's last event. Put
`--idle` where the bars thin out: shorter gaps are you thinking, longer ones are you away. On one
developer's 30 days (1,316 prompts), the rate fell from about 16 prompts per minute of gap at
10–15 min to about 6 at 15–20 min, so 15 min (the default) sat right on the knee.

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
