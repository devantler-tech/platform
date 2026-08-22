#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/scripts" "$fixture/config"
cp "$REPO_ROOT/scripts/guard-shared-publish-workflow-pin.sh" "$fixture/scripts/"

subject="^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@([0-9a-f]{40}|refs/tags/v.+)$"
for index in $(seq 1 8); do
  printf "subject: '%s'\n" "$subject" >>"$fixture/config/subjects.yaml"
done

"$fixture/scripts/guard-shared-publish-workflow-pin.sh" >/dev/null

sed -i \
  "1c\\subject: '^https://github\\\\.com/devantler-tech/actions/\\\\.github/workflows/publish-app\\\\.yaml@refs/heads/main$' # @refs/tags/v.+" \
  "$fixture/config/subjects.yaml"

if "$fixture/scripts/guard-shared-publish-workflow-pin.sh" >"$fixture/stdout" 2>"$fixture/stderr"; then
  printf 'test: inline-comment ref bypass was accepted\n' >&2
  exit 1
fi

grep -q 'refs/heads/main' "$fixture/stderr"
printf 'test: inline-comment ref bypass rejected\n'
