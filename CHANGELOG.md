# Changelog

## Unreleased

## 0.1.1 — 2026-11-26

### Added

- One-line install for macOS and Linux: `curl -fsSL https://github.com/blivius/renpet/releases/latest/download/install.sh | sh`. It checks the download against `checksums.txt`, puts `renpet` in `~/.local/bin` and adds that to your PATH if it isn't there yet.
- Release notes come from this file.

### Fixed

- `brew install blivius/tap/renpet` works. The 0.1.0 release could not write its cask to the tap.

## 0.1.0 — 2026-11-26

First release.

### Added

- Reads the Claude Code (with subagents), Codex and Antigravity logs already on disk. Read-only: no daemon, no account, no network code.
- The app: Overview (week grid, headline numbers, the voice line), Live, Timeline, Projects, Sessions with every prompt, Records, Gaps and Settings. Command palette on `:` or `ctrl+k`, keys on `?`, mouse, light and dark terminals, 80×24 and up.
- Who the time was for, by ordered rules on git remote or path, edited in Settings or `~/.config/renpet/config.json`.
- Live view and `renpet now`: agents working and agents waiting on you, read from the logs every 2 seconds. `--short` prints one line for tmux or a shell prompt.
- Share card: `c` in the app or `renpet card`; `n` (or `--hide-names`) blanks every name.
- Perception check: `p` asks how many hours you think you spent, then shows the measured number.
- `--demo`: a made-up developer's six weeks, on every view and command.
- Commands: `week`, `day`, `now`, `card`, `why`, `gaps`, `config`, with `--json`.
- Installs: Homebrew cask (macOS), `go install`, and archives for macOS and Linux on amd64 and arm64.
