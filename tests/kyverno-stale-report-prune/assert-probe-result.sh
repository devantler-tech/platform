#!/usr/bin/env bash
# Read one complete PolicyReport from stdin. Only current, unambiguous evidence
# for this proof's exact rule and resource can establish a fail or skip result.
set -euo pipefail
[[ $# == 4 && -n "$1" && -n "$2" && "$3" =~ ^[0-9]+$ && "$4" =~ ^(fail|skip)$ ]] || exit 1
if jq -se --arg resource "$1" --arg report "$2" --argjson since "$3" --arg verdict "$4" \
  --argjson now "$(date +%s)" '
  length == 1 and (.[0] | type == "object") and
  (.[0] |
    [.results[]? | select(.policy == "require-owner-label-unevaluated" and .rule == "owner-label")] as $matches |
    .metadata.uid == $report and .scope.uid == $resource and (.results | type) == "array" and
    ($matches | length) == 1 and $matches[0].result == $verdict and
    ($matches[0].timestamp.seconds | type) == "number" and
    $matches[0].timestamp.seconds > 0 and
    $matches[0].timestamp.seconds == ($matches[0].timestamp.seconds | floor) and
    $matches[0].timestamp.seconds >= $since and $matches[0].timestamp.seconds <= $now)
' >/dev/null; then
  exit 0
fi
exit 1
