# renpet

A local time ledger for you and your AI agents. Named after the Egyptian goddess of the year
and renewal, whose name was also the Egyptian word for "year".

Where your time really goes when you work with AI agents: per project, **for whom** (office,
Blivius, self, automation), **agent-running vs your think time**, and how many agents ran **in
parallel**. P0 spike: it reads local transcripts. It is read-only: no daemon, no database,
nothing leaves the machine.

```sh
go install github.com/blivius/renpet@latest   # or: go run . <command>

renpet                              # this week, animated (or: go run .)
go run . week                       # last 7 days as a table
go run . week --since 2026-11-18 --until 2026-11-25 --idle 15m
go run . day 2026-11-22             # the week holding that day, with the day selected
go run . day 2026-11-22 --static    # plain text + full table (also what pipes and NO_COLOR get)
go run . gaps                       # your prompt-gap histogram: helps pick --idle
go run . why tydbase                # the sessions behind a project's numbers
go run . week --json | jq .         # machine-readable
go run . week --rules my-rules.json # your own "for whom" rules
```

The interactive view borrows [termheat](https://gitlab.com/moeen-mahmud/termheat)'s grammar:

```
🌿 renpet  week of Nov 16 — moeen

       00  02  04  06  08  10  12  14  16  18  20  22
Mon    ██····························██████████████████
Tue    ····························████████████████····
…
Sun ›  ██······················████████████████████████

       Less ·· ██ ██ ██ ██ More   ██ 3+ agents at once

agents ██······················████████████████████████
you    ██······················██████████████··████████

Sun Nov 22  8.5h with agents · 5.7h agents ran · 3.7h you · peak 5
            Blivius 4.7h · Office 4.0h · Self 0.5h
This week   42.2h with agents · 37.1h agents ran · 18.4h you · peak 10
            Office 29.8h · Self 11.3h · Blivius 4.8h

⚖  Office took 72% of your agent time this week.

[←/→] week  [↑/↓] day  [t] today  [q] quit
```

- **Grid:** one 2-character cell per hour. Brightness is how much of that hour you or your
  agents were active, on one green ramp.
- **Fire cells:** hours where 3+ agents ran at once burn in a fire ramp and flicker.
- **Lanes:** the selected day is split below the grid into agents vs you.
- **Voice line:** one true line about the week, never invented. The four lines are
  `⚖ … took N%`, `🔥 N agents at once`, `🌙 past midnight on N days` and `🧠 N% thinking`.
- **Motion:** a left-to-right reveal on every load, and lit cells breathe.
- **Speed:** finished weeks are read once and kept in memory.
- **Fallback:** pipes, CI, `NO_COLOR` and `--static` get plain text: `day --static` prints the
  15-minute strips and the full table.

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
