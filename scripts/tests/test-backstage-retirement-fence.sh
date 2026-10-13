#!/usr/bin/env bash
# Evaluate identity and retained-data controls in the pinned admission engine.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
policy=k8s/providers/hetzner/infrastructure/cluster-policies/restrict-backstage-retirement.yaml
scratch=$(mktemp -d)
trap 'if [[ "${PORTAL_KEEP_NATIVE_FIXTURES:-0}" != 1 ]]; then rm -rf "$scratch"; fi' EXIT
[[ -f "$policy" ]] || {
  printf 'Backstage retirement admission: missing policy\n' >&2
  exit 1
}
record=$(jq -cn '{version:1,phase:"fenced",namespaceUID:"ns-1",sourceSHA:("a"*40),revision:("latest@sha256:"+("b"*64)),run:"42",latestHistoryUID:"history-uid",cnpgWriterUID:"cnpg-controller-uid",helmWriterUID:"helm-writer-uid",
  targets:[{kind:"Deployment",name:"backstage",uid:"deployment-uid"},{kind:"Service",name:"backstage",uid:"service-uid"},{kind:"ConfigMap",name:"backstage-app-config",uid:"configmap-uid"},{kind:"HelmRelease",name:"backstage",uid:"release-uid"},{kind:"HelmChart",name:"backstage-backstage",uid:"chart-uid"}],
  retiringUIDs:["deployment-uid","service-uid","configmap-uid","release-uid","chart-uid","rs-uid","pod-uid"],
  retained:[{kind:"Secret",name:"credential",uid:"credential-uid"},{kind:"Secret",name:"sh.helm.release.v1.backstage.v4",uid:"history-uid"},{kind:"Secret",name:"sh.helm.release.v1.backstage.v3",uid:"old-history-uid"},{kind:"PersistentVolumeClaim",name:"backstage-db-3",uid:"claim-uid"},{kind:"PersistentVolume",name:"retained-volume",uid:"volume-uid"},{kind:"ConfigMap",name:"retained-settings",uid:"settings-uid"},{kind:"ServiceAccount",name:"database-account",uid:"account-uid"},{kind:"Cluster",name:"backstage-db",uid:"database-uid"},{kind:"Secret",name:"backstage-db-ca",uid:"ca-uid",renewCertificate:"ca"},{kind:"Secret",name:"backstage-db-server",uid:"server-uid",renewCertificate:"leaf"},{kind:"Secret",name:"backstage-db-replication",uid:"replication-uid",renewCertificate:"leaf"}] }')
deployment='{apiVersion:"apps/v1",kind:"Deployment",metadata:{name:"backstage",namespace:"backstage",uid:"deployment-uid"},spec:{replicas:1}}'
secret='{apiVersion:"v1",kind:"Secret",metadata:{name:"credential",namespace:"backstage",uid:"credential-uid"},type:"Opaque",data:{value:"SYNTHETIC"}}'
history='{apiVersion:"v1",kind:"Secret",metadata:{name:"sh.helm.release.v1.backstage.v4",namespace:"backstage",uid:"history-uid",labels:{owner:"helm",name:"backstage",version:"4",status:"deployed"}},type:"helm.sh/release.v1",data:{release:"SYNTHETIC"}}'
cases=0
run_case() {
  local name=$1 rule=$2 operation=$3 resource=$4 old=$5 expected=$6
  local journal=${7:-$record} user=${8:-fixture} writer_uid=${9:-fixture-user-uid} code=200
  local dir="$scratch/$name"
  mkdir "$dir"
  cp "$policy" "$dir/policy.yaml"
  jq -cn "$resource" >"$dir/resource.json"
  jq -cn "$old" >"$dir/old.json"
  # Pinned CLI seeds oldObject from the new resource, then recursively merges
  # globalValues. Explicit tombstones prevent an absent old field inheriting a
  # newly added value. These fixtures do not claim absent-versus-null fidelity.
  jq --slurpfile new "$dir/resource.json" '
    def old_evidence($candidate):
      if type=="object" and ($candidate|type)=="object" then
        reduce ($candidate|keys[]) as $key (.;
          if has($key) then .[$key] |= old_evidence($candidate[$key])
          else .[$key]=null end)
      else . end;
    old_evidence($new[0])' "$dir/old.json" >"$dir/effective-old.json"
  jq -n --arg op "$operation" --slurpfile old "$dir/effective-old.json" '{apiVersion:"cli.kyverno.io/v1alpha1",kind:"Values",metadata:{name:"retirement"},globalValues:{"request.operation":$op,"request.oldObject":$old[0]}}' >"$dir/values.json"
  jq -n --arg user "$user" --arg uid "$writer_uid" '{apiVersion:"cli.kyverno.io/v1alpha1",kind:"UserInfo",metadata:{name:"retirement"},userInfo:{username:$user,uid:$uid,groups:(if $user=="admin" then ["system:masters"] else ["system:serviceaccounts:flux-system"] end)}}' >"$dir/userinfo.json"
  case "$journal" in 403 | 404 | 500) code=$journal ;; esac
  jq -n --arg journal "$journal" '{apiVersion:"v1",kind:"NamespaceList",metadata:{},items:(if $journal=="absent" then [] else [{apiVersion:"v1",kind:"Namespace",metadata:{name:"backstage",uid:"ns-1",annotations:(if $journal=="unfenced" then {} else {"platform.devantler.tech/backstage-retirement":$journal} end)}}] end)}' >"$dir/namespace.json"
  jq -n --slurpfile resource "$dir/resource.json" --slurpfile ns "$dir/namespace.json" --arg rule "$rule" --arg op "$operation" --arg expected "$expected" --argjson code "$code" '{apiVersion:"cli.kyverno.io/v1alpha1",kind:"Test",metadata:{name:"retirement"},policies:["policy.yaml"],resources:["resource.json"],variables:"values.json",userinfo:"userinfo.json",
    apiCallResponses:[{urlPath:"/api/v1/namespaces?fieldSelector=metadata.name%3Dbackstage",method:"GET",response:{statusCode:$code,body:$ns[0]}}],
    results:[{kind:$resource[0].kind,policy:"restrict-backstage-retirement",rule:$rule,resources:[(($resource[0].metadata.namespace // "")+"/"+$resource[0].metadata.name|ltrimstr("/"))],operation:$op,result:$expected}]}' >"$dir/kyverno-test.yaml"
  cases=$((cases + 1))
}
run_case captured-delete fence-native-targets DELETE "$deployment" "$deployment" pass
run_case replacement-delete fence-native-targets DELETE "$deployment | .metadata.uid=\"replacement\"" "$deployment | .metadata.uid=\"replacement\"" fail
run_case missing-delete-identity fence-native-targets DELETE "$deployment | del(.metadata.uid)" "$deployment | del(.metadata.uid)" fail
run_case recreate-target fence-native-targets CREATE "$deployment" '{}' fail
run_case target-spec-change fence-native-targets UPDATE "$deployment | .spec.replicas=2" "$deployment" fail
run_case native-finalizer-cleanup fence-native-targets UPDATE "$deployment | .metadata.finalizers=[]" "$deployment" pass
run_case ordinary-target fence-native-targets CREATE "$deployment" '{}' skip unfenced
run_case namespace-not-created fence-native-targets CREATE "$deployment" '{}' skip absent
run_case foreign-namespace-identity fence-native-targets DELETE "$deployment" "$deployment" fail "$(jq -c '.namespaceUID="foreign"' <<<"$record")"
run_case incomplete-journal fence-native-targets DELETE "$deployment" "$deployment" fail '{}'
run_case malformed-journal fence-native-targets DELETE "$deployment" "$deployment" error '{broken'
run_case failed-lookup fence-native-targets DELETE "$deployment" "$deployment" error 500
run_case denied-lookup fence-native-targets DELETE "$deployment" "$deployment" error 403
run_case retained-delete preserve-retained-identities DELETE "$secret" "$secret" fail
run_case retained-recreate preserve-retained-identities CREATE "$secret" '{}' fail
run_case retained-data-change preserve-retained-identities UPDATE "$secret | .data.value=\"changed\"" "$secret" fail
run_case retained-noop preserve-retained-identities UPDATE "$secret" "$secret" pass
run_case retained-string-data-change preserve-retained-identities UPDATE "$secret | .stringData={value:\"changed\"}" "$secret" fail
run_case retained-secret-immutable-change preserve-retained-identities UPDATE "$secret | .immutable=true" "$secret" fail
config='{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"retained-settings",namespace:"backstage",uid:"settings-uid"},data:{setting:"SYNTHETIC"},binaryData:{setting:"U1lOVEhFVElD"}}'
run_case retained-binary-data-change preserve-retained-identities UPDATE "$config | .binaryData.setting=\"Y2hhbmdlZA==\"" "$config" fail
run_case retained-config-immutable-change preserve-retained-identities UPDATE "$config | .immutable=true" "$config" fail
run_case retained-config-noop preserve-retained-identities UPDATE "$config" "$config" pass
account='{apiVersion:"v1",kind:"ServiceAccount",metadata:{name:"database-account",namespace:"backstage",uid:"account-uid"},automountServiceAccountToken:false,imagePullSecrets:[{name:"registry"}],secrets:[{name:"token"}]}'
run_case retained-token-mount-change preserve-retained-identities UPDATE "$account | .automountServiceAccountToken=true" "$account" fail
run_case retained-pull-secret-change preserve-retained-identities UPDATE "$account | .imagePullSecrets=[{name:\"replacement\"}]" "$account" fail
run_case retained-token-reference-change preserve-retained-identities UPDATE "$account | .secrets=[{name:\"replacement\"}]" "$account" fail
run_case retained-account-noop preserve-retained-identities UPDATE "$account" "$account" pass
run_case retained-owner-change preserve-retained-identities UPDATE "$secret | .metadata.ownerReferences=[{apiVersion:\"apps/v1\",kind:\"Deployment\",name:\"backstage\",uid:\"deployment-uid\"}]" "$secret" fail
run_case old-history-change preserve-retained-identities UPDATE "$history | .metadata.name=\"sh.helm.release.v1.backstage.v3\" | .metadata.uid=\"old-history-uid\" | .data.release=\"changed\"" "$history | .metadata.name=\"sh.helm.release.v1.backstage.v3\" | .metadata.uid=\"old-history-uid\"" fail
run_case native-history-start preserve-retained-identities UPDATE "$history | .metadata.labels.status=\"uninstalling\" | .data.release=\"native\"" "$history" pass "$record" system:serviceaccount:flux-system:helm-controller helm-writer-uid
run_case native-history-replaced-writer preserve-retained-identities UPDATE "$history | .metadata.labels.status=\"uninstalling\" | .data.release=\"native\"" "$history" fail "$record" system:serviceaccount:flux-system:helm-controller replacement-writer
run_case native-history-unbound-writer preserve-retained-identities UPDATE "$history | .metadata.labels.status=\"uninstalling\" | .data.release=\"native\"" "$history" fail "$(jq -c 'del(.helmWriterUID)' <<<"$record")" system:serviceaccount:flux-system:helm-controller helm-writer-uid
run_case native-history-string-data-change preserve-retained-identities UPDATE "$history | .metadata.labels.status=\"uninstalling\" | .stringData={release:\"changed\"}" "$history" fail "$record" system:serviceaccount:flux-system:helm-controller helm-writer-uid
run_case foreign-history-writer preserve-retained-identities UPDATE "$history | .metadata.labels.status=\"uninstalling\" | .data.release=\"native\"" "$history" fail
run_case native-history-finish preserve-retained-identities UPDATE "$history | .metadata.labels.status=\"uninstalled\" | .data.release=\"native\"" "$history | .metadata.labels.status=\"uninstalling\"" pass "$record" system:serviceaccount:flux-system:helm-controller helm-writer-uid
run_case history-lifecycle-reversal preserve-retained-identities UPDATE "$history" "$history | .metadata.labels.status=\"uninstalled\"" fail "$record" system:serviceaccount:flux-system:helm-controller helm-writer-uid
run_case new-history-revision preserve-retained-identities CREATE "$history | .metadata.name=\"sh.helm.release.v1.backstage.v5\" | .metadata.uid=\"new-history\" | .metadata.labels.version=\"5\"" '{}' fail
run_case alternate-history-name preserve-retained-identities CREATE "$history | .metadata.name=\"alternate-history\" | .metadata.uid=\"new-history\"" '{}' fail
run_case other-release-storage preserve-retained-identities CREATE "$history | .metadata.name=\"sh.helm.release.v1.other.v4\" | .metadata.labels.name=\"other\" | .metadata.uid=\"other-history\"" '{}' skip
cnpg_writer=system:serviceaccount:cnpg-system:cloudnative-pg
ca='{apiVersion:"v1",kind:"Secret",metadata:{name:"backstage-db-ca",namespace:"backstage",uid:"ca-uid",ownerReferences:[{apiVersion:"postgresql.cnpg.io/v1",kind:"Cluster",name:"backstage-db",uid:"database-uid",controller:true}]},type:"Opaque",data:{"ca.crt":"OLD_PUBLIC_CERTIFICATE","ca.key":"UNCHANGED_PRIVATE_KEY"}}'
run_case cnpg-ca-renewal preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$ca" pass "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-ca-foreign-writer preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$ca" fail "$record" fixture cnpg-controller-uid
run_case cnpg-ca-replaced-writer preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$ca" fail "$record" "$cnpg_writer" replacement-controller-uid
run_case cnpg-ca-private-key-change preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .data[\"ca.key\"]=\"REPLACED_PRIVATE_KEY\"" "$ca" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-ca-extra-data preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .data.password=\"ADDED\"" "$ca" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-ca-empty-certificate preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"\"" "$ca" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-ca-owner-change preserve-retained-identities UPDATE "$ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.ownerReferences[0].uid=\"foreign-database\"" "$ca" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-ca-replacement preserve-retained-identities UPDATE "$ca | .metadata.uid=\"replacement-ca\" | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$ca" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-ca-delete preserve-retained-identities DELETE "$ca" "$ca" fail "$record" "$cnpg_writer" cnpg-controller-uid
for certificate in server replication; do
  leaf="{apiVersion:\"v1\",kind:\"Secret\",metadata:{name:\"backstage-db-$certificate\",namespace:\"backstage\",uid:\"$certificate-uid\",ownerReferences:[{apiVersion:\"postgresql.cnpg.io/v1\",kind:\"Cluster\",name:\"backstage-db\",uid:\"database-uid\",controller:true}]},type:\"kubernetes.io/tls\",data:{\"tls.crt\":\"OLD_PUBLIC_CERTIFICATE\",\"tls.key\":\"UNCHANGED_PRIVATE_KEY\"}}"
  run_case "cnpg-$certificate-renewal" preserve-retained-identities UPDATE "$leaf | .data[\"tls.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$leaf" pass "$record" "$cnpg_writer" cnpg-controller-uid
  run_case "cnpg-$certificate-key-change" preserve-retained-identities UPDATE "$leaf | .data[\"tls.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .data[\"tls.key\"]=\"REPLACED_PRIVATE_KEY\"" "$leaf" fail "$record" "$cnpg_writer" cnpg-controller-uid
  run_case "cnpg-$certificate-foreign-writer" preserve-retained-identities UPDATE "$leaf | .data[\"tls.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$leaf" fail "$record" fixture cnpg-controller-uid
done
record=$(jq -c '.rootCAWriterUID="root-ca-writer-uid" | .trustWriterUID="trust-writer-uid" | .retained += [{kind:"ConfigMap",name:"kube-root-ca.crt",uid:"root-ca-uid",controllerProfile:"root-ca"},{kind:"ConfigMap",name:"cloudflare-origin-ca",uid:"trust-bundle-uid",controllerProfile:"trust-bundle"}] | (.retained[] | select(.kind=="PersistentVolumeClaim" and .name=="backstage-db-3")).controllerProfile="cnpg-role"' <<<"$record")
claim='{apiVersion:"v1",kind:"PersistentVolumeClaim",metadata:{name:"backstage-db-3",namespace:"backstage",uid:"claim-uid",labels:{role:"replica","cnpg.io/instanceRole":"replica","cnpg.io/cluster":"backstage-db"}},spec:{volumeName:"retained-volume"}}'
run_case cnpg-role-change preserve-retained-identities UPDATE "$claim | .metadata.labels.role=\"primary\" | .metadata.labels[\"cnpg.io/instanceRole\"]=\"primary\"" "$claim" pass "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-role-foreign-writer preserve-retained-identities UPDATE "$claim | .metadata.labels.role=\"primary\" | .metadata.labels[\"cnpg.io/instanceRole\"]=\"primary\"" "$claim" fail
run_case cnpg-role-unrelated-label preserve-retained-identities UPDATE "$claim | .metadata.labels.role=\"primary\" | .metadata.labels[\"cnpg.io/instanceRole\"]=\"primary\" | .metadata.labels[\"cnpg.io/cluster\"]=\"other\"" "$claim" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-role-invalid preserve-retained-identities UPDATE "$claim | .metadata.labels.role=\"arbitrary\" | .metadata.labels[\"cnpg.io/instanceRole\"]=\"arbitrary\"" "$claim" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-role-binding-change preserve-retained-identities UPDATE "$claim | .metadata.labels.role=\"primary\" | .metadata.labels[\"cnpg.io/instanceRole\"]=\"primary\" | .spec.volumeName=\"different-volume\"" "$claim" fail "$record" "$cnpg_writer" cnpg-controller-uid
root_ca='{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"kube-root-ca.crt",namespace:"backstage",uid:"root-ca-uid",annotations:{"kubernetes.io/description":"Root CA certificate used to verify connections to the Kubernetes API server. No other usage is guaranteed across distributions of Kubernetes clusters."}},data:{"ca.crt":"OLD_PUBLIC_CERTIFICATE"}}'
root_writer=system:serviceaccount:kube-system:root-ca-cert-publisher
run_case root-ca-renewal preserve-retained-identities UPDATE "$root_ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$root_ca" pass "$record" "$root_writer" root-ca-writer-uid
run_case root-ca-foreign-writer preserve-retained-identities UPDATE "$root_ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$root_ca" fail
run_case root-ca-replaced-writer preserve-retained-identities UPDATE "$root_ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$root_ca" fail "$record" "$root_writer" replaced-writer-uid
run_case root-ca-extra-data preserve-retained-identities UPDATE "$root_ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .data.password=\"ADDED\"" "$root_ca" fail "$record" "$root_writer" root-ca-writer-uid
run_case root-ca-unrelated-annotation preserve-retained-identities UPDATE "$root_ca | .data[\"ca.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.annotations.unrelated=\"CHANGED\"" "$root_ca" fail "$record" "$root_writer" root-ca-writer-uid
trust='{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"cloudflare-origin-ca",namespace:"backstage",uid:"trust-bundle-uid",annotations:{"trust.cert-manager.io/hash":("a"*64)},labels:{"trust.cert-manager.io/bundle":"cloudflare-origin-ca"},ownerReferences:[{apiVersion:"trust.cert-manager.io/v1alpha1",kind:"Bundle",name:"cloudflare-origin-ca",uid:"bundle-uid",controller:true}]},data:{"ca-certificates.crt":"OLD_PUBLIC_CERTIFICATE"}}'
trust_writer=system:serviceaccount:cert-manager:trust-manager
run_case trust-bundle-renewal preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.annotations[\"trust.cert-manager.io/hash\"]=\"405f75cf9aa4a065670b772a9e9216d620b02d2bd0b264854cc3e8df5f31d746\"" "$trust" pass "$record" "$trust_writer" trust-writer-uid
run_case trust-bundle-wrong-hash preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.annotations[\"trust.cert-manager.io/hash\"]=(\"b\"*64)" "$trust" fail "$record" "$trust_writer" trust-writer-uid
run_case trust-bundle-stale-hash preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\"" "$trust" fail "$record" "$trust_writer" trust-writer-uid
run_case trust-bundle-foreign-writer preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.annotations[\"trust.cert-manager.io/hash\"]=(\"b\"*64)" "$trust" fail
run_case trust-bundle-owner-change preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.ownerReferences[0].uid=\"foreign-bundle\"" "$trust" fail "$record" "$trust_writer" trust-writer-uid
run_case trust-bundle-extra-data preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .data.password=\"ADDED\"" "$trust" fail "$record" "$trust_writer" trust-writer-uid
run_case trust-bundle-unrelated-label preserve-retained-identities UPDATE "$trust | .data[\"ca-certificates.crt\"]=\"NEW_PUBLIC_CERTIFICATE\" | .metadata.labels.unrelated=\"CHANGED\"" "$trust" fail "$record" "$trust_writer" trust-writer-uid
run_case root-ca-description-correction preserve-retained-identities UPDATE "$root_ca | .metadata.annotations[\"kubernetes.io/description\"]=\"Contains a CA bundle that can be used to verify the kube-apiserver when using internal endpoints such as the internal service IP or kubernetes.default.svc. No other usage is guaranteed across distributions of Kubernetes clusters.\"" "$root_ca" pass "$record" "$root_writer" root-ca-writer-uid
run_case root-ca-unexpected-description preserve-retained-identities UPDATE "$root_ca | .metadata.annotations[\"kubernetes.io/description\"]=\"unrelated content\"" "$root_ca" fail "$record" "$root_writer" root-ca-writer-uid
record=$(jq -c '.retained += [{kind:"ConfigMap",name:"cnpg-default-monitoring",uid:"monitoring-uid",controllerProfile:"cnpg-monitoring"}]' <<<"$record")
monitoring='{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"cnpg-default-monitoring",namespace:"backstage",uid:"monitoring-uid",annotations:{"cnpg.io/operatorVersion":"1.30.1"}},data:{queries:"SELECT 1"}}'
run_case cnpg-monitoring-refresh preserve-retained-identities UPDATE "$monitoring | .data.queries=\"SELECT 2\"" "$monitoring" pass "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-monitoring-foreign-writer preserve-retained-identities UPDATE "$monitoring | .data.queries=\"SELECT 2\"" "$monitoring" fail
run_case cnpg-monitoring-extra-data preserve-retained-identities UPDATE "$monitoring | .data.queries=\"SELECT 2\" | .data.password=\"ADDED\"" "$monitoring" fail "$record" "$cnpg_writer" cnpg-controller-uid
run_case cnpg-monitoring-owner-change preserve-retained-identities UPDATE "$monitoring | .data.queries=\"SELECT 2\" | .metadata.ownerReferences=[{kind:\"Cluster\",name:\"other\",uid:\"other\"}]" "$monitoring" fail "$record" "$cnpg_writer" cnpg-controller-uid
consumer='{apiVersion:"helm.toolkit.fluxcd.io/v2",kind:"HelmRelease",metadata:{name:"new-consumer",namespace:"backstage",uid:"new-consumer-uid"},spec:{interval:"5m",chartRef:{kind:"HelmChart",name:"backstage-backstage",namespace:"backstage"}}}'
run_case new-chart-consumer fence-chart-consumers CREATE "$consumer" '{}' fail
run_case foreign-namespace-consumer fence-chart-consumers CREATE "$consumer | .metadata.namespace=\"other\"" '{}' fail
run_case neighboring-chart-consumer fence-chart-consumers CREATE "$consumer | .spec.chartRef.name=\"other\"" '{}' skip
run_case real-consumer-failed-lookup fence-chart-consumers CREATE "$consumer" '{}' error 500
run_case real-consumer-denied-lookup fence-chart-consumers CREATE "$consumer" '{}' error 403
run_case neighboring-chart-failed-lookup fence-chart-consumers CREATE "$consumer | .spec.chartRef.name=\"other\"" '{}' error 500
run_case unbound-neighboring-chart fence-chart-consumers CREATE "$consumer | .spec.chartRef.name=\"other\"" '{}' skip "$(jq -c '.version=2' <<<"$record")"
run_case unbound-neighboring-namespace fence-chart-consumers CREATE "$consumer | .metadata.namespace=\"other\" | .spec.chartRef.name=\"other\"" '{}' skip "$(jq -c '.version=2' <<<"$record")"
run_case unbound-real-chart-consumer fence-chart-consumers CREATE "$consumer" '{}' fail "$(jq -c '.version=2' <<<"$record")"
run_case captured-chart-consumer fence-chart-consumers UPDATE "$consumer | .metadata.name=\"backstage\" | .metadata.uid=\"release-uid\"" "$consumer | .metadata.name=\"backstage\" | .metadata.uid=\"release-uid\"" pass
pv='{apiVersion:"v1",kind:"PersistentVolume",metadata:{name:"retained-volume",uid:"volume-uid"},spec:{persistentVolumeReclaimPolicy:"Retain"}}'
run_case retained-volume-delete preserve-retained-identities DELETE "$pv" "$pv" fail
run_case retained-volume-reclaim-change preserve-retained-identities UPDATE "$pv | .spec.persistentVolumeReclaimPolicy=\"Delete\"" "$pv" fail
run_case unrelated-volume preserve-retained-identities DELETE "$pv | .metadata.name=\"neighbor\"" "$pv | .metadata.name=\"neighbor\"" skip
pod='{apiVersion:"v1",kind:"Pod",metadata:{name:"new-pod",namespace:"backstage",uid:"new-uid",ownerReferences:[{apiVersion:"apps/v1",kind:"ReplicaSet",name:"backstage-rs",uid:"rs-uid"}]}}'
run_case new-retiring-descendant fence-retiring-dependencies CREATE "$pod" '{}' fail
run_case captured-descendant-cleanup fence-retiring-dependencies UPDATE "$pod | .metadata.uid=\"pod-uid\" | .metadata.finalizers=[]" "$pod | .metadata.uid=\"pod-uid\"" pass
run_case database-pod-replacement fence-retiring-dependencies CREATE "$pod | .metadata.ownerReferences=[{apiVersion:\"postgresql.cnpg.io/v1\",kind:\"Cluster\",name:\"backstage-db\",uid:\"database-uid\"}]" '{}' skip
ns="{apiVersion:\"v1\",kind:\"Namespace\",metadata:{name:\"backstage\",uid:\"ns-1\",annotations:{\"platform.devantler.tech/backstage-retirement\":$(jq -cn --arg value "$record" '$value')}}}"
run_case namespace-delete preserve-retirement-journal DELETE "$ns" "$ns" fail
run_case journal-remove preserve-retirement-journal UPDATE "$ns | .metadata.annotations={}" "$ns" fail
run_case journal-unchanged preserve-retirement-journal UPDATE "$ns" "$ns" pass
run_case authorized-journal-maintenance preserve-retirement-journal UPDATE "$ns | .metadata.annotations={}" "$ns" pass "$record" admin
bash scripts/validate-kyverno-fixture-evaluation.sh "$scratch"
printf 'Backstage retirement admission: %s evaluated engine cases passed.\n' "$cases"
