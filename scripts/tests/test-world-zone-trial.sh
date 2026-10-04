#!/usr/bin/env bash
# The private zone trial must not broaden the other tenants' trust or daily API access.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
trial="${root}/k8s/providers/hetzner/apps/world-at-ruin"
policy="${root}/k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
# Print the fixed diagnostic in $1 and terminate the contract test with status 1.
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

kubectl kustomize "${trial}" >"${work}/trial.yaml"
yq -o=json -I=0 eval-all '.' "${work}/trial.yaml" | jq -s '.' >"${work}/trial.json"
jq -e '
  all(.[]; (.kind == "Namespace" and .metadata.name == "world-at-ruin") or .metadata.namespace == "world-at-ruin") and
  all(.[]; (.kind | IN("HTTPRoute", "Gateway", "PersistentVolumeClaim", "Cluster", "GameServer", "Fleet")) | not) and
  ([.[] | select(.kind == "ServiceAccount") | .automountServiceAccountToken] == [false]) and
  ([.[] | select(.kind == "NetworkPolicy") | .spec] == [{podSelector: {}, policyTypes: ["Ingress", "Egress"]}]) and
  ([.[] | select(.kind == "Certificate") | .spec] | length == 1) and
  ([.[] | select(.kind == "Certificate") | .spec][0] | .dnsNames == ["zone-trial.devantler.tech"] and .secretName == "world-at-ruin-zone-tls" and .issuerRef.name == "letsencrypt-prod") and
  ([.[] | select(.kind == "Role" and .metadata.name == "world-at-ruin-zone-trial-operator") | .rules] == [[{apiGroups: [""], resources: ["pods/portforward", "pods/exec"], verbs: ["create"]}]]) and
  ([.[] | select(.kind == "RoleBinding" and .metadata.name == "world-at-ruin-zone-trial-operator")][0] | .roleRef.kind == "Role" and .roleRef.name == "world-at-ruin-zone-trial-operator" and .subjects == [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: "oidc:${admin_email}"}]) and
  ([.[] | select(.kind == "SecretStore")][0] | .metadata.name == "world-at-ruin" and .spec.provider.vault.auth.kubernetes.role == "app-world-at-ruin" and .spec.provider.vault.auth.kubernetes.serviceAccountRef.name == "world-at-ruin")
' "${work}/trial.json" >/dev/null || fail 'the trial must remain private, namespaced and narrowly authorized'

# These checks also run against one-field ablations below. Merely finding the
# expected names would still pass a wider quota, posture exception or Vault role.
check_budget() {
  jq -e '
    ([.[] | select(.kind == "ResourceQuota") | .spec.hard] == [{
      "requests.cpu":"500m", "requests.memory":"256Mi",
      "limits.cpu":"1", "limits.memory":"512Mi", "pods":"2",
      "persistentvolumeclaims":"0", "services.loadbalancers":"0", "services.nodeports":"0"
    }]) and
    ([.[] | select(.kind == "LimitRange") | .spec.limits] == [[{
      type:"Container", default:{cpu:"500m",memory:"256Mi"},
      defaultRequest:{cpu:"100m",memory:"64Mi"}
    }]])
  ' "$1" >/dev/null 2>&1 || { printf 'VIOLATION B1: trial resource budget differs\n'; return 1; }
}

# Require the four host budget/cleanup controls to survive interrupted tenant
# finalization in rendered input $1. The operator exec grant remains removable.
check_retirement() {
  jq -e '
    [.[] | select(.metadata.namespace == "world-at-ruin" and
      (.kind | IN("ResourceQuota", "LimitRange", "ServiceAccount", "RoleBinding", "Role"))) |
      select(.metadata.annotations."kustomize.toolkit.fluxcd.io/prune" == "disabled") |
      {kind:.kind,name:.metadata.name}] | sort_by(.kind,.name) == [
        {kind:"LimitRange",name:"zone-trial"},
        {kind:"ResourceQuota",name:"zone-trial"},
        {kind:"RoleBinding",name:"world-at-ruin"},
        {kind:"ServiceAccount",name:"world-at-ruin"}
      ]
  ' "$1" >/dev/null 2>&1 || { printf 'VIOLATION L1: retained budget or cleanup authority differs\n'; return 1; }
}

# Require namespace-wide host isolation in rendered input $1, including retention
# during tenant removal or rollback; return 1 for any policy or pruning drift.
check_isolation() {
  jq -e '
    [.[] | select(.kind == "NetworkPolicy" and .metadata.namespace == "world-at-ruin") |
      {name:.metadata.name, namespace:.metadata.namespace, spec:.spec,
       prune:.metadata.annotations."kustomize.toolkit.fluxcd.io/prune"}] == [{
        name:"default-deny", namespace:"world-at-ruin",
        spec:{podSelector:{},policyTypes:["Ingress","Egress"]}, prune:"disabled"
      }]
  ' "$1" >/dev/null 2>&1 || { printf 'VIOLATION N1: retained namespace isolation differs\n'; return 1; }
}

# Public game packages need no namespace registry credential. Reject both
# materializing the shared credential and references to any registry Secret.
check_public_registry() {
  jq -e '
    all(.[]; (.kind != "Secret") and
      (.kind != "ExternalSecret" or .metadata.name != "ghcr-auth")) and
    ([.[] | select(.kind == "ServiceAccount") | (.imagePullSecrets // [])] == [[]]) and
    ([.[] | select(.kind == "OCIRepository") | (.spec.secretRef // null)] == [null])
  ' "$1" >/dev/null 2>&1 || { printf 'VIOLATION R1: trial registry credential boundary differs\n'; return 1; }
}

# Inspect the exception JSON in $1; return 1 unless its identities and control are exact.
check_exception() {
  jq -e '
    .apiVersion == "kubescape.io/v1beta1" and .kind == "ClusterSecurityException" and
    .metadata.name == "world-at-ruin-trial-operator" and
    (.metadata.namespace // "") == "" and
    .spec.posture == [{controlID:"C-0002",action:"ignore"}] and
    .spec.match == {resources:[
      {apiGroup:"rbac.authorization.k8s.io",kind:"RoleBinding",name:"^world-at-ruin-zone-trial-operator$"},
      {apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"^world-at-ruin-zone-trial-operator$"}
    ]}
  ' "$1" >/dev/null 2>&1 || { printf 'VIOLATION C1: trial exception scope differs\n'; return 1; }
}

# Extract the rendered Job's bounded policy payload, like the existing OpenBao
# OIDC contract. Never execute the Job or call bao. Accept only the reviewed HCL
# subset (two literal paths with JSON-compatible capability arrays); unfamiliar
# grammar fails closed instead of being partly inspected.
check_vault_policy() {
  local payload rows
  payload="$(awk '
    /^[[:space:]]*bao policy write app-world-at-ruin - <<\047POLICY\047[[:space:]]*$/ {
      captures++; capturing=1; next
    }
    capturing && /^[[:space:]]*POLICY[[:space:]]*$/ { completed++; capturing=0; next }
    capturing { print }
    END { if (captures != 1 || completed != 1 || capturing) exit 1 }
  ' "$1")" || { printf 'VIOLATION V1: dedicated Vault policy is unavailable\n'; return 1; }
  rows="$(awk '
    {
      line=$0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
      if (line == "" || line ~ /^#/) next
      if (state == 0 && line ~ /^path "[^"\\]+"[[:space:]]*\{$/) {
        path=line; sub(/^path "/, "", path); sub(/"[[:space:]]*\{$/, "", path); state=1
      } else if (state == 1 && line ~ /^capabilities[[:space:]]*=/) {
        capabilities=line; sub(/^capabilities[[:space:]]*=[[:space:]]*/, "", capabilities); state=2
      } else if (state == 2 && line == "}") {
        print path "\t" capabilities; count++; state=0
      } else { bad=1; exit 1 }
    }
    END { if (bad || state != 0 || count != 2) exit 1 }
  ' <<<"${payload}")" || { printf 'VIOLATION V1: dedicated Vault policy grammar differs\n'; return 1; }
  jq -R -s -e '
    split("\n") | map(select(length > 0) | split("\t") |
      {path:.[0], capabilities:(.[1] | fromjson | sort)}) | sort_by(.path) == [
      {path:"secret/data/apps/world-at-ruin/*",capabilities:["create","read","update"]},
      {path:"secret/metadata/apps/world-at-ruin/*",capabilities:["create","read","update"]}
    ]
  ' <<<"${rows}" >/dev/null 2>&1 || { printf 'VIOLATION V1: dedicated Vault KV bounds differ\n'; return 1; }
}

# Inspect the rendered shell payload in $1 without executing it; return 1 on any
# missing, duplicate or widened tenant identity, policy assignment or lifetime.
check_vault_role() {
  local assignments
  assignments="$(awk '
    /^[[:space:]]*bao write auth\/kubernetes\/role\/app-world-at-ruin([[:space:]]|$)/ {
      captures++; capturing=1; line=$0
      sub(/^[[:space:]]*bao write auth\/kubernetes\/role\/app-world-at-ruin[[:space:]]*/, "", line)
    }
    capturing {
      if (line == "") line=$0
      continued=sub(/\\[[:space:]]*$/, "", line)
      sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
      if (line != "") print line
      line=""
      if (!continued) { completed++; capturing=0 }
    }
    END { if (captures != 1 || completed != 1 || capturing) exit 1 }
  ' "$1")" || { printf 'VIOLATION V2: dedicated Vault role is unavailable\n'; return 1; }
  jq -R -s -e '
    split("\n") | map(select(length > 0) | capture("^(?<key>[a-z_]+)=(?<value>[^[:space:]]+)$")) |
    sort_by(.key) == [
      {key:"bound_service_account_names",value:"world-at-ruin"},
      {key:"bound_service_account_namespaces",value:"world-at-ruin"},
      {key:"policies",value:"app-world-at-ruin"},
      {key:"ttl",value:"1h"}
    ]
  ' <<<"${assignments}" >/dev/null 2>&1 || { printf 'VIOLATION V2: dedicated Vault identity or policy binding differs\n'; return 1; }
}

yq -o=json '.' "${root}/k8s/bases/infrastructure/cluster-security-exceptions/world-at-ruin-trial-operator.yaml" >"${work}/exception.json"
kubectl kustomize "${root}/k8s/bases/infrastructure/vault-config" >"${work}/vault.yaml"
yq -N -r 'select(.kind == "Job" and .metadata.name == "vault-config") |
  .spec.template.spec.containers[] | select(.name == "vault-config") | .command[-1]' \
  "${work}/vault.yaml" >"${work}/vault.sh"
sh -n "${work}/vault.sh" || fail 'the rendered vault-config command must parse'
check_budget "${work}/trial.json" || fail 'the trial budget control failed'
check_retirement "${work}/trial.json" || fail 'the trial retirement control failed'
check_isolation "${work}/trial.json" || fail 'the trial isolation control failed'
check_public_registry "${work}/trial.json" || fail 'the trial must not receive a registry credential'
check_exception "${work}/exception.json" || fail 'the trial exception control failed'
check_vault_policy "${work}/vault.sh" || fail 'the trial Vault policy control failed'
check_vault_role "${work}/vault.sh" || fail 'the trial Vault role control failed'

ablations=0
# Run checker $1 on fixture $3 and require its exact violation class $2; abort
# if a broken fixture passes or fails for a different reason.
expect_violation() {
  local check="$1" id="$2" fixture="$3" output
  if output="$("${check}" "${fixture}")"; then
    fail "ablation ${id} was not rejected"
  fi
  [[ "${output}" == "VIOLATION ${id}:"* ]] || fail "ablation ${id} tripped another check"
  ablations=$((ablations + 1))
}
# Mutate source JSON $3 with expression $4, then test checker $1/class $2;
# refuse an unchanged fixture so the negative control cannot be vacuous.
ablate_json() {
  local check="$1" id="$2" source="$3" mutation="$4" fixture="${work}/ablation.json"
  jq "${mutation}" "${source}" >"${fixture}"
  cmp -s "${source}" "${fixture}" && fail "ablation ${id} changed nothing"
  expect_violation "${check}" "${id}" "${fixture}"
}
# Mutate the private rendered Vault payload with sed expression $3, then test
# checker $1/class $2; refuse an unchanged fixture and never execute that payload.
ablate_vault() {
  local check="$1" id="$2" mutation="$3" fixture="${work}/ablation.sh"
  sed "${mutation}" "${work}/vault.sh" >"${fixture}"
  cmp -s "${work}/vault.sh" "${fixture}" && fail "ablation ${id} changed nothing"
  expect_violation "${check}" "${id}" "${fixture}"
}
ablate_json check_budget B1 "${work}/trial.json" 'map(if .kind == "ResourceQuota" then .spec.hard."requests.cpu" = "1" else . end)'
ablate_json check_budget B1 "${work}/trial.json" 'map(if .kind == "ResourceQuota" then .spec.hard.persistentvolumeclaims = "1" else . end)'
ablate_json check_budget B1 "${work}/trial.json" 'map(if .kind == "ResourceQuota" then .spec.hard."services.loadbalancers" = "1" else . end)'
ablate_json check_budget B1 "${work}/trial.json" 'map(if .kind == "LimitRange" then .spec.limits[0].default.memory = "512Mi" else . end)'
for kind in ResourceQuota LimitRange ServiceAccount RoleBinding; do
  ablate_json check_retirement L1 "${work}/trial.json" "map(if .kind == \"${kind}\" and .metadata.name != \"world-at-ruin-zone-trial-operator\" then del(.metadata.annotations.\"kustomize.toolkit.fluxcd.io/prune\") else . end)"
done
ablate_json check_isolation N1 "${work}/trial.json" 'map(if .kind == "NetworkPolicy" then del(.metadata.annotations."kustomize.toolkit.fluxcd.io/prune") else . end)'
ablate_json check_isolation N1 "${work}/trial.json" 'map(if .kind == "NetworkPolicy" then .spec.ingress = [{}] else . end)'
ablate_json check_isolation N1 "${work}/trial.json" 'map(if .kind == "NetworkPolicy" then .spec.podSelector = {matchLabels:{trial:"selected-only"}} else . end)'
ablate_json check_isolation N1 "${work}/trial.json" 'map(select(.kind != "NetworkPolicy"))'
ablate_json check_public_registry R1 "${work}/trial.json" 'map(if .kind == "ServiceAccount" then .imagePullSecrets = [{name:"ghcr-auth"}] else . end)'
ablate_json check_public_registry R1 "${work}/trial.json" 'map(if .kind == "OCIRepository" then .spec.secretRef = {name:"ghcr-auth"} else . end)'
ablate_json check_public_registry R1 "${work}/trial.json" '. + [{kind:"ExternalSecret",metadata:{name:"ghcr-auth"}}]'
ablate_json check_exception C1 "${work}/exception.json" '.spec.match.resources[0].name = ".*"'
ablate_json check_exception C1 "${work}/exception.json" '.spec.posture += [{controlID:"C-0015",action:"ignore"}]'
ablate_json check_exception C1 "${work}/exception.json" 'del(.spec.match)'
ablate_vault check_vault_policy V1 's#secret/data/apps/world-at-ruin/\*#secret/data/apps/*#'
ablate_vault check_vault_policy V1 '/bao policy write app-world-at-ruin - <</,/^[[:space:]]*POLICY[[:space:]]*$/s/"create", "update", "read"/"create", "update", "read", "delete"/'
ablate_vault check_vault_policy V1 's#secret/metadata/apps/world-at-ruin/\*#secret/metadata/apps/another-tenant/*#'
ablate_vault check_vault_role V2 's/bound_service_account_names=world-at-ruin/bound_service_account_names=*/'
ablate_vault check_vault_role V2 's/bound_service_account_namespaces=world-at-ruin/bound_service_account_namespaces=*/'
ablate_vault check_vault_role V2 's/policies=app-world-at-ruin/policies=app-world-at-ruin,vault-admin/'
ablate_vault check_vault_role V2 '/bound_service_account_namespaces=world-at-ruin/d'
kubectl kustomize "${root}/k8s/providers/hetzner/apps" >"${work}/prod.yaml"
kubectl kustomize "${root}/k8s/providers/docker/apps" >"${work}/local.yaml"
yq -o=json -I=0 eval-all '.' "${work}/prod.yaml" | jq -s '.' >"${work}/prod.json"
check_isolation "${work}/prod.json" || fail 'the applied prod layer must retain the host isolation policy'
jq '[.[] | select(.metadata.name == "world-at-ruin" or .metadata.namespace == "world-at-ruin")]' "${work}/prod.json" >"${work}/prod-trial.json"
check_retirement "${work}/prod-trial.json" || fail 'the applied prod layer must retain budget and cleanup authority'
for kind in ResourceQuota LimitRange ServiceAccount RoleBinding; do
  ablate_json check_retirement L1 "${work}/prod-trial.json" "map(if .kind == \"${kind}\" and .metadata.name != \"world-at-ruin-zone-trial-operator\" then del(.metadata.annotations.\"kustomize.toolkit.fluxcd.io/prune\") else . end)"
done
check_public_registry "${work}/prod-trial.json" || fail 'the applied prod layer must not supply a registry credential'
if sed -n '/^readonly -a FANOUT_NAMESPACES=(/,/^)/p' "${root}/scripts/refresh-flux-ghcr-auth.sh" | grep -qF '"world-at-ruin"'; then
  fail 'a public-package trial must not participate in registry credential fanout'
fi
[[ "$(yq -N -r 'select(.kind == "Namespace" and .metadata.name == "world-at-ruin") | .metadata.name' "${work}/prod.yaml")" == "world-at-ruin" ]] || fail 'the trial must be present in the prod app layer'
[[ -z "$(yq -N -r 'select(.metadata.name == "world-at-ruin" or .metadata.namespace == "world-at-ruin") | .kind' "${work}/local.yaml")" ]] || fail 'the trial must remain prod-only'

bash "${root}/scripts/tests/test-world-zone-signatures.sh"
subject="$(yq -r '.spec.verify.matchOIDCIdentity[0].subject' "${trial}/oci-repository.yaml")"
[[ "${subject}" == "$(yq -r '.spec.attestors[] | select(.name == "publishwarzone") | .cosign.keyless.identities[0].subjectRegExp' "${policy}")" ]] || fail 'OCI and admission signer identities must agree'
artifact_filter="$(yq -r '.spec.ref.semverFilter' "${trial}/oci-repository.yaml")"
[[ '0.115.1' =~ ${artifact_filter} && ! '0.115.1-rc.1' =~ ${artifact_filter} ]] || fail 'the actual unprefixed stable manifest tags must pass discovery'
[[ "$(yq -r '.spec.ref.semver' "${trial}/oci-repository.yaml")" == '>=0.115.1' ]] || fail 'packages with the tenant-owned standard NetworkPolicy must remain excluded'

printf 'PASS: trial budget, retained cleanup, isolation, public registry, exact exception and Vault bounds; 7 controls + %d ablations\n' "${ablations}"
printf 'PASS: private prod-only zone trial, scoped operator grant and stable release signer boundaries\n'
