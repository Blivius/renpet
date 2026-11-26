# renpet

A local time ledger for you and your AI agents. Named after the Egyptian goddess of the year
and renewal, whose name was also the Egyptian word for "year".

Where your time really goes when you work with AI agents: per project, **for whom** (work,
side projects, automation… you name them), **agent-running vs your think time**, and how many agents ran **in
parallel**. P0 spike: it reads local transcripts. It is read-only: no daemon, no database,
nothing leaves the machine.

```sh
go install github.com/blivius/renpet@latest   # or: go run . <command>

renpet                              # the app: everything below has a home in it ([:] commands, [?] keys)
go run . week                       # last 7 days as a table
go run . week --since 2026-11-18 --until 2026-11-25 --idle 15m
go run . day 2026-11-22             # the week holding that day, with the day selected
go run . day 2026-11-22 --static    # plain text + full table (also what pipes and NO_COLOR get)
go run . now                        # agents working and waiting on you right now
go run . now --short                # one line for tmux or a shell prompt: ●2 ◐1 · 4h 12m
go run . card --hide-names          # the last 7 days as a card to screenshot, names blanked
go run . gaps                       # your prompt-gap histogram: helps pick --idle
go run . why tydbase                # the sessions behind a project's numbers, as a table
go run . week --json | jq .         # machine-readable
go run . config                     # where settings live, and what's in effect
go run . config init                # write a starter settings file
```

The app is the place to use Renpet: every flag below has a home in it. Views down the left
(a tab bar under 100 columns), a command palette on `:` or `ctrl+k`, every key on `?`, and the
mouse for clicks and scrolling. Its grammar is borrowed from
[termheat](https://gitlab.com/moeen-mahmud/termheat):

```
 1 Overview 2 Live 3 Timeline 4 Projects 5 Sessions 6 Records 7 Gaps 8 Settings

 week of Nov 23                                    ▲ 17% vs this point last week

 you       8h 16m  █████████████
 agents   17h 45m  ████████████████████████████  2.1× you
 agents waited on you 9h 16m · peak 7 at once, Mon 22:53

        00 01 02 03 04 05 06 07 08 09 10 11 12 13 14 15 16 17 18 19 20 21 22 23
 Mon    ██ ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ██ ·· ·· ·· ██ ██ ██ ██ ██ ██ ██ ██
 Tue    ██ ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ██ ·· ██ ██ ██ ██ ██ ██ ·· ·· ██
 Wed ›  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ██ ██ ██ ██ ██ ██ ██ ██ ██ ██ ·· ··
 …

 agents ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ██ ██ ██ ██ ██ ██ ██ ██ ██ ██ ·· ··
 you    ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ██ ██ ██ ██ ██ ██ ·· ██ ██ ██ ·· ··
 Wed Nov 25  5h 04m with agents · agents 5h 55m · you 2h 42m · peak 7
 for    Side 12h 18m · Work 4h 22m · Self 3h 15m · Unknown 30m
 ⚖  Side took 60% of your agent time.

 [↑/↓] day  [←/→] week  [⏎] projects  [r] day·week·month  [?] help
```

- **Headline:** your hours, your agents' hours and the multiple, and how long your agents sat
  waiting for you. Next to the title, the change against the same stretch of the period before
  (a Wednesday is compared with last Monday–Wednesday, not all of last week). It is left out
  when the agents have already deleted the older logs it would need.
- **Grid:** one cell per hour, every hour labelled (00 06 12 18 stand out). Brightness is how
  much of that hour you or your agents were active, on one green ramp. A month shows as a
  calendar instead: one cell per day, brightness against the busiest day.
- **Fire cells:** hours where 3+ agents ran at once burn in a fire ramp and flicker.
- **Lanes:** the selected day is split below the grid into agents vs you.
- **Voice line:** one true line about the range, never invented. In order of priority:
  `❓ N% has no owner yet`, `⚖ … took N%`, `🔥 N agents at once`, `🌙 past midnight on N
  days`, `🧠 N% thinking`.
- **Ranges:** `r` steps day → week → month, `←/→` (or `[ ]` from any view) moves through them,
  `t` comes back to today. Custom dates are in the palette.
- **Colours** follow your terminal: a light background gets a light palette (the palette can
  switch it). `NO_COLOR` is respected.
- **Logo:** the sidebar's mark is drawn from the logo's own shapes in half blocks, so it is the
  real mark in any terminal, no image support needed.
- **Motion:** time-based at 30 fps. A 0.45 s ease-out reveal on every load, lit cells breathe
  between 90% and 100%, and fire cells flicker at 1.4 Hz.
- **Speed:** transcripts are parsed on all CPUs, and only once. How far each file has been read
  is kept in your user cache dir (`renpet/sessions-v3.gob`: timestamps, folders, session titles
  and the first 120 characters of your first prompt; never your other prompts), so
  the next run reads just the new lines. On ~670 MB of transcripts a week, the first run takes
  about 1.5 s and later runs about 0.1 s. Finished weeks also stay in memory while the view is
  open.
- **Fallback:** pipes, CI, `NO_COLOR` and `--static` get plain text: `day --static` prints the
  15-minute strips and the full table.

## Views and keys

Every number leads to its evidence. `⏎` (or `→`) goes one level deeper, `esc` (or `←`) one back.
Number keys jump between views; `?` shows the keys for the view you're on.

| View | Shows | Keys |
|---|---|---|
| 1 Overview | The headline, the grid, the selected day's lanes, who it was for, the voice line | `↑/↓` day · `←/→` period · `⏎` projects |
| 2 Live | Sessions working now (`●`) and waiting on you (`◐`, its agent ended its turn), each with how long and its last 30 minutes as a lane, and today so far. Reads the logs every 2 s while open | `↑/↓` session · `⏎` its prompts |
| 3 Timeline | One day, one lane per session: `━` while its agent works, `─` while it waits on you, `┿` where you sent a prompt, and a row counting the agents working at once | `↑/↓` session · `←/→` day · `⏎` its prompts · `/` filter · `a` all |
| 4 Projects | The range's projects, by hours, repo name first, coloured by whom | `tab` day/range · `/` filter · `o` order (hours, name, whom) · `a` all |
| 5 Sessions | Every session, or one project's: when, agent, hours, an activity sparkline, the session's name | `tab` · `/` · `o` order (time, hours) · `a` all |
| Prompts | One session: agent and you lanes, then every prompt you sent with how long you thought before it and how long the agents ran after it | `↑/↓`; the selected prompt shows in full below |
| 6 Records | Your bests across every transcript on disk: most agents at once, longest run without you, latest night, busiest day, biggest multiple, streaks. Then your perception checks | `⏎` the timeline of that day |
| 7 Gaps | Your prompt-gap histogram with the idle line on it | `←/→` move the line · `⏎` save it as your idle gap |
| 8 Settings | Week start, idle gap, your rules, and the range's folders no rule matches | `←/→` change · `x` delete a rule · `K/J` move it · `⏎` assign a folder |

**Everywhere:** `:` or `ctrl+k` commands · `r` day·week·month · `[ ]` previous/next · `t` today
· `p` perception check · `c` card (`n` hides names) · `?` keys · `q` quit · click a view, a row (a second click opens it) or a day; scroll to move.

**The palette** reaches what the flags do: a day, week, month or custom range (`--since`,
`--until`), the idle gap (`--idle`), showing everything (`--all`), exporting the range as JSON or
a table (`--json`, `renpet week`) next to where you started Renpet, creating the settings file
(`config init`), and the theme.

- **Session names:** the name you gave a Claude Code session, else the one Claude Code gave it,
  else your first prompt. Subagents carry their parent's name (`↳ …`).
- **Folded rows:** projects under 6 minutes are hidden, automation folders are one row
  (`Automation · 57 folders`), and a subagent's time is folded into the session that started it
  (`+15 subagents`), until you press `a`. They are still in every total, and a line says so.
- **Sessions that move:** a Claude Code or Codex session that moves to another repo (a `cd`, or
  a resume from another folder) is cut where it moved. Each part counts for its own project and
  whom, so one transcript can show under two projects. A `cd` inside the same repo cuts nothing.
  Antigravity transcripts don't record folders, so they stay whole.
- **Perception check:** `p` hides the numbers and asks how many hours you spent with your
  agents in the range on screen. Then it shows what was measured and how far off you were, and
  saves the guess to `guesses.json` next to your settings. Records lists your past guesses.
- **Live and `renpet now`:** a session is live until its log has been quiet for the idle gap.
  It waits on you once its agent ended its turn: Claude Code's `end_turn`, Codex's
  `task_complete`, an Antigravity answer that calls no tool. A permission prompt still reads as
  working; agent hooks could tell the two apart later. Finished subagents and finished
  automation runs are left out.
- **Card:** `c` shows the range as a card to screenshot: you vs agents, who it was for, the top
  three projects and the voice line. `n` blanks every name (the colours stay) and drops the
  voice line if it names one. `renpet card` prints it, `--json` as data.
- **Prompt text** is read from the transcript when you open a session. It is never cached.
- **Settings save as you change them** to your config file (written through a symlink, never
  replacing it), and the range is recounted with them right away.

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
| agents | agent-run time summed across sessions, so agents running in parallel add up |
| you | wall-clock in think gaps: two sessions waiting on you at once count once |
| agents waited on you | think gaps summed per session: how long your agents sat waiting for you |
| peak ∥ | the most agents working at once, and when |

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

## Settings

Settings live in `~/.config/renpet/config.json`. `$XDG_CONFIG_HOME` and `$RENPET_CONFIG` are
respected. Flags override the file for one run. `renpet config` shows the path and what's in
effect; `renpet config init` writes a starter file. In the app, Settings (`8`) edits the same file.

```json
{
  "week_start": "monday",
  "idle": "15m",
  "rules": [
    {"remote": "github.com/acme/", "whom": "work"},
    {"path": "~/work", "whom": "work"},
    {"path": "~/.claude/jobs", "whom": "automation"},
    {"path": "~/code", "whom": "side"}
  ]
}
```

| Setting | Meaning | Flag |
|---|---|---|
| `week_start` | First row of the week grid, `monday` … `sunday` | `--week-start` |
| `idle` | A gap longer than this is nobody's time; see below | `--idle` |
| `rules` | Who each folder's time is for. **Ordered: the first match wins** | `--rules file.json` |

**How rules match**
1. A `remote` rule matches a substring of the repo's origin URL. It works even when a work repo
   is cloned under a personal folder.
2. A `path` rule matches the folder and everything under it.
3. Anything no rule matches shows as `unknown`. It is never guessed.

The built-in default knows only `~/.claude/jobs` → automation. Your rules are personal, so they
belong in your config, not in this repo.

## Known limits (P0)

| Limit | Effect |
|---|---|
| Waiting on a permission prompt counts as agent-run | P1 hooks split it out |
| Antigravity IDE conversations have no workspace on disk | They show as `(no workspace)` / unknown |
| Cursor and Gemini CLI are not read | Their time is missing |
| Only time inside agent sessions is seen | Editor, browser and meetings are missing until P1 capture |
| Transcript formats are undocumented and can change | Unparseable lines are skipped, never guessed |
| Claude Code deletes transcripts after 30 days unless `cleanupPeriodDays` is raised | Older history is lost |
