#!/bin/sh
# Print the CHANGELOG.md section of one version: the notes of its GitHub
# release (docs/ARCHITECTURE.md, section 14.1).
#
#   sh scripts/release-notes.sh v1.2.3 [CHANGELOG.md]
#
# The section starts at the heading "## [1.2.3]" (Keep a Changelog, normally
# "## [1.2.3] - YYYY-MM-DD") and ends before the next "## " heading. Blank
# lines at its start and link reference definitions ("[1.2.3]: https://…")
# are left out. The v prefix is optional. Exit codes: 0 notes printed,
# 1 no section or an empty one, 2 usage error.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: $0 vX.Y.Z [CHANGELOG.md]" >&2
	exit 2
fi
version=${1#v}
changelog=${2:-CHANGELOG.md}
case $version in
'' | *[!0-9A-Za-z.-]*)
	echo "$0: not a version: $1" >&2
	exit 2
	;;
esac
if [ ! -r "$changelog" ]; then
	echo "$0: cannot read $changelog" >&2
	exit 2
fi

status=0
notes=$(awk -v version="$version" '
	BEGIN { heading = "## [" version "]" }
	{ sub(/\r$/, "") }
	!found {
		if (index($0, heading) == 1 && (length($0) == length(heading) || substr($0, length(heading) + 1, 1) == " "))
			found = 1
		next
	}
	/^## / { exit }
	/^\[.*\]: / { next }
	!started && /^[ \t]*$/ { next }
	{ started = 1; print }
	END { if (!found) exit 3 }
' "$changelog") || status=$?

if [ "$status" -eq 3 ]; then
	echo "$0: $changelog has no section \"## [$version]\". Move the entries under" >&2
	echo "\"## [Unreleased]\" to \"## [$version] - YYYY-MM-DD\" before you tag v$version." >&2
	exit 1
elif [ "$status" -ne 0 ]; then
	echo "$0: awk failed on $changelog" >&2
	exit 2
elif [ -z "$notes" ]; then
	echo "$0: the section \"## [$version]\" in $changelog is empty" >&2
	exit 1
fi
printf '%s\n' "$notes"
