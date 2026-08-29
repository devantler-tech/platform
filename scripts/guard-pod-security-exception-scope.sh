#!/usr/bin/env bash
#
# Fail when the pod-security exception's scope stops matching the mutation it names
# as its compensating control.
#
# #2824 exists because these two scopes drifted apart in silence: the exception
# suppressed C-0016/C-0055 cluster-wide while the Kyverno mutation it cites,
# `add-security-context`, does not run in twelve namespaces. The narrowing made them
# agree — and left that agreement resting on a code comment. Both directions of drift
# read as compliance:
#
#   * a namespace ADDED to the mutation's exclusions but not to the exception ⇒ the
#     exception suppresses controls where nothing injects the fields. That is the
#     original defect, reintroduced.
#   * a namespace REMOVED from the mutation's exclusions but left in the exception ⇒
#     controls stay enforced where the mutation now runs, producing findings that are
#     not real gaps and inviting someone to "fix" them by widening the exception.
#
# Neither shows up as a failing check anywhere else, which is why this guard exists.
#
# 🔴 THE EXCLUSION LIST IS NOT THE ANSWER — READ THE MATCH BLOCKS TOO.
#
# `add-pod-security-context` excludes FIFTEEN namespaces, not twelve: the twelve plus
# cert-manager, keda and opencost. Those three are excluded there only because
# `add-imageuid-pod-security-context` matches them BY NAME and supplies the same
# pod-level fields, so the mutation does reach them and the exception correctly does
# not list them. A guard that compares the exception against the exclusion list alone
# reports three false differences on a correct tree, and the natural way to silence it
# is to add those three to the exception — which is precisely the drift it is meant to
# catch. So the set this guard compares is:
#
#     namespaces the mutation does NOT reach
#       = exclusions(add-pod-security-context) MINUS matches(add-imageuid-pod-security-context)
#
# `add-container-security-context` excludes the twelve directly and is not used to
# derive the set: it supplies container-level fields, while the exception's rationale
# is about the pod-level ones. It is cross-checked instead, because a divergence
# between the two mutation rules is its own drift and would otherwise be invisible.
#
# 🔴 FAIL CLOSED. Every input this guard cannot read is exit 2, never a quiet pass. An
# empty set compared against an empty set is the failure mode that makes a guard look
# green forever, so each of the three lists is required to be non-empty before any
# comparison happens.
#
# Exit codes:
#   0  the two sets are equal
#   1  they diverge — the differing namespaces and their side are named
#   2  cannot check: bad usage, a missing or unparseable file, a rule or expression
#      that has moved, or any list that came back empty

set -uo pipefail

die() {
  printf 'guard-pod-security-exception-scope: %s\n' "$*" >&2
  exit 2
}

[ "$#" -le 2 ] || die "usage: $0 [<policy-file> <exception-file>]"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" ||
  die "could not resolve the repository root"

policy_file="${1:-$repo_root/k8s/bases/infrastructure/cluster-policies/best-practices/add-security-context.yaml}"
exception_file="${2:-$repo_root/k8s/bases/infrastructure/cluster-security-exceptions/pod-security-mutations.yaml}"

[ -f "$policy_file" ] || die "mutation policy '$policy_file' not found"
[ -f "$exception_file" ] || die "exception '$exception_file' not found"

# Each read is asserted separately. A combined pipeline would let one silent empty
# result be absorbed by the next stage, which is the shape this guard fails closed on.
read_list() { # <label> <file> <yq-expression>
  local label="$1" file="$2" expr="$3" out
  out="$(yq -r "$expr" "$file" 2>/dev/null | grep -v '^null$' | sed '/^[[:space:]]*$/d' | sort -u)" ||
    die "could not read $label from '$file' — unparseable YAML, or the expression no longer matches"
  [ -n "$out" ] ||
    die "$label came back EMPTY in '$file'; refusing to compare an empty set, which would pass vacuously"
  printf '%s\n' "$out"
}

pod_rule_exclusions="$(read_list \
  'the add-pod-security-context exclusions' "$policy_file" \
  '.spec.rules[] | select(.name == "add-pod-security-context") | .exclude.any[].resources.namespaces[]')" || exit 2

imageuid_rule_matches="$(read_list \
  'the add-imageuid-pod-security-context matched namespaces' "$policy_file" \
  '.spec.rules[] | select(.name == "add-imageuid-pod-security-context") | .match.any[].resources.namespaces[]')" || exit 2

container_rule_exclusions="$(read_list \
  'the add-container-security-context exclusions' "$policy_file" \
  '.spec.rules[] | select(.name == "add-container-security-context") | .exclude.any[].resources.namespaces[]')" || exit 2

exception_not_in="$(read_list \
  'the exception NotIn namespace set' "$exception_file" \
  '.spec.match.namespaceSelector.matchExpressions[] | select(.key == "kubernetes.io/metadata.name" and .operator == "NotIn") | .values[]')" || exit 2

unreached="$(comm -23 \
  <(printf '%s\n' "$pod_rule_exclusions") \
  <(printf '%s\n' "$imageuid_rule_matches"))"
[ -n "$unreached" ] ||
  die "every excluded namespace is matched by add-imageuid-pod-security-context, leaving nothing for the exception to cover — that is not a state this repository has ever been in, so it is treated as a broken read"

status=0

report_diff() { # <label> <left> <right> <left-name> <right-name>
  local only_left only_right
  only_left="$(comm -23 <(printf '%s\n' "$2") <(printf '%s\n' "$3"))"
  only_right="$(comm -13 <(printf '%s\n' "$2") <(printf '%s\n' "$3"))"
  [ -z "$only_left" ] && [ -z "$only_right" ] && return 0
  printf 'guard-pod-security-exception-scope: %s\n' "$1" >&2
  if [ -n "$only_left" ]; then
    printf '  only in %s:\n' "$4" >&2
    printf '%s\n' "$only_left" | sed 's/^/    - /' >&2
  fi
  if [ -n "$only_right" ]; then
    printf '  only in %s:\n' "$5" >&2
    printf '%s\n' "$only_right" | sed 's/^/    - /' >&2
  fi
  return 1
}

report_diff \
  'the exception no longer covers exactly the namespaces the mutation does not reach' \
  "$unreached" "$exception_not_in" \
  'the mutation-unreached set (add-pod-security-context exclusions minus add-imageuid-pod-security-context matches)' \
  "the exception's NotIn values" || status=1

# A cross-check, not the primary comparison: the two mutation rules exclude the same
# twelve today, and a divergence between them means one of the two field sets is being
# injected where the other is not — which changes what the exception is compensating
# for without touching the exception at all.
report_diff \
  'the two add-security-context rules no longer exclude the same namespaces' \
  "$unreached" "$container_rule_exclusions" \
  'the pod-level unreached set' \
  'the add-container-security-context exclusions' || status=1

if [ "$status" -ne 0 ]; then
  printf 'guard-pod-security-exception-scope: the exception and its compensating mutation have drifted; fix whichever side is wrong rather than widening the exception\n' >&2
  exit 1
fi

printf 'guard-pod-security-exception-scope: OK — %d namespace(s) unreached by the mutation, matched exactly by the exception\n' \
  "$(printf '%s\n' "$unreached" | wc -l | tr -d ' ')"
