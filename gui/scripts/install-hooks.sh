#!/bin/sh
# Installs a pre-push hook that runs check-secrets.sh on everything being pushed.
set -eu
root=$(git rev-parse --show-toplevel)
hook="$(git rev-parse --git-path hooks)/pre-push"
cat >"$hook" <<'HOOK'
#!/bin/sh
zero=0000000000000000000000000000000000000000
while read -r _ local_sha _ remote_sha; do
	[ "$local_sha" = "$zero" ] && continue
	range=$local_sha
	[ "$remote_sha" != "$zero" ] && range="$remote_sha..$local_sha"
	if ! "$(git rev-parse --show-toplevel)/gui/scripts/check-secrets.sh" "$range"; then
		echo "pre-push: blocked. Remove the secret from the commits, then push again."
		exit 1
	fi
done
HOOK
chmod +x "$hook"
echo "Installed $hook"
