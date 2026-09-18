#!/usr/bin/env bash
#
# Deploys main through the CD workflow after the convergence report reads BEHIND (#3869).
#
# The caller holds the prod-deploy lock, so no other observer can decide at the same
# time. Two rules keep a burst of pushes from stacking redundant deploys:
#
#   - skip when a CD run that has not finished already deploys main's current tip; a
#     later observer then sees that run instead of dispatching its own
#   - after dispatching, wait until the new run is listed, so the lock is only
#     released once the next observer can see it
#
# A CD run for an older commit does not count: it would leave prod behind the tip.
#
#   exit 0  a CD run for main's tip was dispatched, or one was already pending
#   exit 1  the tip, the run list, the dispatch, or the new run could not be confirmed

set -uo pipefail

repository="${REDEPLOY_REPOSITORY:?REDEPLOY_REPOSITORY is required}"
main_ref="${REDEPLOY_MAIN_REF:-origin/main}"
attempts="${REDEPLOY_POLL_ATTEMPTS:-30}"
interval="${REDEPLOY_POLL_INTERVAL:-2}"

fail() {
  printf '::error title=Redeploy of main not confirmed::%s\n' "$1"
  exit 1
}

# Runs of cd.yaml on main that have not finished, as `<id> <head sha>` lines.
unfinished_runs() {
  gh run list --repo "$repository" --workflow cd.yaml --branch main --limit 100 \
    --json databaseId,headSha,status \
    --jq '.[] | select(.status != "completed") | "\(.databaseId) \(.headSha)"'
}

tip="$(git rev-parse --verify --quiet "${main_ref}^{commit}")" || fail "could not resolve ${main_ref}"
[[ "$tip" =~ ^[0-9a-f]{40}$ ]] || fail "${main_ref} resolved to '${tip}', not a commit"

before="$(unfinished_runs)" || fail "could not list CD runs on main"

if TIP="$tip" awk '$2 == ENVIRON["TIP"] { found = 1 } END { exit !found }' <<<"$before"; then
  printf '::notice title=Redeploy already pending::a CD run for %s has not finished; not dispatching another\n' "$tip"
  exit 0
fi

gh workflow run cd.yaml --repo "$repository" --ref main || fail "could not dispatch cd.yaml on main"

for ((attempt = 1; attempt <= attempts; attempt++)); do
  if after="$(unfinished_runs)" &&
    BEFORE="$before" awk '
      BEGIN { n = split(ENVIRON["BEFORE"], lines, "\n"); for (i = 1; i <= n; i++) { split(lines[i], f, " "); seen[f[1]] = 1 } }
      NF && !($1 in seen) { found = 1 }
      END { exit !found }
    ' <<<"$after"; then
    printf '::notice title=Redeploying main::dispatched CD because prod is behind %s\n' "$tip"
    exit 0
  fi
  sleep "$interval"
done

fail "dispatched cd.yaml on main, but no new CD run appeared after ${attempts} checks"
