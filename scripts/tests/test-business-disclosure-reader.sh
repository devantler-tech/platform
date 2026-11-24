#!/usr/bin/env bash
# Inspect the actual rendered bootstrap payload; never authenticate or read a value.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
binary_dir="$(mktemp -d)"
readonly binary_dir
trap 'rm -f "${binary_dir}/check"; rmdir "${binary_dir}"' EXIT
(cd "${root_dir}" && GOTOOLCHAIN=local go build -o "${binary_dir}/check" ./scripts/check-business-disclosure-reader)

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

rendered="$(kubectl kustomize "${root_dir}/k8s/bases/infrastructure/vault-config")"
config_script="$(yq -r '
  select(.kind == "Job" and .metadata.name == "vault-config") |
  .spec.template.spec.containers[] | select(.name == "vault-config") | .command[-1]
' - <<<"${rendered}")"
if [ -z "${config_script}" ] || [ "${config_script}" = null ]; then
  fail 'missing rendered configuration script'
fi
sh -n <<<"${config_script}"

extract_payload() {
  local start="$1" end="$2"
  awk -v start="${start}" -v end="${end}" '
    $0 == start { count++; capture=1; next }
    capture && $0 == end { complete++; capture=0; next }
    capture { print }
    END { if (count != 1 || complete != 1 || capture) exit 1 }
  ' <<<"${config_script}"
}

role="$(extract_payload "bao write auth/kubernetes/role/business-site - <<'BUSINESS_SITE_ROLE'" BUSINESS_SITE_ROLE)" ||
  fail 'missing or duplicate dedicated typed role write'
policy="$(extract_payload "bao policy write app-business-site-disclosure-readonly - <<'BUSINESS_SITE_POLICY'" BUSINESS_SITE_POLICY)" ||
  fail 'missing or duplicate dedicated policy write'

valid_role() {
  "${binary_dir}/check" role >/dev/null 2>&1 <<<"$1"
}

valid_policy() {
  "${binary_dir}/check" policy >/dev/null 2>&1 <<<"$1"
}

valid_role "${role}" || fail 'role must bind one exact identity and one read-only policy with a bounded lifetime'
valid_policy "${policy}" || fail 'policy must read only the selected disclosure, SMTP credential and billing key entries'

for kind in role policy; do
  if [ "${kind}" = role ]; then value="${role}"; else value="${policy}"; fi
  if "valid_${kind}" "${value}
${value}"; then fail 'multiple JSON documents were accepted'; fi
  if [ "${kind}" = role ]; then
    ambiguous='{"token_max_ttl":0,'"${value#*\{}"
  else
    ambiguous='{"path":{"secret/*":{"capabilities":["read","update"]}},'"${value#*\{}"
  fi
  if "valid_${kind}" "${ambiguous}"; then fail 'duplicate JSON members were accepted'; fi
done

for change in \
  '.bound_service_account_names = ["*"]' \
  '.bound_service_account_namespaces = ["*"]' \
  '.bound_service_account_names += ["another-app"]' \
  '.bound_service_account_namespaces += ["another-app"]' \
  '.bound_service_account_namespace_selector = "{}"' \
  '.token_policies += ["default"]' \
  '.token_no_default_policy = false' \
  '.token_max_ttl = 0' \
  '.token_ttl = 7200' \
  'del(.bound_service_account_namespaces)'; do
  invalid="$(jq "${change}" <<<"${role}")"
  if valid_role "${invalid}"; then fail 'widened role mutation was accepted'; fi
done
for change in \
  '.path = {"secret/data/apps/business-site/*": {"capabilities": ["read"]}}' \
  '.path = {"secret/metadata/apps/business-site/public-disclosure": {"capabilities": ["read", "list"]}}' \
  '.path["secret/data/apps/business-site/public-disclosure"].capabilities += ["update"]' \
  '.path = {"secret/data/apps/another-app/public-disclosure": {"capabilities": ["read"]}}' \
  '.path["secret/data/apps/business-site/other"] = {"capabilities": ["read"]}' \
  '.path["secret/data/apps/business-site/smtp"].capabilities += ["update"]' \
  '.path["secret/metadata/apps/business-site/smtp"] = {"capabilities": ["read", "list"]}' \
  '.path["secret/data/apps/business-site/numina"].capabilities += ["update"]' \
  '.path["secret/data/apps/business-site/stripe"].capabilities += ["create"]' \
  '.path["secret/metadata/apps/business-site/stripe"] = {"capabilities": ["read", "list"]}' \
  'del(.path["secret/data/apps/business-site/numina"])' \
  'del(.path["secret/data/apps/business-site/stripe"])' \
  '.path["auth/token/*"] = {"capabilities": ["read", "update"]}' \
  '.path["auth/token/lookup-self"].capabilities += ["update"]' \
  'del(.path["auth/token/revoke-self"])'; do
  invalid="$(jq "${change}" <<<"${policy}")"
  if valid_policy "${invalid}"; then fail 'widened policy mutation was accepted'; fi
done

check_preview_payload() {
  local kind="$1" command="$2" end="$3" payload
  payload="$(extract_payload "$command - <<'$end'" "$end")" ||
    fail "missing or duplicate $kind bootstrap payload"
  "${binary_dir}/check" "$kind" <<<"$payload" || fail "$kind exceeds its dedicated scope"
}

check_preview_payload preview-role 'bao write auth/kubernetes/role/business-site-preview' BUSINESS_SITE_PREVIEW_ROLE
check_preview_payload preview-dex-role 'bao write auth/kubernetes/role/business-site-preview-dex' BUSINESS_SITE_PREVIEW_DEX_ROLE
check_preview_payload preview-policy 'bao policy write app-business-site-preview-readonly' BUSINESS_SITE_PREVIEW_POLICY
check_preview_payload preview-dex-policy 'bao policy write app-business-site-preview-dex-readonly' BUSINESS_SITE_PREVIEW_DEX_POLICY

preview_seeds="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/vault-seed")"
check_preview_seed() {
  local name="$1" key="$2" property="$3"
  yq -o=json -I=0 '.' <<<"$preview_seeds" |
    jq -es --arg name "$name" --arg key "$key" --arg property "$property" '
      def resource($kind; $name):
        [.[]|select(.kind==$kind and .metadata.name==$name and .metadata.namespace=="openbao")] |
        if length==1 then .[0].spec else error("expected exactly one preview seed resource") end;
      (resource("Password"; $name)|.length==48) and
      (resource("ExternalSecret"; "generated-"+$name)|
        .refreshPolicy=="CreatedOnce" and
        .target=={"creationPolicy":"Owner","name":("generated-"+$name)} and
        .dataFrom==[{sourceRef:{generatorRef:{apiVersion:"generators.external-secrets.io/v1alpha1",kind:"Password",name:$name}}}]) and
      (resource("PushSecret"; "push-"+$name)|
        .updatePolicy=="IfNotExists" and .selector.secret.name==("generated-"+$name) and
        .secretStoreRefs==[{kind:"ClusterSecretStore",name:"openbao"}] and
        .data==[{match:{secretKey:"password",remoteRef:{remoteKey:("apps/business-site-dev/"+$key),property:$property}}}])
    ' >/dev/null || fail "incorrect generated credential wiring for $name"
}

check_preview_seed business-site-preview-oidc oidc client-secret
check_preview_seed business-site-preview-db-migrator portal-migrator password
check_preview_seed business-site-preview-db-web portal-web password

kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps/customer-identity" |
  yq -o=json -I=0 '.' |
  jq -es '
    def resource($kind; $name):
      [.[]|select(.kind==$kind and .metadata.name==$name and .metadata.namespace=="customer-identity")] |
      if length==1 then .[0] else error("expected exactly one preview Dex resource") end;
    (resource("ServiceAccount"; "business-site-preview-dex-secrets")|.automountServiceAccountToken==false) and
    (resource("SecretStore"; "business-site-preview-dex")|.spec.provider.vault=={
      server:"http://openbao-active.openbao.svc.cluster.local:8200",path:"secret",version:"v2",
      auth:{kubernetes:{mountPath:"kubernetes",role:"business-site-preview-dex",
        serviceAccountRef:{name:"business-site-preview-dex-secrets"}}}
    }) and
    (resource("ExternalSecret"; "customer-dex-preview-client")|.spec=={
      refreshInterval:"1h",secretStoreRef:{kind:"SecretStore",name:"business-site-preview-dex"},
      target:{creationPolicy:"Owner",name:"customer-dex-preview-client"},
      data:[{secretKey:"client-secret",remoteRef:{key:"apps/business-site-dev/oidc",property:"client-secret"}}]
    })
  ' >/dev/null || fail 'the preview Dex consumer must read only its dedicated client value'

kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps" |
  yq -o=json -I=0 'select(.metadata.namespace == "business-site-dev")' |
  jq -es '
    def spec($kind; $name):
      [.[]|select(.kind==$kind and .metadata.name==$name)] |
      if length==1 then .[0].spec else error("expected exactly one preview resource") end;
    def paths: [.data[].remoteRef.key] | sort;
    (any(.[]; .kind=="WebApp" or .kind=="HTTPRoute" or .kind=="Deployment")|not) and
    (spec("Cluster";"portal-preview-db")|
      .instances==1 and .enablePDB==false and .enableSuperuserAccess==false and
      .storage.storageClass=="longhorn-wffc" and .bootstrap.initdb.database=="portal_preview" and
      .bootstrap.initdb.owner=="portal_preview_migrator" and
      .managed.roles==[
        {name:"portal_preview_migrator",ensure:"present",login:true,passwordSecret:{name:"portal-preview-db-migrator"}},
        {name:"portal_preview_web",ensure:"present",login:true,inherit:false,passwordSecret:{name:"portal-preview-db-web"}}] and
      .certificates.serverAltDNSNames==["portal-preview-db-rw.business-site-dev.svc.cluster.local"]) and
    (spec("PushSecret";"portal-preview-db-ca")|
      .selector.secret.name=="portal-preview-db-ca" and
      .data==[{match:{secretKey:"ca.crt",remoteRef:{remoteKey:"apps/business-site-dev/portal-db-ca",property:"ca"}}}]) and
    (spec("ExternalSecret";"business-site-dev-migration")|
      paths==["apps/business-site-dev/portal-db-ca","apps/business-site-dev/portal-migrator"] and
      (.target.template.data["database-url"]|startswith("postgres://portal_preview_migrator:") and
        endswith("@portal-preview-db-rw.business-site-dev.svc.cluster.local:5432/portal_preview?sslmode=verify-full&sslrootcert=/run/portal-migration/ca.crt"))) and
    (spec("ExternalSecret";"business-site-dev-runtime")|
      paths==["apps/business-site-dev/oidc","apps/business-site-dev/portal-db-ca","apps/business-site-dev/portal-web"] and
      (.target.template.data["portal-settings"]|split("\n")|map(select(length>0)) as $settings |
        ($settings|map(split("=")[0])|sort)==[
          "PORTAL_CALLBACK_URL","PORTAL_CLIENT_SECRET_PROJECTION_FILE","PORTAL_DATABASE_URL","PORTAL_IDENTITY_PROFILE",
          "PORTAL_OIDC_CLIENT_ID","PORTAL_OIDC_ISSUER","PORTAL_OPERATOR_SUBJECTS"] and
        any($settings[]; .=="PORTAL_IDENTITY_PROFILE=preview") and
        any($settings[]; .=="PORTAL_CALLBACK_URL=https://dev.devantler.tech/callback") and
        any($settings[]; .=="PORTAL_OIDC_CLIENT_ID=business-site-preview") and
        any($settings[]; startswith("PORTAL_DATABASE_URL=postgres://portal_preview_web:") and
          endswith("@portal-preview-db-rw.business-site-dev.svc.cluster.local:5432/portal_preview?sslmode=verify-full&sslrootcert=/run/webapp-inputs/portal-database-ca"))))
  ' >/dev/null || fail 'the preview database and projected inputs must stay isolated'

grep -Fq "'scripts/tests/test-business-disclosure-reader.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must select changes to this contract'
grep -Fq 'bash scripts/tests/test-business-disclosure-reader.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute this contract'

printf 'PASS: rendered business disclosure identity and 29 invalid-payload controls\n'
printf 'PASS: isolated preview readers, create-only credential seeds, and verified-TLS database projections\n'
