#!/bin/sh
# Every published version must have its own section in CHANGELOG.md, and
# every section must correspond to a published version — apart from
# "Unreleased", which holds pending work until a number is tagged.
#
# This check exists because the two drifted twice: entries piled up under one
# heading while three versions had already been released, so the release notes
# on GitHub described things the version did not contain.
set -e
cd "$(dirname "$0")/.."

fail=0

# Sections, in the order they appear.
sections=$(grep '^## ' CHANGELOG.md | sed 's/^## //')

# A shallow clone has no tags: the ordering check still applies, the
# comparison against tags cannot, and failing there would break the CI of
# anyone who clones without them.
tags=$(git tag 2>/dev/null || true)
if [ -z "$tags" ]; then
	echo "no tags available (shallow clone?): checking section order only"
fi

# 1. Every tag has a section.
for tag in $tags; do
	version=$(echo "$tag" | sed 's/^v//')
	case "$version" in *-*) continue ;; esac   # pre-releases are exempt
	if ! echo "$sections" | grep -qx "$version"; then
		echo "CHANGELOG.md has no '## $version' section for tag $tag" >&2
		fail=1
	fi
done

# 2. A section with no tag is only worth a note: a clone that does not carry
# every tag would otherwise report a long list of false alarms. The drift that
# actually happens is the other way round, and check 1 catches it.
for section in $sections; do
	[ "$section" = "Unreleased" ] && continue
	[ -z "$tags" ] && continue
	if ! git rev-parse -q --verify "refs/tags/v$section" >/dev/null 2>&1; then
		echo "note: '## $section' has no tag v$section here (not yet released, or tag not fetched)"
	fi
done

# 3. Newest first, and Unreleased on top when present.
first=$(echo "$sections" | head -1)
ordered=$(echo "$sections" | grep -v '^Unreleased$' | sort -rV)
actual=$(echo "$sections" | grep -v '^Unreleased$')
if [ "$ordered" != "$actual" ]; then
	echo "CHANGELOG.md sections are not in descending version order" >&2
	fail=1
fi
if echo "$sections" | grep -qx "Unreleased" && [ "$first" != "Unreleased" ]; then
	echo "the Unreleased section must come first in CHANGELOG.md" >&2
	fail=1
fi

[ "$fail" = 0 ] && echo "CHANGELOG.md matches the published versions"
exit $fail
