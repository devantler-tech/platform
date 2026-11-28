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
    all(.[]; .metadata.namespace!="customer-identity" or
      (.metadata.name!="business-site-preview-dex" and .metadata.name!="business-site-preview-dex-secrets" and
       .metadata.name!="customer-dex-preview-client" and .metadata.name!="customer-dex-preview-client-v1"))
  ' >/dev/null || fail 'the retired preview client still has a credential projection'

kubectl kustomize "${root_dir}/k8s/clusters/prod" |
  yq -o=json -I=0 'select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization")' |
  jq -es --arg cnpg_ready "has(status.readyInstances) && status.readyInstances >= 1 && has(status.currentPrimary) && status.currentPrimary != ''" '
    def root($name):
      [.[]|select(.metadata.name==$name and .metadata.namespace=="flux-system")] |
      if length==1 then .[0].spec else error("expected exactly one Platform root") end;
    all(.[]; .metadata.name!="business-site-preview") and
    (root("apps") | .wait and .sourceRef=={kind:"OCIRepository",name:"flux-system"} and
      any(.healthCheckExprs[];
        .apiVersion=="postgresql.cnpg.io/v1" and .kind=="Cluster" and
        .current==$cnpg_ready))
  ' >/dev/null || fail 'the production apps layer must verify CNPG readiness and the retired migration root must be absent'

kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps" |
  yq -o=json -I=0 'select(.metadata.namespace == "business-site-dev")' |
  jq -es '
    def spec($kind; $name):
      [.[]|select(.kind==$kind and .metadata.name==$name)] |
      if length==1 then .[0].spec else error("expected exactly one preview resource") end;
    (any(.[]; .kind=="Kustomization" or .kind=="HTTPRoute" or .kind=="Ingress" or .kind=="Deployment" or .kind=="Job" or .kind=="WebApp"
      or .metadata.name=="business-site-preview-deployer")|not) and
    (spec("Cluster";"portal-preview-db")|
      .instances==1 and .enablePDB==false and .enableSuperuserAccess==false and
      .storage.storageClass=="longhorn-wffc" and .bootstrap.initdb.database=="portal_preview" and
      .bootstrap.initdb.owner=="portal_preview_migrator" and
      .managed.roles==[
        {name:"portal_preview_migrator",ensure:"present",login:true,passwordSecret:{name:"portal-preview-db-migrator"}},
        {name:"portal_preview_web",ensure:"present",login:true,inherit:false,passwordSecret:{name:"portal-preview-db-web"}},
        {name:"coroot_monitor",ensure:"present",login:true,inRoles:["pg_monitor"],
          passwordSecret:{name:"portal-preview-db-coroot-monitor"}}] and
      .certificates.serverAltDNSNames==["portal-preview-db-rw.business-site-dev.svc.cluster.local"]) and
    (spec("PushSecret";"portal-preview-db-ca")|
      .selector.secret.name=="portal-preview-db-ca" and
      .data==[{match:{secretKey:"ca.crt",remoteRef:{remoteKey:"apps/business-site-dev/portal-db-ca",property:"ca"}}}]) and
    all(.[];.metadata.name!="business-site-dev-runtime" and .metadata.name!="business-site-dev-portal"
      and .metadata.name!="business-site-dev-migration" and .metadata.name!="business-site-dev-migration-network")
  ' >/dev/null || fail 'the preview database and projected inputs must stay isolated'

kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps" |
  yq -o=json -I=0 '.' |
  jq -es '
    def resource($namespace; $kind; $name):
      [.[]|select(.kind==$kind and .metadata.name==$name and .metadata.namespace==$namespace)] |
      if length==1 then .[0] else error("expected exactly one preview application resource") end;
    (resource("business-site";"ExternalSecret";"business-site-practice-migration")|.spec|
      .secretStoreRef=={name:"openbao",kind:"ClusterSecretStore"} and
      .data==[
        {secretKey:"password",remoteRef:{key:"apps/business-site-dev/portal-migrator",property:"password"}},
        {secretKey:"ca",remoteRef:{key:"apps/business-site-dev/portal-db-ca",property:"ca"}}
      ] and
      .target=={name:"business-site-practice-migration",creationPolicy:"Owner",template:{engineVersion:"v2",data:{
        "database-url":"postgres://portal_preview_migrator:{{ .password | urlquery }}@portal-preview-db-rw.business-site-dev.svc.cluster.local:5432/portal_preview?sslmode=verify-full&sslrootcert=/run/portal-migration/ca.crt",
        "ca.crt":"{{ .ca }}"
      }}}) and
    (resource("business-site";"CiliumNetworkPolicy";"business-site-practice-migration")|.spec=={
      endpointSelector:{matchLabels:{"app.kubernetes.io/name":"business-site-practice-migration"}},
      egress:[
        {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"business-site-dev","cnpg.io/cluster":"portal-preview-db"}}],
         toPorts:[{ports:[{port:"5432",protocol:"TCP"}]}]},
        {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}}],
         toPorts:[{ports:[{port:"53",protocol:"ANY"}],rules:{dns:[{matchName:"portal-preview-db-rw.business-site-dev.svc.cluster.local"}]}}]}
      ]
    }) and
    (resource("business-site";"ExternalSecret";"business-site-runtime")|.spec|
      ([.data[]|select((.remoteRef.key//"")|startswith("apps/business-site-dev/"))])==[
        {secretKey:"portal_preview_web_password",sourceRef:{storeRef:{name:"openbao",kind:"ClusterSecretStore"}},remoteRef:{key:"apps/business-site-dev/portal-web",property:"password"}},
        {secretKey:"portal_preview_database_ca",sourceRef:{storeRef:{name:"openbao",kind:"ClusterSecretStore"}},remoteRef:{key:"apps/business-site-dev/portal-db-ca",property:"ca"}}
      ] and
      .target.template.data["portal-database-ca"]=="{{ .portal_database_ca }}" and
      (.target.template.data["portal-settings"]|split("\n")|map(select(startswith("PORTAL_CLIENT_PREVIEW_"))))==[
        "PORTAL_CLIENT_PREVIEW_DATABASE_URL=postgres://portal_preview_web:{{ .portal_preview_web_password | urlquery }}@portal-preview-db-rw.business-site-dev.svc.cluster.local:5432/portal_preview?sslmode=verify-full",
        "PORTAL_CLIENT_PREVIEW_DATABASE_CA={{ .portal_preview_database_ca | b64enc }}"
      ]) and
    (resource("business-site";"CiliumNetworkPolicy";"business-site-portal-egress")|.spec|
      .endpointSelector.matchLabels=={"app.kubernetes.io/name":"business-site"} and
      [.egress[]|select(any(.toEndpoints[]?;.matchLabels["k8s:io.kubernetes.pod.namespace"]=="business-site-dev"))]==[
        {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"business-site-dev","cnpg.io/cluster":"portal-preview-db"}}],
         toPorts:[{ports:[{port:"5432",protocol:"TCP"}]}]}
      ]) and
    (resource("business-site-dev";"CiliumNetworkPolicy";"portal-preview-db")|.spec|
      .endpointSelector.matchLabels=={"cnpg.io/cluster":"portal-preview-db"} and
      [.ingress[]|select(any(.fromEndpoints[]?;.matchLabels["k8s:io.kubernetes.pod.namespace"]=="business-site"))]==[
        {fromEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"business-site","app.kubernetes.io/name":"business-site"}},
                        {matchLabels:{"k8s:io.kubernetes.pod.namespace":"business-site","app.kubernetes.io/name":"business-site-practice-migration"}}],
         toPorts:[{ports:[{port:"5432",protocol:"TCP"}]}]}
      ]) and
    (resource("customer-identity";"HelmRelease";"customer-dex")|.spec.values|
      all(.envVars[]; .name!="CUSTOMER_PREVIEW_CLIENT_SECRET") and
      all(.config.staticClients[]; .id!="business-site-preview") and
      ([.config.staticClients[]|select(.id=="business-portal")|.redirectURIs]==[["https://portal.devantler.tech/callback"]]))
  ' >/dev/null || fail 'the portal and practice migrator must use separate verified preview database projections'

grep -Fq "'scripts/tests/test-business-disclosure-reader.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must select changes to this contract'
grep -Fq 'bash scripts/tests/test-business-disclosure-reader.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute this contract'

printf 'PASS: rendered business disclosure identity and 29 invalid-payload controls\n'
printf 'PASS: isolated preview readers, create-only credential seeds, and verified-TLS database projections\n'
