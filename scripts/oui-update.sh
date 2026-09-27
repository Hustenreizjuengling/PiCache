#!/bin/sh
# Refresh the embedded MAC vendor table internal/oui/oui.bin from the IEEE
# registries (docs/ARCHITECTURE.md 4, package oui). Run before a release:
#
#   make oui          (or: sh scripts/oui-update.sh from the repository root)
#
# Downloads the CSV exports of MA-L, MA-M and MA-S into a temporary
# directory and runs the generator (go run internal/oui/gen/main.go <dir>),
# which rewrites internal/oui/oui.bin. A plain `go build` never touches the
# network. Record the retrieval date in THIRD_PARTY_NOTICES.md. Exit codes:
# 0 written, 1 a download or the generator failed.
set -eu

base=https://standards-oui.ieee.org
if [ ! -f internal/oui/oui.go ]; then
	echo "$0: run it from the repository root" >&2
	exit 1
fi
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT HUP INT TERM
for f in oui/oui.csv oui28/mam.csv oui36/oui36.csv; do
	name=${f#*/}
	# The registry answers some clients with 418 now and then: retry.
	if ! curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors --max-time 120 \
		-o "$dir/$name" "$base/$f"; then
		echo "$0: could not download $base/$f" >&2
		exit 1
	fi
done
go run internal/oui/gen/main.go "$dir"
echo "retrieved $(date -u +%Y-%m-%d): update the date in THIRD_PARTY_NOTICES.md"
