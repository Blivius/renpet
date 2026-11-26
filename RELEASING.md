# Releasing

One binary, one number, one tag. Semver. Short lines in `CHANGELOG.md`, written for someone
installing the release.

1. **Draft the notes.** `git cliff --unreleased --strip all` prints the `feat`, `fix` and `perf`
   commits since the last tag (`cliff.toml`). Edit them under `## Unreleased` into one line per
   change, grouped as Added, Changed, Fixed, and **Breaking** where it is.
2. **Name the section.** Rename `## Unreleased` to `## 0.1.2 — <today>` and put an empty
   `## Unreleased` above it. `sh scripts/changelog-section.sh 0.1.2` prints what the release page
   will say.
3. **Check, commit, tag.** `go vet ./... && go test ./...`, commit, push, then
   `git tag v0.1.2 && git push origin v0.1.2`. The tag is what publishes.
4. **Watch the workflow.** `release.yml` refuses a tag with no changelog section before building
   anything. Then GoReleaser builds the four archives, creates the GitHub Release with the
   section as its body, attaches `install.sh` and `checksums.txt`, and writes `Casks/renpet.rb`
   to `blivius/homebrew-tap`. It needs `HOMEBREW_TAP_TOKEN`: a fine-grained token with Contents
   read and write on `blivius/homebrew-tap` only.
5. **Check the installs.**

   ```sh
   curl -fsSL https://github.com/blivius/renpet/releases/latest/download/install.sh | sh
   brew update && brew install blivius/tap/renpet
   go install github.com/blivius/renpet@latest
   ```

A release that failed part-way: fix it, then cut the next patch number. Release assets and the
tap commit are already public.
