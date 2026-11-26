#!/bin/sh
# Install renpet from a GitHub release.
#
#   curl -fsSL https://github.com/blivius/renpet/releases/latest/download/install.sh | sh
#
# Puts the binary in ~/.local/bin and, if that isn't on your PATH yet, adds it in your shell's rc
# file (it says which). Nothing needs root. Renpet only reads your agents' logs; it writes nothing
# but its own cache and settings.
#
#   RENPET_VERSION=0.1.1          a specific release instead of the latest
#   RENPET_INSTALL_DIR=<dir>      install somewhere other than ~/.local/bin
#   RENPET_NO_MODIFY_PATH=1       leave every rc file alone
#   RENPET_DOWNLOAD_BASE=<url>    fetch the archive and checksums.txt from a mirror (or a test build)
#
# Uninstall: rm ~/.local/bin/renpet, and delete the PATH line it added if it added one.

set -eu

slug=renpet
repo=blivius/renpet
version=${RENPET_VERSION:-latest}
bindir=${RENPET_INSTALL_DIR:-$HOME/.local/bin}

say() { printf '%s\n' "$*"; }
fail() {
    printf 'error: %s\n' "$1" >&2
    if [ -n "${2:-}" ]; then printf '  hint: %s\n' "$2" >&2; fi
    exit 1
}

# ── which build ───────────────────────────────────────────────────────────────────────────────
case $(uname -s) in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    MINGW* | MSYS* | CYGWIN*) fail "native Windows is not supported yet" "run this inside WSL (wsl --install)" ;;
    *) fail "no build for $(uname -s)" "go install github.com/$repo@latest" ;;
esac
case $(uname -m) in
    arm64 | aarch64) arch=arm64 ;;
    x86_64 | amd64) arch=amd64 ;;
    *) fail "no build for $(uname -m)" "go install github.com/$repo@latest" ;;
esac
# A shell under Rosetta reports x86_64 on Apple silicon; the native build is the right one.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
    arch=arm64
fi

if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
    # where /releases/latest redirects to: …/releases/tag/v0.1.1
    latest() { curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
    latest() { wget -q -S -O /dev/null "https://github.com/$repo/releases/latest" 2>&1 | awk '/^ *[Ll]ocation: /{ u = $2 } END { print u }'; }
else
    fail "neither curl nor wget is installed" "install one of them and run this again"
fi
if command -v sha256sum >/dev/null 2>&1; then
    digest() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
    digest() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
    fail "neither sha256sum nor shasum is installed" "the download cannot be checked without one"
fi

if [ "$version" = latest ]; then
    url=$(latest) || url=""
    version=${url##*/tag/}
    case $version in
        v[0-9]*) ;;
        *) fail "could not find the latest release" "pick one: RENPET_VERSION=0.1.1 (see https://github.com/$repo/releases)" ;;
    esac
fi
version=${version#v}
asset=${slug}_${version}_${os}_${arch}.tar.gz
base=${RENPET_DOWNLOAD_BASE:-https://github.com/$repo/releases/download/v$version}

# ── download and check ────────────────────────────────────────────────────────────────────────
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $slug $version for $os/$arch"
fetch "$base/$asset" "$tmp/$asset" ||
    fail "could not download $base/$asset" "check the version exists: https://github.com/$repo/releases"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download $base/checksums.txt"
expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || fail "checksums.txt does not list $asset"
actual=$(digest "$tmp/$asset")
[ "$actual" = "$expected" ] ||
    fail "$asset failed its checksum (got $actual, expected $expected)" "nothing was installed; try again"
tar -xzf "$tmp/$asset" -C "$tmp" "$slug"

# ── install ───────────────────────────────────────────────────────────────────────────────────
mkdir -p "$bindir"
chmod 755 "$tmp/$slug"
mv -f "$tmp/$slug" "$bindir/$slug" # a rename: a shell running renpet mid-install sees old or new
"$bindir/$slug" --version >/dev/null 2>&1 || fail "installed to $bindir/$slug, but it does not start" "run $bindir/$slug --version to see why"
say "installed $("$bindir/$slug" --version) to $bindir/$slug"

# ── PATH ──────────────────────────────────────────────────────────────────────────────────────
next="try it: $slug --demo (a made-up week), then $slug (yours)"
case ":$PATH:" in
    *":$bindir:"*)
        say "$next"
        exit 0
        ;;
esac
if [ "${RENPET_NO_MODIFY_PATH:-}" = 1 ]; then
    say "add $bindir to your PATH to run $slug (RENPET_NO_MODIFY_PATH is set, so no rc file was changed)"
    exit 0
fi
case $(basename "${SHELL:-sh}") in
    zsh) rc=${ZDOTDIR:-$HOME}/.zshrc line="export PATH=\"$bindir:\$PATH\"" ;;
    bash)
        if [ "$os" = darwin ]; then rc=$HOME/.bash_profile; else rc=$HOME/.bashrc; fi
        line="export PATH=\"$bindir:\$PATH\""
        ;;
    fish)
        rc=${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/$slug.fish
        line="fish_add_path \"$bindir\""
        mkdir -p "$(dirname "$rc")"
        ;;
    *) rc=$HOME/.profile line="export PATH=\"$bindir:\$PATH\"" ;;
esac
if ! grep -qsF "$line" "$rc"; then
    printf '\n# added by the %s installer\n%s\n' "$slug" "$line" >>"$rc"
    say "added $bindir to PATH in $rc"
fi
say "open a new terminal (or run: $line), then $next"
