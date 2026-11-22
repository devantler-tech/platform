#!/usr/bin/env bash
# Executes the committed setup's navigation reconciliation against a fictional
# API. No cluster, credentials or identity accounts are used.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "setup") | .command[2]' \
  "${1:-$repo_root/k8s/providers/hetzner/apps/customer-identity/cron-job-setup.yaml}" |
  awk '/# --- Languages / { selected=1; print; next } selected && /# --- / { selected=0 } selected' >"$scratch/reconcile.sh"
[[ -s $scratch/reconcile.sh ]] || {
  echo 'No setup reconciliation selected' >&2
  exit 1
}

export customers_org_id=fictional-customers
fail() {
  echo "$1" >&2
  exit 1
}
org_label() { printf '%s' "$1"; }
api() {
  local method=$1 route=$2 body=${3:-}
  if [[ $route == /admin/v1/restrictions && $method == GET ]]; then
    printf '%s\n' '{"allowedLanguages":["en"],"disallowPublicOrgRegistration":true}'
    return
  fi
  [[ ${api_org:-} == fictional-customers ]] || fail 'Wrong organization scope'
  case "$method $route" in
    'GET /management/v1/text/login/en') jq -n --slurpfile t "$scratch/texts.json" '{customText:$t[0]}' ;;
    'PUT /management/v1/text/login/en')
      jq -e '.footerText.help == "Back to website (opens in a new tab)"
        and .loginText.description == "The portal is invitation-only."
        and .loginText.userMustBeMemberOfOrg == "Sign in with the account from your invitation email."' <<<"$body" >/dev/null || fail 'Wrong login text'
      printf '%s\n' "$body" >"$scratch/texts.json"
      printf 'texts\n' >>"$scratch/writes"
      ;;
    'GET /management/v1/policies/privacy') cat "$scratch/privacy.json" ;;
    'POST /management/v1/policies/privacy' | 'PUT /management/v1/policies/privacy')
      [[ $method == "$expected_method" ]] || fail 'Wrong create/update method'
      jq -e --argjson body "$body" '.policy | del(.details,.isDefault)
        | .helpLink="https://devantler.tech/" | . == $body' "$scratch/privacy.json" >/dev/null || fail 'Unrelated privacy settings changed'
      jq -n --argjson p "$body" '{policy:($p + {isDefault:false})}' >"$scratch/privacy.json"
      printf 'privacy\n' >>"$scratch/writes"
      ;;
    *) fail "Unexpected API call: $method $route" ;;
  esac
}
reconcile() (
  changes=()
  # shellcheck disable=SC1091
  source "$scratch/reconcile.sh"
  [[ ${#changes[@]} -le 2 ]] || fail 'Unexpected changes'
)

for inherited in true false; do
  expected_method=PUT
  [[ $inherited == false ]] || expected_method=POST
  printf '{}\n' >"$scratch/texts.json"
  : >"$scratch/writes"
  jq -n --argjson inherited "$inherited" '{policy:{isDefault:$inherited,
    details:{sequence:"42"}, tosLink:"https://example.invalid/terms",
    privacyLink:"https://example.invalid/privacy", helpLink:"https://example.invalid/help",
    supportEmail:"support@example.invalid", docsLink:"https://example.invalid/docs",
    customLink:"https://example.invalid/tools", customLinkText:"Tools"}}' >"$scratch/privacy.json"
  reconcile
  jq -e '.policy.helpLink == "https://devantler.tech/"' "$scratch/privacy.json" >/dev/null || fail 'Website link was not installed'
  [[ $(wc -l <"$scratch/writes" | tr -d ' ') == 2 ]] || fail 'Expected one text and one policy write'
  reconcile
  [[ $(wc -l <"$scratch/writes" | tr -d ' ') == 2 ]] || fail 'Repeated reconciliation wrote again'
done

# A refused/malformed read must never be interpreted as an empty policy to write.
for invalid in '{}' '{"policy":null}' '{"policy":{"helpLink":17}}'; do
  printf '%s\n' "$invalid" >"$scratch/privacy.json"
  : >"$scratch/writes"
  if reconcile >"$scratch/refusal.log" 2>&1; then
    fail 'Malformed policy was accepted'
  fi
  [[ ! -s $scratch/writes ]] || fail 'Malformed policy caused a write'
done
printf 'Customer login navigation: create, update, preservation, idempotence and refusal passed\n'
