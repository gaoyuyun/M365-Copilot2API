#!/bin/sh
set -eu

allowed_workflow=.github/workflows/publish-container.yml
extra_workflows=$(git ls-files .github/workflows | grep -vx "$allowed_workflow" || true)

if [ -z "$extra_workflows" ]; then
	exit 0
fi

printf '%s\n' "$extra_workflows" | while IFS= read -r workflow; do
	git rm -f -- "$workflow"
done

git commit --amend --no-edit --no-verify
