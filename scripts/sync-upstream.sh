#!/bin/sh
set -eu

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

if ! git diff --quiet || ! git diff --cached --quiet; then
	echo "working tree must be clean before rebasing upstream" >&2
	exit 1
fi

upstream_remote=${1:-upstream}
upstream_branch=${2:-main}

git fetch "$upstream_remote" "$upstream_branch"
git rebase --exec './scripts/enforce-local-workflows.sh' "$upstream_remote/$upstream_branch"

./scripts/enforce-local-workflows.sh
