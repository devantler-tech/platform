#!/usr/bin/env bash
# Exercise the actual pinned server with the authored datastore, not a second
# mock server. All credentials below are disposable fixtures on network=none.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
name="private-mail-proof-$(basename "$scratch")"
negative="${name}-negative"
mount_name="${name}-mounts"
cleanup() {
  local rc=$? cleanup_rc=0
  trap - EXIT
  # --force tolerates a server that has already exited. A failed removal is
  # never reported as a successful proof; the runner must surface the residue.
  docker rm -fv "$name" "$negative" "${mount_name}-initContainers" "${mount_name}-containers" > /dev/null 2>&1 || cleanup_rc=1
  rm -rf "$scratch"
  if [ "$cleanup_rc" != 0 ] && [ "$rc" = 0 ]; then
    echo 'FAIL: mail proof container cleanup was not confirmed' >&2
    rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT

# Scan preparation and administration explicitly with the same reviewed
# exceptions as the platform gate, including the shared bundle in isolation.
go -C "$root" run ./scripts/generate-kubescape-exceptions -o "$scratch/exceptions.json"
for bundle in private-mail private-mail-administration private-mail-proxy-identity; do
  ksail workload validate "$root/k8s/bases/apps/$bundle" --strict --ignore-missing-schemas=false
  kubectl kustomize "$root/k8s/bases/apps/$bundle" >"$scratch/$bundle.yaml"
  ksail workload scan "$scratch/$bundle.yaml" --no-render --framework nsa,mitre \
    --exceptions "$scratch/exceptions.json" --compliance-threshold 100 \
    --format json -o "$scratch/$bundle-scan.json"
  jq -e '.summaryDetails.complianceScore == 100 and
    (.summaryDetails.frameworks | length == 2)' "$scratch/$bundle-scan.json" >/dev/null

  # Prove each enabled render rejects a real privileged-container regression.
  yq '(select(.kind == "StatefulSet").spec.template.spec.containers[0].securityContext.privileged) = true' \
    "$scratch/$bundle.yaml" >"$scratch/$bundle-unsafe.yaml"
  if ksail workload scan "$scratch/$bundle-unsafe.yaml" --no-render --framework nsa,mitre \
    --exceptions "$scratch/exceptions.json" --compliance-threshold 100 \
    --format json -o "$scratch/$bundle-unsafe.json" >"$scratch/$bundle-unsafe.log" 2>&1; then
    echo "FAIL: the enabled $bundle security scan accepted a privileged container" >&2
    exit 1
  fi
  jq -e '[.summaryDetails.frameworks[].controls["C-0057"].ResourceCounters.failedResources // 0] | any(. > 0)' \
    "$scratch/$bundle-unsafe.json" >/dev/null
done

# An opt-in route is rendered separately from infrastructure's admission rules.
# Evaluate them together: schema validity alone missed an undeclared tile group.
admission="$scratch/administration-admission"
mkdir "$admission"
cp "$scratch/private-mail-administration.yaml" "$admission/mail.yaml"
cp "$root/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-homepage-service-groups.yaml" "$admission/policy.yaml"
cat >"$admission/kustomization.yaml" <<'KUSTOMIZATION'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - mail.yaml
  - policy.yaml
KUSTOMIZATION
ksail workload validate "$admission" --strict --ignore-missing-schemas=false --kyverno-policies
yq '(select(.kind == "HTTPRoute").metadata.annotations."gethomepage.dev/group") = "private-mail-unlisted-group"' \
  "$admission/mail.yaml" >"$scratch/unlisted-group.yaml"
mv "$scratch/unlisted-group.yaml" "$admission/mail.yaml"
if ksail workload validate "$admission" --strict --ignore-missing-schemas=false --kyverno-policies \
  >"$scratch/unlisted-group.log" 2>&1; then
  echo 'FAIL: mail administration admitted an undeclared Homepage group' >&2
  exit 1
fi
grep -Fq 'policy "restrict-homepage-service-groups" rule "declared-service-group" failed for HTTPRoute/private-mail/private-mail' \
  "$scratch/unlisted-group.log"
cp "$scratch/private-mail.yaml" "$scratch/render.yaml"

yq -er 'select(.kind == "ConfigMap" and .metadata.name == "private-mail-datastore").data."config.json"' \
  "$scratch/render.yaml" >"$scratch/config.json"
image="$(yq -er 'select(.kind == "StatefulSet" and .metadata.name == "private-mail").spec.template.spec.containers[0].image' "$scratch/render.yaml")"
jq -e '."@type" == "RocksDb" and .path == "/var/lib/stalwart/data"' "$scratch/config.json" >/dev/null
chmod 644 "$scratch/config.json"
docker pull "$image" >/dev/null

# Exercise the rendered administration mounts with the actual published image
# and OCI runtime before any production rollout. A directory-only host runtime
# cannot reproduce ConfigMap subPath file mounts, so this runs in the existing
# Linux Docker proof alongside the server checks below.
administration_image="$(yq -er 'select(.kind == "StatefulSet").spec.template.spec.containers[0].image' "$scratch/private-mail-administration.yaml")"
mkdir "$scratch/tls"
chmod 755 "$scratch/tls"
printf 'disposable mount fixture\n' > "$scratch/tls/tls.crt"
printf 'disposable mount fixture\n' > "$scratch/tls/tls.key"
chmod 644 "$scratch/tls/"*
docker pull "$administration_image" >/dev/null
for group in initContainers containers; do
  datastore_path="$(GROUP="$group" yq -er 'select(.kind == "StatefulSet").spec.template.spec[strenv(GROUP)][0].volumeMounts[] | select(.name == "datastore").mountPath' "$scratch/private-mail-administration.yaml")"
  datastore_key="$(GROUP="$group" yq -er 'select(.kind == "StatefulSet").spec.template.spec[strenv(GROUP)][0].volumeMounts[] | select(.name == "datastore").subPath' "$scratch/private-mail-administration.yaml")"
  tls_path="$(GROUP="$group" yq -er 'select(.kind == "StatefulSet").spec.template.spec[strenv(GROUP)][0].volumeMounts[] | select(.name == "tls").mountPath' "$scratch/private-mail-administration.yaml")"
  [[ "$datastore_key" == config.json ]] || { echo 'FAIL: the projected datastore key is unavailable' >&2; exit 1; }
  docker run --name "${mount_name}-${group}" --network none --read-only \
    --user 2000:2000 --cpus 1 --memory 512m \
    --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --mount "type=bind,source=$scratch/config.json,target=$datastore_path,readonly" \
    --mount "type=bind,source=$scratch/tls,target=$tls_path,readonly" \
    --entrypoint /bin/sh "$administration_image" -ec \
      'test -r /etc/stalwart/config.json; test -r /etc/stalwart/tls/tls.crt; test -r /etc/stalwart/tls/tls.key; exec /usr/local/bin/stalwart --version' \
      > "$scratch/$group-mount.log" 2>&1
done
printf '%s\n' 'PASS: native administration configuration and TLS file mounts'

run_server() {
  local target=$1
  shift
  docker run --detach --name "$target" --network none --read-only \
    --user 2000:2000 --cpus 1 --memory 512m \
    --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --tmpfs /var/lib/stalwart:rw,uid=2000,gid=2000,mode=0700 \
    --tmpfs /tmp:rw,uid=2000,gid=2000,mode=0700 \
    "$@" "$image" >/dev/null
}
wait_ready() {
  local target=$1 attempt
  for ((attempt = 0; attempt < 30; attempt++)); do
    if docker exec "$target" curl --fail --silent --max-time 2 \
      http://127.0.0.1:8080/healthz/ready >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo 'FAIL: the actual mail preparation server did not become ready' >&2
  return 1
}
run_server "$name" \
  --mount "type=bind,source=$scratch/config.json,target=/etc/stalwart/config.json,readonly" \
  --env STALWART_RECOVERY_MODE=1 \
  --env STALWART_RECOVERY_ADMIN=fixture:private-mail-fixture-only
wait_ready "$name"

docker exec "$name" curl --fail --silent --show-error --max-time 5 \
  --user fixture:private-mail-fixture-only \
  --header 'Content-Type: application/json' \
  --data '{"using":["urn:ietf:params:jmap:core","urn:stalwart:jmap"],"methodCalls":[["x:DataStore/get",{},"proof"]]}' \
  http://127.0.0.1:8080/jmap >"$scratch/api.json"
jq -e '.methodResponses | length == 1 and .[0][0] == "x:DataStore/get"' "$scratch/api.json" >/dev/null
status="$(docker exec "$name" curl --silent --max-time 5 --output /dev/null --write-out '%{http_code}' \
  --user fixture:wrong-fixture-password --header 'Content-Type: application/json' \
  --data '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}' http://127.0.0.1:8080/jmap)"
[ "$status" = 401 ] || { echo 'FAIL: management accepted an incorrect fixture credential' >&2; exit 1; }

for port in 25 143 465 587 993; do
  rc=0
  docker exec "$name" curl --silent --max-time 2 "http://127.0.0.1:$port" >/dev/null 2>&1 || rc=$?
  [ "$rc" = 7 ] || { echo 'FAIL: a mail listener was open or its absence was unproven' >&2; exit 1; }
done
docker logs "$name" >"$scratch/server.log" 2>&1
if grep -Eqi 'temporary administrator|private-mail-fixture-only' "$scratch/server.log"; then
  echo 'FAIL: the supplied administrator credential was exposed or replaced' >&2
  exit 1
fi

# A deliberately empty install without the supplied credential must trigger the
# very log condition above. Capture privately and delete without printing it.
run_server "$negative" --env CONFIG_PATH=/tmp/missing-config.json
wait_ready "$negative"
docker logs "$negative" >"$scratch/negative.log" 2>&1
grep -qi 'temporary administrator' "$scratch/negative.log" || {
  echo 'FAIL: missing-credential control did not reach the generated-password path' >&2
  exit 1
}
echo 'PASS: pinned server accepts authored datastore and supplied authentication; wrong password rejected; no mail listeners or generated log credential; missing-credential control detected.'
