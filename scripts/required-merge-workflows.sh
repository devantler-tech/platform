#!/usr/bin/env bash
# Read the effective rules for main, including inherited organization rules.
# This is a one-shot read. It neither updates a PR nor enqueues one.
set -euo pipefail
unknown() { printf 'Required merge workflows: UNKNOWN — %s\n' "$*" >&2; exit 2; }
pages="$(gh api repos/devantler-tech/platform/rules/branches/main --paginate --slurp)" ||
  unknown 'effective branch rules could not be read'
result="$(jq -ce '
  def valid_path:
    type=="string" and test("^\\.github/workflows/[A-Za-z0-9_./-]+\\.ya?ml$")
    and (contains("/../")|not) and (contains("/./")|not) and (contains("//")|not);
  if type!="array" or length==0 then error("missing rule pages") else . end
  | if all(.[]; type=="array" and all(.[]; type=="object" and (.type|type)=="string"))
    then add else error("malformed rule page") end
  | map(select(.type=="workflows"))
  | if all(.[];
      (.ruleset_id|type)=="number" and .ruleset_id>0
      and (.ruleset_source_type|type)=="string" and (.ruleset_source_type|length)>0
      and (.ruleset_source|type)=="string" and (.ruleset_source|length)>0
      and (.parameters|type)=="object"
      and (.parameters.workflows|type)=="array" and (.parameters.workflows|length)>0
      and all(.parameters.workflows[];
        (.path|valid_path) and (.repository_id|type)=="number" and .repository_id>0
        and (.ref|type)=="string" and (.ref|length)>0))
    then . else error("required-workflow source is incomplete") end
  | [.[] | . as $rule | .parameters.workflows[]
    | {path,repository_id,ref,ruleset_id:$rule.ruleset_id,
       ruleset_source_type:$rule.ruleset_source_type,ruleset_source:$rule.ruleset_source}]
  | unique | sort_by(.path,.repository_id,.ref,.ruleset_id)
' <<< "$pages")" || unknown 'effective workflow rules could not be interpreted'
printf '%s\n' "$result"
