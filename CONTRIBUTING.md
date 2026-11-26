# Contributing

Issues labelled [good first issue](https://github.com/blivius/renpet/labels/good%20first%20issue)
are small and have the code location in them. [help wanted](https://github.com/blivius/renpet/labels/help%20wanted)
ones mostly need someone who uses a tool or system I don't, such as Cursor, OpenCode or Windows.
Comment on an issue before starting on it, so two people don't build the same thing.

## Build and test

```sh
go run . --demo          # the made-up week, no logs needed
go test ./...            # what CI runs, with gofmt and go vet
```

Go is in `go.mod`. There's one package. `main.go` reads the logs and does the counting, and the
other files are the views.

## What a pull request needs

- Tests for what it changes. `main_test.go` builds sessions from small inline fixtures. Copy the
  nearest test.
- Renpet stays read-only and local. No network calls, no writes outside its own cache and
  settings, and no prompt text in the cache.
- Never commit a real transcript. Write a fixture with made-up text and real-looking timestamps.
- A line in `CHANGELOG.md` under `## Unreleased` if users will notice the change.

## Adding an agent

1. Add a glob for its log files in `discover` (`main.go`).
2. Add a parser next to `parseClaude` and `parseCodex`, and a case in `parseFrom`. It calls
   `s.add(time, human, text)` per event, sets `s.Cwd`, and sets `s.Waiting` when the agent ends its turn.
3. Add a test with a fixture of a few lines.
