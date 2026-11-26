#!/bin/sh
# Print one release's section of CHANGELOG.md, for the GitHub Release body.
#
#   sh scripts/changelog-section.sh 0.1.1
#
# The heading is left out (the release page carries the tag). A version with no section, or an
# empty one, is a failure: the release workflow runs this before publishing anything, so a tag
# pushed without notes is refused rather than released with an empty body.
set -eu
version=${1:?usage: changelog-section.sh <version>}
version=${version#v}
out=$(awk -v h="## $version" '
    index($0, h) == 1 && (length($0) == length(h) || substr($0, length(h) + 1, 1) == " ") { on = 1; next }
    on && /^## / { exit }
    on { print }
' "$(dirname "$0")/../CHANGELOG.md" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')
if [ -z "$out" ]; then
    echo "CHANGELOG.md has no section for $version: add '## $version — <date>' with its notes" >&2
    exit 1
fi
printf '%s\n' "$out"
