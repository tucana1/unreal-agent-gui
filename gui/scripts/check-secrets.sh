#!/bin/sh
# Fails if tracked files, or the commits in an optional range, contain likely
# secrets. Matches are reported by file and line only, never by value, so the
# output is safe for public CI logs.
#
# Usage: gui/scripts/check-secrets.sh [<revision range>]
set -eu
cd "$(git rev-parse --show-toplevel)"

pattern='sk-or-v1-[0-9a-f]{20,}|sk-(proj-|ant-)?[A-Za-z0-9_-]{32,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|xox[baprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{35}'
excluded=':!third_party :!internal/openaiapi/types.gen.go'
found=0

# shellcheck disable=SC2086 # $excluded holds several pathspecs.
if git grep -nIE "$pattern" -- . $excluded | cut -d: -f1,2 | grep .; then
	echo "check-secrets: the files above look like they contain a secret."
	found=1
fi
if [ $# -gt 0 ] && git log -p --format='commit %H' "$1" | grep -qIE "$pattern"; then
	echo "check-secrets: a commit in $1 looks like it contains a secret."
	found=1
fi

# Values saved in the app's local settings (API keys, environment variables)
# must never reach the repository.
config="${UAG_CONFIG:-$HOME/Library/Application Support/unreal-agent-gui/config.json}"
if [ -f "$config" ] && command -v python3 >/dev/null 2>&1; then
	secrets=$(mktemp)
	trap 'rm -f "$secrets"' EXIT
	python3 - "$config" >"$secrets" <<'PY'
import json, sys
config = json.load(open(sys.argv[1]))
for value in [*(config.get("api_keys") or {}).values(), *(config.get("env") or {}).values()]:
    if len(value.strip()) >= 12:
        print(value.strip())
PY
	if [ -s "$secrets" ]; then
		if git grep -qIF -f "$secrets" -- .; then
			echo "check-secrets: a key from your local Unreal Agent settings is in the working tree."
			found=1
		fi
		if [ $# -gt 0 ] && git log -p "$1" | grep -qF -f "$secrets"; then
			echo "check-secrets: a key from your local Unreal Agent settings is in $1."
			found=1
		fi
	fi
fi
exit "$found"
