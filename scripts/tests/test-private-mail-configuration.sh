#!/usr/bin/env bash
# Native protocol proof using disposable .test accounts and a private network.
set -Eeuo pipefail
trap 'printf "FAIL: native mail proof stopped at line %s\n" "$LINENO" >&2' ERR
umask 077
assert_diagnostic_log_privacy() {
  local log=$1 secret
  shift
  if ! grep -Fq 'server.startup' "$log"; then
    echo 'FAIL: normal serving produced no startup diagnostic event' >&2; return 1
  fi
  for secret in "$@"; do
    if grep -Fq -f "$secret" "$log"; then
      echo 'FAIL: a generated fixture credential appeared in server logs' >&2; return 1
    fi
  done
  if grep -Eqi 'temporary administrator|private-mail-recovery-fixture-only' "$log"; then
    echo 'FAIL: a recovery credential appeared in server logs' >&2; return 1
  fi
  if grep -Eqi '(^|[^[:alnum:]_-])(smtp|imap|jmap|auth|oauth|message|message-ingest|queue|delivery|http|websocket)\.[[:alnum:]_-]+' "$log"; then
    echo 'FAIL: excluded message or authentication events appeared in server logs' >&2; return 1
  fi
  if grep -Fq -e 'Native policy proof' -e 'mail-policy@example.test' -e 'Preserved native mail fixture.' -e 'Human submission proof' -e 'Receiving continuity' -e 'Native client retention proof' -e 'private-canary' "$log"; then
    echo 'FAIL: private fixture message content appeared in server logs' >&2; return 1
  fi
}
test_diagnostic_log_privacy() (
  local proof
  proof=$(mktemp -d)
  trap 'rm -rf "$proof"' EXIT
  printf 'server.startup\n' >"$proof/log"
  assert_diagnostic_log_privacy "$proof/log"
  printf 'message-ingest.ham message not in the listed canaries\n' >>"$proof/log"
  if assert_diagnostic_log_privacy "$proof/log" >"$proof/result" 2>&1; then
    echo 'FAIL: diagnostic privacy check accepted an unlisted message-ingest event' >&2
    exit 1
  fi
  grep -Fq 'excluded message or authentication events' "$proof/result"
  printf 'server.startup\nprivate-canary\n' >"$proof/log"
  if assert_diagnostic_log_privacy "$proof/log" >"$proof/result" 2>&1; then
    echo 'FAIL: diagnostic privacy check accepted a listed message canary' >&2
    exit 1
  fi
  grep -Fq 'private fixture message content' "$proof/result"
  echo 'PASS: diagnostic log privacy controls'
)
test_diagnostic_log_privacy
if [ "${1:-}" = --test-log-privacy ]; then exit 0; fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture_user="$(id -u):$(id -g)"
[ "$(id -u)" != 0 ] || { echo 'FAIL: native mail proof requires an unprivileged fixture user' >&2; exit 1; }
scratch="$(mktemp -d)"
name="mail-config-$(basename "$scratch" | tr '[:upper:]' '[:lower:]')"
network="${name}-network"
proxy_name="${name}-client-proxy"
proxy_started=0
server='ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c'
cli='ghcr.io/stalwartlabs/cli:1.0.13@sha256:9a1e07307d6f890a193322c4b737fc0868405f0f8486a19922cbf362f43884fd'
cleanup() {
  local rc=$? cleanup_rc=0
  trap - EXIT
  if [ "$proxy_started" = 1 ]; then
    docker rm -fv "$proxy_name" > /dev/null 2>&1 || cleanup_rc=1
  fi
  docker rm -fv "$name" > /dev/null 2>&1 || cleanup_rc=1
  docker network rm "$network" > /dev/null 2>&1 || cleanup_rc=1
  docker image rm "$name-bootstrap-image" > /dev/null 2>&1 || cleanup_rc=1
  rm -rf "$scratch"
  if [ "$rc" = 0 ] && [ "$cleanup_rc" != 0 ]; then
    echo 'FAIL: native mail proof cleanup was not confirmed' >&2
    rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT
mkdir -p "$scratch/inputs" "$scratch/data" "$scratch/tmp" "$scratch/etc/tls" "$scratch/cli-home"
chmod 700 "$scratch/data" "$scratch/tmp"
chmod 755 "$scratch/etc" "$scratch/etc/tls"
printf '%s' '{"@type":"RocksDb","path":"/var/lib/stalwart/data","bufferSize":33554432,"cacheSize":33554432}' >"$scratch/etc/config.json"
printf '%s' '{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator","maintainerAliases":["former-a","former-b"]}' >"$scratch/inputs/settings.json"
for account in administrator maintainer agent; do
  openssl rand -hex 32 | tr -d '\n' >"$scratch/inputs/$account-password"
done
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj '/CN=mail.example.test' \
  -addext 'subjectAltName=DNS:mail.example.test' -keyout "$scratch/etc/tls/tls.key" \
  -out "$scratch/etc/tls/tls.crt" >"$scratch/certificate.log" 2>&1
chmod 644 "$scratch/etc/config.json" "$scratch/etc/tls/"*
docker pull "$server" >/dev/null
docker pull "$cli" >/dev/null
docker network create --internal "$network" >/dev/null
mkdir "$scratch/build"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$root" build -trimpath -ldflags='-s -w' -o "$scratch/build/private-mail-bootstrap" ./scripts/private-mail-config
cp "$root/scripts/private-mail-config/Dockerfile" "$scratch/build/Dockerfile"
bash "$root/scripts/private-mail-config/stage-webui.sh" "$scratch/build"
docker build --tag "$name-bootstrap-image" "$scratch/build" >"$scratch/build.log" 2>&1
[ "$(docker image inspect "$name-bootstrap-image" --format '{{.Config.User}}')" = '2000:2000' ] || {
  echo 'FAIL: bootstrap image lost its unprivileged deployment identity' >&2; exit 1;
}
docker run --rm --network none --read-only --user 2000:2000 --cap-drop ALL \
  --security-opt no-new-privileges:true --entrypoint /bin/sh "$name-bootstrap-image" \
  -ec 'sha256sum /usr/local/share/stalwart/webui-*.zip; test -r /usr/local/share/licenses/stalwart-webui/LICENSE; test -r /usr/local/share/licenses/stalwart-webui/SOURCE; test -r /usr/local/share/licenses/caddy/LICENSE' >"$scratch/image-assets.txt"
[ "$(cut -d ' ' -f 1 "$scratch/image-assets.txt")" = "$(jq -er .sha256 "$root/scripts/private-mail-config/webui-pins.json")" ]
docker run --rm --network none --read-only --user 2000:2000 --cap-drop ALL \
  --security-opt no-new-privileges:true --entrypoint /usr/local/bin/caddy \
  "$name-bootstrap-image" version >"$scratch/client-proxy-version.txt"
grep -Eq '^v2\.11\.7([[:space:]]|$)' "$scratch/client-proxy-version.txt"

run_bootstrap() {
  local bootstrap_status=0 bootstrap_stage
  # Bind-mounted files belong to the invoking unprivileged user. Keep one
  # identity through bootstrap and every restart, as the deployment requires.
  docker run --rm --name "$name-bootstrap" --network "$network" --read-only \
    --user "$fixture_user" --cpus 1 --memory 512m --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --mount "type=bind,source=$scratch/etc,target=/etc/stalwart,readonly" \
    --mount "type=bind,source=$scratch/inputs,target=/etc/private-mail,readonly" \
    --mount "type=bind,source=$scratch/data,target=/var/lib/stalwart" \
    --mount "type=bind,source=$scratch/tmp,target=/tmp" \
    "$name-bootstrap-image" "$@" >"$scratch/bootstrap.log" 2>&1 || bootstrap_status=$?
  if [ "$bootstrap_status" != 0 ]; then
    # Emit fixed stage labels only. Native diagnostics and fixture credentials
    # stay in the private scratch directory, never the hosted job output.
    for bootstrap_stage in 'initial inventory' 'native apply' 'policy readback'; do
      if grep -Fq "$bootstrap_stage:" "$scratch/bootstrap.log"; then
        printf 'NATIVE_BOOTSTRAP_FAILURE_STAGE=%s\n' "$bootstrap_stage" >&2
      fi
    done
    printf 'NATIVE_BOOTSTRAP_FAILURE_CALLER=%s EXIT=%s\n' "${BASH_LINENO[0]}" "$bootstrap_status" >&2
  fi
  return "$bootstrap_status"
}

start_server() {
  local mode=$1 health_port=8080
  local -a recovery=() application_assets=()
  if [ "$mode" = preparation ] || [ "$mode" = old-interface-preparation ] || [ "$mode" = isolated-preparation ]; then
    recovery=(--env STALWART_RECOVERY_MODE=1 --env STALWART_RECOVERY_ADMIN=fixture:private-mail-recovery-fixture-only)
  fi
  if [ "$mode" = isolated-preparation ]; then
    health_port=18080
    recovery+=(--env STALWART_RECOVERY_MODE_PORT=18080)
  fi
  if [ "$mode" = old-interface ] || [ "$mode" = old-interface-preparation ]; then
    application_assets=(--mount "type=bind,source=$scratch/stale-assets,target=/usr/local/share/stalwart,readonly")
  fi
  docker run --detach --name "$name" --network "$network" --read-only \
    --user "$fixture_user" --cpus 1 --memory 512m --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --mount "type=bind,source=$scratch/etc,target=/etc/stalwart,readonly" \
    --mount "type=bind,source=$scratch/data,target=/var/lib/stalwart" \
    --mount "type=bind,source=$scratch/tmp,target=/tmp" \
    "${recovery[@]}" "${application_assets[@]}" --entrypoint /usr/local/bin/stalwart "$name-bootstrap-image" --config /etc/stalwart/config.json >/dev/null
  for ((attempt=0;attempt<60;attempt++)); do
    if docker exec "$name" curl --fail --silent --max-time 2 "http://127.0.0.1:$health_port/healthz/ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo 'FAIL: pinned native mail server did not become ready' >&2
  return 1
}
native_cli() {
  docker run --rm --interactive --user "$fixture_user" --network "$network" --cap-drop ALL --security-opt no-new-privileges:true \
    --workdir /proof/cli-home --env HOME=/proof/cli-home --env TMPDIR=/proof/cli-home \
    --env "STALWART_URL=http://$name:8080" --env STALWART_USER=fixture \
    --env STALWART_PASSWORD=private-mail-recovery-fixture-only \
    --mount "type=bind,source=$scratch,target=/proof" "$cli" "$@"
}
administration_cli() (
  # Pass the disposable credential through the environment, never an argv value.
  STALWART_PASSWORD=$(cat "$scratch/inputs/administrator-password")
  export STALWART_PASSWORD
  docker run --rm --interactive --user "$fixture_user" --network "$network" --cap-drop ALL --security-opt no-new-privileges:true \
    --workdir /proof/cli-home --env HOME=/proof/cli-home --env TMPDIR=/proof/cli-home \
    --env "STALWART_URL=http://$name:8080" --env STALWART_USER=operator@example.test --env STALWART_PASSWORD \
    --mount "type=bind,source=$scratch,target=/proof" "$cli" "$@"
)
protocol_curl() {
  # Keep protocol clients in the isolated network. An internal bridge need not
  # expose host port bindings, and a separate client retains non-loopback checks.
  docker run --rm --network "$network" --user "$fixture_user" --cpus 1 --memory 128m \
    --read-only --cap-drop ALL --security-opt no-new-privileges:true --workdir /proof \
    --mount "type=bind,source=$scratch,target=/proof,readonly" \
    --entrypoint curl "$server" --disable "$@"
}
inventory() {
  docker exec -i "$name" curl --fail --silent --show-error --max-time 10 \
    --user fixture:private-mail-recovery-fixture-only --header 'Content-Type: application/json' \
    --data-binary @- http://127.0.0.1:8080/jmap <"$root/scripts/private-mail-config/inventory-request.json" \
    >"$scratch/inputs/listener-inventory.json"
}
verify_administration_assets() {
  local prefix asset
  for prefix in admin account; do
    protocol_curl --fail --silent --show-error --max-time 10 "http://$name:8080/$prefix/login" >"$scratch/$prefix.html"
    grep -Fq "<base href=\"/$prefix/\"" "$scratch/$prefix.html"
    grep -oE '(src|href)="\./assets/[A-Za-z0-9_.-]+\.(js|css)"' "$scratch/$prefix.html" |
      sed -E 's/^(src|href)="\.\///;s/"$//' >"$scratch/$prefix-assets.txt"
    [ -s "$scratch/$prefix-assets.txt" ]
    while IFS= read -r asset; do
      unzip -p "$scratch/build/webui.zip" "$asset" >"$scratch/expected-asset"
      protocol_curl --fail --silent --show-error --max-time 10 "http://$name:8080/$prefix/$asset" >"$scratch/actual-asset"
      cmp "$scratch/expected-asset" "$scratch/actual-asset"
    done <"$scratch/$prefix-assets.txt"
  done
}
verify_receiving_aliases() {
  local human_account alias
  protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl "http://$name:8080/jmap/session" >"$scratch/human-session.json"
  human_account=$(jq -er '.primaryAccounts."urn:ietf:params:jmap:mail"' "$scratch/human-session.json")
  # Receiving into this account is the contract. The fixture retains native
  # spam handling; do not assume an unauthenticated message lands in INBOX or
  # that a native Message-ID header index behaves like a raw substring search.
  jq -cn --arg account "$human_account" '{using:["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],methodCalls:[["Email/query",{accountId:$account,limit:16,calculateTotal:true},"received"],["Email/get",{accountId:$account,"#ids":{resultOf:"received",name:"Email/query",path:"/ids"},properties:["id","messageId","mailboxIds"]},"received-metadata"]]}' >"$scratch/receiving-query.json"
  protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl --header 'Content-Type: application/json' --data-binary @/proof/receiving-query.json "http://$name:8080/jmap" >"$scratch/receiving-result.json"
  jq -e --arg account "$human_account" '
    .methodResponses as $r | ($r|length)==2 and $r[0][0]=="Email/query" and $r[1][0]=="Email/get"
    and $r[0][1].accountId==$account and $r[1][1].accountId==$account and $r[0][1].position==0
    and ($r[0][1].ids|type)=="array" and $r[0][1].total==($r[0][1].ids|length) and $r[0][1].total<=16
    and ($r[0][1].ids|unique|length)==$r[0][1].total and $r[1][1].notFound==[]
    and ($r[1][1].list|map(.id)|sort)==($r[0][1].ids|sort)' "$scratch/receiving-result.json" >/dev/null
  for alias in former-a former-b; do
    jq -Se --arg message "$alias-receiving@example.test" '
      [.methodResponses[1][1].list[]|select(.messageId==[$message])]
      | if length==1 and (.[0].id|type)=="string" and .[0].id!="" and (.[0].mailboxIds|type)=="object" and (.[0].mailboxIds|length)>0
        then .[0]|{id,messageId,mailboxIds} else error("alias message is not uniquely retained") end' "$scratch/receiving-result.json" >"$scratch/$alias-receiving-metadata.json"
  done
}
prepare_plan() {
  local output=$1
  inventory
  go -C "$root" run ./scripts/private-mail-config --input-dir "$scratch/inputs" --output-file "$output"
}
apply_plan() {
  native_cli apply --file /proof/plan.ndjson --json >"$scratch/apply.jsonl" 2>"$scratch/apply.err"
  jq -se '[.[]|select(.status=="error")]|length==0' "$scratch/apply.jsonl" >/dev/null
}
verify_rendered_self_checks() {
  local probe arg status rewritten i
  local bundle=${1:-private-mail-administration} container=${2:-private-mail} target=${3:-$name} loopback=${4:-127.0.0.1:8080}
  local -a check_command=() refused_command=()
  kubectl kustomize "$root/k8s/bases/apps/$bundle" >"$scratch/health-render.yaml"
  CONTAINER_NAME="$container" yq -o=json -I=0 'select(.kind == "StatefulSet" and .metadata.name == "private-mail").spec.template.spec.containers[] | select(.name == strenv(CONTAINER_NAME))' \
    "$scratch/health-render.yaml" >"$scratch/health-container.json"
  for probe in startupProbe readinessProbe livenessProbe; do
    jq -e --arg probe "$probe" '.[$probe].exec.command | type=="array" and length>0 and all(.[];type=="string")' "$scratch/health-container.json" >/dev/null
    check_command=()
    while IFS= read -r -d '' arg; do check_command+=("$arg"); done < <(jq -j --arg probe "$probe" '.[$probe].exec.command[] | ., "\u0000"' "$scratch/health-container.json")
    docker exec "$target" "${check_command[@]}" >"$scratch/$probe.stdout" 2>"$scratch/$probe.stderr"
    [ ! -s "$scratch/$probe.stdout" ] || { echo 'FAIL: a self-check printed response content' >&2; exit 1; }
    refused_command=("${check_command[@]}")
    rewritten=0
    for i in "${!refused_command[@]}"; do
      refused_command[i]="${refused_command[i]//$loopback/127.0.0.1:1}"
      if [[ "${refused_command[i]}" != "${check_command[i]}" ]]; then rewritten=$((rewritten+1)); fi
    done
    [ "$rewritten" = 1 ] || { echo 'FAIL: self-check lacks its unique loopback target' >&2; exit 1; }
    status=0
    docker exec "$target" "${refused_command[@]}" >"$scratch/$probe-refused.stdout" 2>"$scratch/$probe-refused.stderr" || status=$?
    [ "$status" = 7 ] || { echo 'FAIL: the native connection-refusal control was not observed' >&2; exit 1; }
    docker exec "$target" "${check_command[@]}" >"$scratch/$probe-recovered.stdout" 2>"$scratch/$probe-recovered.stderr"
  done
  echo 'PASS: actual rendered self-checks accept native health and reject an unreachable endpoint'
}
run_bootstrap
# The actual pinned server must honor the recovery override while keeping the
# normal Service's port closed. Probe from a separate network client, without
# --fail on the negative: any HTTP response there would show an open endpoint.
start_server isolated-preparation
protocol_curl --fail --silent --show-error --max-time 3 "http://$name:18080/healthz/ready" >/dev/null
if protocol_curl --silent --output /dev/null --max-time 3 "http://$name:8080/healthz/ready"; then
  echo 'FAIL: recovery mode exposed the normal administration port' >&2
  exit 1
fi
echo 'PASS: native recovery management is separate from normal administration'
docker stop "$name" >/dev/null
docker rm "$name" >/dev/null
start_server preparation
prepare_plan "$scratch/plan.ndjson"
apply_plan
native_cli query Account --fields id --json >"$scratch/accounts-before.jsonl"
native_cli query Certificate --fields id --json >"$scratch/certificates-before.jsonl"
native_cli query Application --fields id --json >"$scratch/applications-before.jsonl"
native_cli query Tracer --fields id --json >"$scratch/tracers-before.jsonl"
# Permanent client rules are outside configuration ownership and must survive
# both configuration replay and the later complete initializer re-entry.
printf '%s\n' '{"@type":"create","object":"BlockedIp","value":{"protected-probe":{"address":"192.0.2.30","reason":"portScanning"},"protected-control":{"address":"192.0.2.31","reason":"manual"}}}' >"$scratch/protected-blocks.ndjson"
native_cli apply --file /proof/protected-blocks.ndjson --json >"$scratch/protected-blocks-apply.jsonl" 2>"$scratch/protected-blocks-apply.err"
jq -se 'any(.[];.status=="ok") and all(.[];.status!="error")' "$scratch/protected-blocks-apply.jsonl" >/dev/null
native_cli query BlockedIp --fields id,address,reason,createdAt,expiresAt --json >"$scratch/protected-blocks-before.jsonl"
jq -sS 'sort_by(.id)' "$scratch/protected-blocks-before.jsonl" >"$scratch/protected-blocks-before.json"
jq -e 'length==2 and ([.[].address]|sort)==["192.0.2.30","192.0.2.31"] and ([.[].id]|unique|length)==2 and all(.[];.id!="" and has("expiresAt") and .expiresAt==null)' "$scratch/protected-blocks-before.json" >/dev/null

# Primary addresses and aliases share a namespace across Users, Groups and
# MailingLists. Free only the owned aliases before creating each negative.
inventory
domain_id=$(jq -er '.methodResponses[6][1].list|map(select(.name=="example.test"))|if length==1 then .[0].id else error("ambiguous fixture domain") end' "$scratch/inputs/listener-inventory.json")
maintainer_id=$(jq -er '.methodResponses[10][1].list|map(select(.name=="maintainer"))|if length==1 then .[0].id else error("ambiguous retained account") end' "$scratch/inputs/listener-inventory.json")
jq -cn --arg domain "$domain_id" '{aliases:{"0":{name:"former-a",domainId:$domain,enabled:true,description:null},"1":{name:"former-b",domainId:$domain,enabled:true,description:null}}}' >"$scratch/approved-aliases.json"
printf '%s' '{"aliases":{}}' >"$scratch/clear-owned-aliases.json"
for collision in user-primary group-primary user-alias disabled-user-alias list-primary list-alias; do
  object=Account
  response_index=10
  case "$collision" in list-*) object=MailingList; response_index=12 ;; esac
  native_cli update Account "$maintainer_id" --file /proof/clear-owned-aliases.json >"$scratch/clear-owned.out" 2>"$scratch/clear-owned.err"
  jq -cn --arg kind "$collision" --arg object "$object" --arg domain "$domain_id" '
    ({name:(if ($kind|endswith("-primary")) then "former-a" else "owned-negative" end),domainId:$domain}
     + (if $object=="Account" then {"@type":(if $kind=="group-primary" then "Group" else "User" end)} else {recipients:{}} end)
     + (if ($kind|endswith("-alias")) then {aliases:{"0":{name:"former-a",domainId:$domain,enabled:($kind!="disabled-user-alias"),description:null}}} else {} end)) as $value
    | {"@type":"create",object:$object,value:{owned:$value}}' >"$scratch/receiving-negative.ndjson"
  native_cli apply --file /proof/receiving-negative.ndjson --json >"$scratch/receiving-create.jsonl" 2>"$scratch/receiving-create.err"
  jq -se 'any(.[];.status=="ok") and all(.[];.status!="error")' "$scratch/receiving-create.jsonl" >/dev/null
  inventory
  negative_id=$(jq -er --argjson i "$response_index" --arg kind "$collision" '.methodResponses[$i][1].list|map(select(.name==(if ($kind|endswith("-primary")) then "former-a" else "owned-negative" end)))|if length==1 then .[0].id else error("ambiguous owned negative") end' "$scratch/inputs/listener-inventory.json")
  jq -S --argjson i "$response_index" --arg id "$negative_id" '.methodResponses[$i][1].list|map(select(.id==$id))' "$scratch/inputs/listener-inventory.json" >"$scratch/receiving-owner-before.json"
  if prepare_plan "$scratch/unsafe-receiving.ndjson" >"$scratch/receiving-refusal.log" 2>&1; then
    echo 'FAIL: a competing receiving owner produced a plan' >&2; exit 1
  fi
  [ ! -e "$scratch/unsafe-receiving.ndjson" ]
  docker stop --time 30 "$name" >/dev/null
  docker rm "$name" >/dev/null
  if run_bootstrap; then
    echo 'FAIL: bootstrap took a competing receiving address' >&2; exit 1
  fi
  start_server preparation
  inventory
  jq -S --argjson i "$response_index" --arg id "$negative_id" '.methodResponses[$i][1].list|map(select(.id==$id))' "$scratch/inputs/listener-inventory.json" >"$scratch/receiving-owner-after.json"
  cmp "$scratch/receiving-owner-before.json" "$scratch/receiving-owner-after.json"
  jq -e --arg id "$maintainer_id" '.methodResponses[10][1].list|map(select(.id==$id))|length==1 and (.[0].aliases|length)==0' "$scratch/inputs/listener-inventory.json" >/dev/null
  printf '%s\n' "$negative_id" | native_cli delete "$object" --stdin >"$scratch/receiving-delete.out" 2>"$scratch/receiving-delete.err"
  inventory
  jq -e --argjson i "$response_index" --arg id "$negative_id" '.methodResponses[$i][1].list|all(.[];.id!=$id)' "$scratch/inputs/listener-inventory.json" >/dev/null
  native_cli update Account "$maintainer_id" --file /proof/approved-aliases.json >"$scratch/receiving-restore.out" 2>"$scratch/receiving-restore.err"
done

# An earlier safe snapshot cannot authorize a new bootstrap after live drift.
prepare_plan "$scratch/earlier-safe-snapshot.ndjson"
jq --arg domain "$domain_id" '.aliases["2"]={name:"unapproved",domainId:$domain,enabled:true,description:null}' "$scratch/approved-aliases.json" >"$scratch/drift-aliases.json"
native_cli update Account "$maintainer_id" --file /proof/drift-aliases.json >"$scratch/drift-alias.out" 2>"$scratch/drift-alias.err"
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
if run_bootstrap; then
  echo 'FAIL: bootstrap trusted a stale receiving snapshot' >&2; exit 1
fi
start_server preparation
inventory
jq -e --arg id "$maintainer_id" '.methodResponses[10][1].list|map(select(.id==$id))|length==1 and (.[0].aliases|length)==3' "$scratch/inputs/listener-inventory.json" >/dev/null
native_cli update Account "$maintainer_id" --file /proof/approved-aliases.json >"$scratch/drift-restore.out" 2>"$scratch/drift-restore.err"

# The renderer must reject an actual unexpected stored listener, even though
# recovery ignores its socket and the health endpoint remains green.
printf '%s\n' '{"@type":"upsert","object":"NetworkListener","matchOn":["name"],"value":{"negative":{"name":"unexpected","protocol":"smtp","bind":{"0.0.0.0:3456":true},"useTls":false}}}' >"$scratch/negative.ndjson"
native_cli apply --file /proof/negative.ndjson --json >"$scratch/negative-apply.jsonl" 2>"$scratch/negative-apply.err"
if prepare_plan "$scratch/unsafe-plan.ndjson" >"$scratch/negative-render.log" 2>&1; then
  echo 'FAIL: unexpected native listener was accepted' >&2; exit 1
fi
[ ! -e "$scratch/unsafe-plan.ndjson" ]
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
if run_bootstrap; then
  echo 'FAIL: bootstrap permitted serving after unsafe inventory' >&2; exit 1
fi
start_server preparation
native_cli query NetworkListener --where name=unexpected --fields id --json >"$scratch/negative-listener.jsonl"
jq -s '[.[].id] | if length==1 then . else error("negative listener identity is ambiguous") end' "$scratch/negative-listener.jsonl" |
  native_cli delete NetworkListener --stdin >"$scratch/negative-delete.out" 2>"$scratch/negative-delete.err"

# A second application shares HTTP even when the mail listeners are safe. Its
# presence must fence initialization rather than being silently preserved.
printf '%s\n' '{"@type":"upsert","object":"Application","matchOn":["description"],"value":{"negative":{"description":"Unexpected fixture application","enabled":false,"resourceUrl":"file:///usr/local/share/stalwart/webui.zip","urlPrefix":{"/unexpected":true}}}}' >"$scratch/negative-application.ndjson"
native_cli apply --file /proof/negative-application.ndjson --json >"$scratch/negative-application-apply.jsonl" 2>"$scratch/negative-application-apply.err"
if prepare_plan "$scratch/unsafe-application-plan.ndjson" >"$scratch/negative-application-render.log" 2>&1; then
  echo 'FAIL: unexpected native application was accepted' >&2; exit 1
fi
[ ! -e "$scratch/unsafe-application-plan.ndjson" ]
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
if run_bootstrap; then
  echo 'FAIL: bootstrap permitted serving after an unexpected application' >&2; exit 1
fi
start_server preparation
inventory
# Application/query cannot filter by description in the pinned server. Select
# the owned negative fixture from a complete bounded query/get instead.
jq -ce '
  .methodResponses[7][1] as $query | .methodResponses[8][1] as $get |
  if $query.total != 2 or $query.position != 0 or ($query.ids|unique|length) != 2 or
     ($get.list|length) != 2 or $get.notFound != [] or
     ([$get.list[].id]|sort) != ($query.ids|sort) then
    error("negative application inventory is incomplete")
  else
    [$get.list[]|select(.description=="Unexpected fixture application")|.id] |
    if length==1 and (.[0]|type)=="string" and .[0]!="" then .
    else error("negative application identity is ambiguous") end
  end' "$scratch/inputs/listener-inventory.json" |
  native_cli delete Application --stdin >"$scratch/negative-application-delete.out" 2>"$scratch/negative-application-delete.err"

# The pinned server rejects a reserved primary domain, but accepts it as a
# local domain alias. That alias must not turn the rejection sink into a local
# recipient. Exercise the real accepted alias, then restore the fixture.
inventory
domain_id=$(jq -er '[.methodResponses[6][1].list[]|select(.name=="example.test")]|if length==1 then .[0].id else error("ambiguous fixture domain") end' "$scratch/inputs/listener-inventory.json")
printf '%s' '{"aliases":{"example.invalid":true}}' >"$scratch/sink-alias-patch.json"
native_cli update Domain "$domain_id" --file /proof/sink-alias-patch.json >"$scratch/sink-alias.out" 2>"$scratch/sink-alias.err"
if prepare_plan "$scratch/unsafe-sink-plan.ndjson" >"$scratch/sink-negative.log" 2>&1; then
  echo 'FAIL: a registered rejection-domain alias was accepted' >&2; exit 1
fi
[ ! -e "$scratch/unsafe-sink-plan.ndjson" ]
printf '%s' '{"aliases":{}}' >"$scratch/restore-alias-patch.json"
native_cli update Domain "$domain_id" --file /proof/restore-alias-patch.json >"$scratch/restore-alias.out" 2>"$scratch/restore-alias.err"

# Seed a previously wider permission and an extra expression branch. Replay
# must clear both maps rather than preserving keys absent from the new plan.
# Also suspend the managed administrator: replay must restore its inherited
# access, proved by the regular administrator's requests after serving starts.
printf '%s\n' '{"@type":"upsert","object":"Domain","matchOn":["name"],"value":{"domain":{"name":"example.test"}}}' \
  '{"@type":"upsert","object":"Account","matchOn":["name","domainId"],"value":{"agents":{"@type":"User","name":"agents","domainId":"#domain","permissions":{"@type":"Replace","enabledPermissions":{"jmapEmailImport":true,"jmapEmailCreate":true}}}}}' \
  '{"@type":"upsert","object":"Account","matchOn":["name","domainId"],"value":{"administrator":{"@type":"User","name":"operator","domainId":"#domain","permissions":{"@type":"Replace","enabledPermissions":{},"disabledPermissions":{}}}}}' \
  '{"@type":"update","object":"MtaStageRcpt","value":{"rewrite":{"match":{"9":{"if":"true","then":"'"'"'operator@example.test'"'"'"}}}}}' >"$scratch/wider.ndjson"
native_cli apply --file /proof/wider.ndjson --json >"$scratch/wider-apply.jsonl" 2>"$scratch/wider-apply.err"
prepare_plan "$scratch/replay.ndjson"
mv "$scratch/replay.ndjson" "$scratch/plan.ndjson"
apply_plan
native_cli get MtaStageRcpt --json >"$scratch/rcpt.json"
jq -e '.rewrite.match|keys|sort==["0","1"]' "$scratch/rcpt.json" >/dev/null
native_cli query Account --fields id --json >"$scratch/accounts-after.jsonl"
native_cli query Certificate --fields id --json >"$scratch/certificates-after.jsonl"
native_cli query Application --fields id --json >"$scratch/applications-after.jsonl"
native_cli query Tracer --fields id --json >"$scratch/tracers-after.jsonl"
for object in accounts certificates applications tracers; do
  jq -s '[.[].id]|sort' "$scratch/$object-before.jsonl" >"$scratch/before.ids"
  jq -s '[.[].id]|sort' "$scratch/$object-after.jsonl" >"$scratch/after.ids"
  cmp "$scratch/before.ids" "$scratch/after.ids"
done
inventory
jq -e '.methodResponses[0][1].total==5 and ([.methodResponses[1][1].list[].name]|sort)==["client-proxy","imaps","management","smtp-inbound","submission"]' "$scratch/inputs/listener-inventory.json" >/dev/null
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
start_server serving
verify_rendered_self_checks
agent_password=$(cat "$scratch/inputs/agent-password")
human_password=$(cat "$scratch/inputs/maintainer-password")
administrator_password=$(cat "$scratch/inputs/administrator-password")
printf 'user = "agents@example.test:%s"\n' "$agent_password" >"$scratch/agent.curl"
printf 'user = "maintainer@example.test:%s"\n' "$human_password" >"$scratch/human.curl"
printf 'user = "operator@example.test:%s"\n' "$administrator_password" >"$scratch/administrator.curl"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/administrator.curl "http://$name:8080/jmap/session" >"$scratch/administrator-session.json"
# A native client credential is independently owned. Neither replay nor a full
# initializer re-entry may remove it or clear primary authentication restrictions.
jq -cn '{using:["urn:ietf:params:jmap:core","urn:stalwart:jmap"],methodCalls:[["x:AppPassword/set",{create:{app:{description:"Native client retention proof",expiresAt:"2031-01-01T00:00:00Z"}}},"app"]]}' >"$scratch/client-create.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/client-create.json "http://$name:8080/jmap" >"$scratch/client-create-result.json"
jq -er '.methodResponses[0][1].created.app.secret|select(type=="string" and length>0)' "$scratch/client-create-result.json" >"$scratch/client-password"
jq -er '"user = " + ("maintainer@example.test:" + .methodResponses[0][1].created.app.secret|@json)' "$scratch/client-create-result.json" >"$scratch/client.curl"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/client.curl "http://$name:8080/jmap/session" >"$scratch/client-before.json"
jq -cn --arg id "$maintainer_id" '{using:["urn:ietf:params:jmap:core","urn:stalwart:jmap"],methodCalls:[["x:Account/set",{update:{($id):{"credentials/0/allowedIps":{"192.0.2.88/32":true}}}},"restrict"]]}' >"$scratch/primary-restriction.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/administrator.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/primary-restriction.json "http://$name:8080/jmap" >"$scratch/primary-restriction-result.json"
jq -e --arg id "$maintainer_id" '.methodResponses[0][1].updated|has($id)' "$scratch/primary-restriction-result.json" >/dev/null
primary_status=$(docker exec -i "$name" curl --disable --noproxy '*' --silent --show-error --max-time 10 \
  --config - --output /dev/null --write-out '%{http_code}' http://127.0.0.1:8080/jmap/session <"$scratch/human.curl")
[ "$primary_status" = 403 ]
cp "$scratch/inputs/maintainer-password" "$scratch/old-maintainer-password"
openssl rand -hex 32 | tr -d '\n' >"$scratch/inputs/maintainer-password"
human_password=$(cat "$scratch/inputs/maintainer-password")
printf 'user = "maintainer@example.test:%s"\n' "$human_password" >"$scratch/human.curl"
cp "$root/scripts/private-mail-config/inventory-request.json" "$scratch/client-inventory-request.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/administrator.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/client-inventory-request.json "http://$name:8080/jmap" >"$scratch/inputs/listener-inventory.json"
go -C "$root" run ./scripts/private-mail-config --input-dir "$scratch/inputs" --output-file "$scratch/client-replay.ndjson"
administration_cli apply --file /proof/client-replay.ndjson --json >"$scratch/client-replay-result.jsonl" 2>"$scratch/client-replay.err"
jq -se '[.[]|select(.status=="error")]|length==0' "$scratch/client-replay-result.jsonl" >/dev/null
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/client.curl "http://$name:8080/jmap/session" >"$scratch/client-after-replay.json"
primary_status=$(docker exec -i "$name" curl --disable --noproxy '*' --silent --show-error --max-time 10 \
  --config - --output /dev/null --write-out '%{http_code}' http://127.0.0.1:8080/jmap/session <"$scratch/human.curl")
[ "$primary_status" = 403 ]
jq -cn --arg id "$maintainer_id" '{using:["urn:ietf:params:jmap:core","urn:stalwart:jmap"],methodCalls:[["x:Account/set",{update:{($id):{"credentials/0/allowedIps":{}}}},"restore"]]}' >"$scratch/primary-restore.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/administrator.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/primary-restore.json "http://$name:8080/jmap" >"$scratch/primary-restore-result.json"
jq -e --arg id "$maintainer_id" '.methodResponses[0][1].updated|has($id)' "$scratch/primary-restore-result.json" >/dev/null
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl "http://$name:8080/jmap/session" >"$scratch/rotated-primary-session.json"
echo 'PASS: native client credential and primary address restriction survive managed-password replay'
# The private bridge has no external egress. Verify each served interface and
# compare every script/style referenced by its entry page to the pinned ZIP.
verify_administration_assets
printf '%s' '{"using":["urn:ietf:params:jmap:core","urn:stalwart:jmap"],"methodCalls":[["x:Application/query",{"limit":2,"calculateTotal":true},"applications"]]}' >"$scratch/management-request.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/administrator.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/management-request.json "http://$name:8080/jmap" >"$scratch/management-authenticated.json"
jq -e '.methodResponses[0][0]=="x:Application/query" and .methodResponses[0][1].total==1' "$scratch/management-authenticated.json" >/dev/null
management_status=$(protocol_curl --silent --show-error --max-time 10 --header 'Content-Type: application/json' \
  --data-binary @/proof/management-request.json --output /dev/null --write-out '%{http_code}' "http://$name:8080/jmap")
[ "$management_status" = 401 ]
printf 'From: agents@example.test\r\nTo: maintainer@example.test\r\nSubject: Native policy proof\r\nMessage-ID: <mail-policy@example.test>\r\n\r\nPreserved native mail fixture.\r\n' >"$scratch/known.eml"
smtp=(--silent --show-error --max-time 15 --cacert /proof/etc/tls/tls.crt --connect-to "mail.example.test:1587:$name:1587" --ssl-reqd --url smtp://mail.example.test:1587 --mail-from agents@example.test --upload-file /proof/known.eml)
auth_advertised() {
  grep -Eqi '^< 250[- ]AUTH([ =]|$)' "$1"
}
protocol_curl --fail "${smtp[@]}" --config /proof/agent.curl --mail-rcpt maintainer@example.test >"$scratch/allowed.out" 2>"$scratch/allowed.err"
for recipient in operator@example.test someone@remote.example.test maintainer+tag@example.test former-a@example.test former-b@example.test; do
  if protocol_curl "${smtp[@]}" --config /proof/agent.curl --mail-rcpt "$recipient" >"$scratch/denied.out" 2>"$scratch/denied.err"; then
    echo 'FAIL: agent recipient restriction was bypassed' >&2; exit 1
  fi
  grep -q '550' "$scratch/denied.err"
done
if protocol_curl --verbose "${smtp[@]}" --mail-rcpt maintainer@example.test >"$scratch/no-auth.out" 2>"$scratch/no-auth.err"; then
  echo 'FAIL: submission accepted an unauthenticated envelope' >&2; exit 1
fi
grep -q '503' "$scratch/no-auth.err"
# The same detector must see AUTH on submission. This probe supplies no
# credentials, so capability logging cannot record an authentication secret.
auth_advertised "$scratch/no-auth.err" || {
  echo 'FAIL: authenticated-submission capabilities were not observed' >&2; exit 1;
}
if protocol_curl --silent --show-error --max-time 10 --url "smtp://$name:1025" --mail-from sender@remote.example.test --mail-rcpt someone@other.example.test --upload-file /proof/known.eml >"$scratch/relay.out" 2>"$scratch/relay.err"; then
  echo 'FAIL: unauthenticated relay was accepted' >&2; exit 1
fi
grep -q '550' "$scratch/relay.err"
# Human submission retains normal external sending. The isolated .test
# destination cannot leave this private fixture network.
printf 'From: maintainer@example.test\r\nTo: someone@remote.example.test\r\nSubject: Human submission proof\r\n\r\nDisposable fixture.\r\n' >"$scratch/human.eml"
protocol_curl --fail --silent --show-error --max-time 15 --config /proof/human.curl \
  --cacert /proof/etc/tls/tls.crt --connect-to "mail.example.test:1587:$name:1587" \
  --ssl-reqd --url smtp://mail.example.test:1587 --mail-from maintainer@example.test \
  --mail-rcpt someone@remote.example.test --upload-file /proof/human.eml >"$scratch/human-send.out" 2>"$scratch/human-send.err"
# Incoming local delivery succeeds without credentials. Observe both EHLOs
# across STARTTLS: successful delivery alone says nothing about AUTH being
# offered, because curl can omit authentication when the server omits AUTH.
protocol_curl --fail --silent --show-error --verbose --max-time 10 \
  --cacert /proof/etc/tls/tls.crt --connect-to "mail.example.test:1025:$name:1025" \
  --ssl-reqd --url smtp://mail.example.test:1025 --mail-from sender@remote.example.test \
  --mail-rcpt maintainer@example.test --upload-file /proof/known.eml >"$scratch/mx-auth.out" 2>"$scratch/mx-capabilities.log"
if [ "$(grep -c '^> EHLO ' "$scratch/mx-capabilities.log")" -lt 2 ]; then
  echo 'FAIL: MX capabilities were not observed before and after TLS' >&2; exit 1
fi
if auth_advertised "$scratch/mx-capabilities.log"; then
  echo 'FAIL: MX offered agent authentication' >&2; exit 1
fi
for alias in former-a former-b; do
  printf 'From: sender@remote.example.test\r\nTo: %s@example.test\r\nSubject: Receiving continuity %s\r\nMessage-ID: <%s-receiving@example.test>\r\n\r\nReceiving continuity %s proof.\r\n' "$alias" "$alias" "$alias" "$alias" >"$scratch/alias.eml"
  protocol_curl --fail --silent --show-error --max-time 15 --cacert /proof/etc/tls/tls.crt \
    --connect-to "mail.example.test:1025:$name:1025" --ssl-reqd --url smtp://mail.example.test:1025 \
    --mail-from sender@remote.example.test --mail-rcpt "$alias@example.test" --upload-file /proof/alias.eml >"$scratch/alias-delivery.out" 2>"$scratch/alias-delivery.err"
done
verify_receiving_aliases
for alias in former-a former-b; do
  cp "$scratch/$alias-receiving-metadata.json" "$scratch/$alias-receiving-before.json"
done
if protocol_curl --silent --show-error --max-time 10 --url "smtp://$name:1025" --mail-from sender@remote.example.test --mail-rcpt unknown@example.test --upload-file /proof/known.eml >"$scratch/unknown-recipient.out" 2>"$scratch/unknown-recipient.err"; then
  echo 'FAIL: an undeclared receiving address was accepted' >&2; exit 1
fi
grep -q '550' "$scratch/unknown-recipient.err"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/agent.curl "http://$name:8080/jmap/session" >"$scratch/session.json"
account_id=$(jq -er '.primaryAccounts."urn:ietf:params:jmap:mail"' "$scratch/session.json")
jq -cn --arg account "$account_id" '{using:["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],methodCalls:[["Email/query",{accountId:$account},"query"],["Email/import",{accountId:$account,emails:{}},"import"],["Email/set",{accountId:$account,create:{fake:{subject:"private-canary"}}},"set"]]}' >"$scratch/write-request.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/agent.curl --header 'Content-Type: application/json' --data-binary @/proof/write-request.json "http://$name:8080/jmap" >"$scratch/write-result.json"
jq -e '.methodResponses[0][0]=="Email/query" and .methodResponses[1][0]=="error" and .methodResponses[1][1].type=="forbidden" and .methodResponses[2][0]=="error" and .methodResponses[2][1].type=="forbidden"' "$scratch/write-result.json" >/dev/null
upload_status=$(protocol_curl --silent --max-time 10 --config /proof/agent.curl --data-binary @/proof/known.eml --output /dev/null --write-out '%{http_code}' "http://$name:8080/jmap/upload/$account_id/")
[ "$upload_status" = 403 ]
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl --cacert /proof/etc/tls/tls.crt --connect-to "mail.example.test:1993:$name:1993" 'imaps://mail.example.test:1993/INBOX;UID=1' >"$scratch/known-read.eml"
grep -q 'Preserved native mail fixture.' "$scratch/known-read.eml"
# Prime the native application cache with a previous unversioned bundle. An
# upgrade must invalidate that cache while preserving the Application identity.
docker stop --time 30 "$name" >/dev/null
docker logs "$name" >"$scratch/smtp-auth-server.log" 2>&1
docker rm "$name" >/dev/null
# Establish which native protocol can inspect a recovered datastore before
# designing the isolated restore worker. A recovery-only listener must not be
# assumed to provide ordinary mailbox access. Both outcomes below are evidence
# about the pinned server, never a claim that production data was restored.
start_server preparation
recovery_mail_status=$(protocol_curl --silent --show-error --max-time 10 \
  --config /proof/human.curl --header 'Content-Type: application/json' \
  --data-binary @/proof/receiving-query.json --output /dev/null \
  --write-out '%{http_code}' "http://$name:8080/jmap")
if [ "$recovery_mail_status" = 200 ]; then
  protocol_curl --fail --silent --show-error --max-time 10 \
    --config /proof/human.curl --header 'Content-Type: application/json' \
    --data-binary @/proof/receiving-query.json "http://$name:8080/jmap" \
    >"$scratch/recovery-mail-result.json"
  if jq -e '.methodResponses|length==2 and .[0][0]=="Email/query" and .[1][0]=="Email/get"' \
    "$scratch/recovery-mail-result.json" >/dev/null; then
    verify_receiving_aliases
    echo 'EMAIL_RECOVERY_PROTOCOL=MAILBOX_READ_VERIFIED mode=recovery'
  elif jq -e '.methodResponses|length>0 and all(.[]; .[0]=="error" and (.[1].type=="forbidden" or .[1].type=="unknownMethod"))' \
    "$scratch/recovery-mail-result.json" >/dev/null; then
    echo 'EMAIL_RECOVERY_PROTOCOL=MAILBOX_UNAVAILABLE mode=recovery'
  else
    echo 'FAIL: native recovery mailbox protocol returned an unclassified response' >&2; exit 1
  fi
elif [ "$recovery_mail_status" = 401 ] || [ "$recovery_mail_status" = 403 ]; then
  echo 'EMAIL_RECOVERY_PROTOCOL=MAILBOX_UNAVAILABLE mode=recovery'
else
  echo 'FAIL: native recovery mailbox protocol returned an unexpected status' >&2; exit 1
fi
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
mkdir "$scratch/stale-assets" "$scratch/stale-source"
printf '%s' '<!doctype html><html><head><base href="/" /></head><body>previous-ui-cache-canary</body></html>' >"$scratch/stale-source/index.html"
(cd "$scratch/stale-source" && zip -q "$scratch/stale-assets/webui.zip" index.html)
cp "$scratch/stale-assets/webui.zip" "$scratch/stale-assets/previous-ui-cache-canary.zip"
start_server old-interface-preparation
inventory
application_id=$(jq -er '.methodResponses[8][1].list|if length==1 then .[0].id else error("ambiguous administration identity") end' "$scratch/inputs/listener-inventory.json")
printf '%s' "$application_id" >"$scratch/cached-application-before.id"
# Clear any initially cached bundle using a second safe fixture URI. Both
# resources in this priming process contain only our disposable canary page.
printf '%s' '{"resourceUrl":"file:///usr/local/share/stalwart/previous-ui-cache-canary.zip"}' >"$scratch/stale-application-clear.json"
native_cli update Application "$application_id" --file /proof/stale-application-clear.json >"$scratch/stale-clear.out" 2>"$scratch/stale-clear.err"
printf '%s' '{"resourceUrl":"file:///usr/local/share/stalwart/webui.zip"}' >"$scratch/stale-application.json"
native_cli update Application "$application_id" --file /proof/stale-application.json >"$scratch/stale-application.out" 2>"$scratch/stale-application.err"
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
start_server old-interface
protocol_curl --fail --silent --show-error --max-time 10 "http://$name:8080/admin/login" >"$scratch/stale-served.html"
grep -q 'previous-ui-cache-canary' "$scratch/stale-served.html"
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
# Re-enter an explicitly owned recovery process after both cached UI and real
# mail exist. A successful readback must not retain the previous cached bundle.
jq -cn --arg id "$maintainer_id" '{"@type":"update",object:"Account",id:$id,value:{"credentials/0/expiresAt":"2000-01-01T00:00:00Z"}}' >"$scratch/primary-expiry.ndjson"
# The preceding interface probe has stopped its server. Seed only the fixed
# disposable account in recovery, then stop it before the real initializer.
start_server preparation
native_cli apply --file /proof/primary-expiry.ndjson --json >"$scratch/primary-expiry-result.jsonl" 2>"$scratch/primary-expiry.err"
jq -se '[.[]|select(.status=="error")]|length==0' "$scratch/primary-expiry-result.jsonl" >/dev/null
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
# Expiration keeps the primary credential usable for password recovery, but
# downgrades its permissions. Bind the negative to a query that succeeds again
# after restoring the expiry, rather than assuming session sign-in is refused.
primary_account=$(jq -er '.primaryAccounts."urn:ietf:params:jmap:mail"' "$scratch/rotated-primary-session.json")
jq -cn --arg account "$primary_account" '{using:["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],methodCalls:[["Email/query",{accountId:$account},"query"]]}' >"$scratch/expiry-query.json"
start_server serving
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/expiry-query.json "http://$name:8080/jmap" >"$scratch/expiry-before-bootstrap.json"
jq -e '.methodResponses[0][0]=="error" and .methodResponses[0][1].type=="forbidden"' "$scratch/expiry-before-bootstrap.json" >/dev/null
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
run_bootstrap
start_server serving
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/client.curl "http://$name:8080/jmap/session" >"$scratch/client-after-bootstrap.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/expiry-query.json "http://$name:8080/jmap" >"$scratch/expiry-after-bootstrap.json"
jq -e '.methodResponses[0][0]=="error" and .methodResponses[0][1].type=="forbidden"' "$scratch/expiry-after-bootstrap.json" >/dev/null
jq -cn --arg id "$maintainer_id" '{"@type":"update",object:"Account",id:$id,value:{"credentials/0/expiresAt":null}}' >"$scratch/primary-expiry-restore.ndjson"
administration_cli apply --file /proof/primary-expiry-restore.ndjson --json >"$scratch/primary-expiry-restore-result.jsonl" 2>"$scratch/primary-expiry-restore.err"
jq -se '[.[]|select(.status=="error")]|length==0' "$scratch/primary-expiry-restore-result.jsonl" >/dev/null
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl "http://$name:8080/jmap/session" >"$scratch/primary-after-bootstrap.json"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/expiry-query.json "http://$name:8080/jmap" >"$scratch/expiry-restored.json"
jq -e '.methodResponses[0][0]=="Email/query"' "$scratch/expiry-restored.json" >/dev/null
echo 'PASS: native client credential and primary expiration survive complete initializer re-entry'
protocol_curl --fail --silent --show-error --max-time 10 "http://$name:8080/admin/login" >"$scratch/refreshed-interface.html"
if grep -q 'previous-ui-cache-canary' "$scratch/refreshed-interface.html"; then
  echo 'FAIL: bootstrap reused the previous cached administration bundle' >&2; exit 1
fi
verify_administration_assets
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/administrator.curl \
  --header 'Content-Type: application/json' --data-binary @/proof/management-request.json "http://$name:8080/jmap" >"$scratch/cached-application-after.json"
jq -er '.methodResponses[0][1]|if .total==1 and (.ids|length)==1 then .ids[0] else error("ambiguous refreshed administration identity") end' "$scratch/cached-application-after.json" | tr -d '\n' >"$scratch/cached-application-after.id"
cmp "$scratch/cached-application-before.id" "$scratch/cached-application-after.id"
protocol_curl --fail --silent --show-error --max-time 10 --config /proof/human.curl --cacert /proof/etc/tls/tls.crt --connect-to "mail.example.test:1993:$name:1993" 'imaps://mail.example.test:1993/INBOX;UID=1' >"$scratch/preserved-read.eml"
grep -q 'Preserved native mail fixture.' "$scratch/preserved-read.eml"
# Omitted settings must preserve configured aliases and their delivered mail.
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
jq 'del(.maintainerAliases)' "$scratch/inputs/settings.json" >"$scratch/inputs/settings.next"
mv "$scratch/inputs/settings.next" "$scratch/inputs/settings.json"
run_bootstrap
start_server preparation
inventory
jq -e --arg id "$maintainer_id" '.methodResponses[10][1].list|map(select(.id==$id))|length==1 and (.[0].aliases|length)==2' "$scratch/inputs/listener-inventory.json" >/dev/null
native_cli query BlockedIp --fields id,address,reason,createdAt,expiresAt --json >"$scratch/protected-blocks-after.jsonl"
jq -sS '[.[]|select(.address=="192.0.2.30" or .address=="192.0.2.31")]|sort_by(.id)' "$scratch/protected-blocks-after.jsonl" >"$scratch/protected-blocks-after.json"
cmp "$scratch/protected-blocks-before.json" "$scratch/protected-blocks-after.json"
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
start_server serving
verify_receiving_aliases
for alias in former-a former-b; do
  cmp "$scratch/$alias-receiving-before.json" "$scratch/$alias-receiving-metadata.json"
done
docker logs "$name" >"$scratch/server.log" 2>&1
for log in "$scratch/smtp-auth-server.log" "$scratch/server.log"; do
  assert_diagnostic_log_privacy "$log" "$scratch/inputs/"*-password "$scratch/old-maintainer-password" "$scratch/client-password"
done
if grep -Eqi 'temporary administrator|private-mail-recovery-fixture-only' "$scratch/server.log"; then
  echo 'FAIL: a recovery credential appeared in server logs' >&2; exit 1
fi
# The new transport policy is opt-in. Exercise native persisted readback in
# both directions without changing the fixture's identities or mailbox data.
for forwarded in true false; do
  docker stop --time 30 "$name" >/dev/null
  docker rm "$name" >/dev/null
  jq --argjson enabled "$forwarded" '.useForwardedClientIP=$enabled' \
    "$scratch/inputs/settings.json" >"$scratch/inputs/settings.next"
  mv "$scratch/inputs/settings.next" "$scratch/inputs/settings.json"
  if [ "$forwarded" = true ] && run_bootstrap --proxy-client-ip=true; then
    echo 'FAIL: bootstrap combined HTTP and transport identity modes' >&2; exit 1
  fi
  run_bootstrap
  start_server serving
done

# Prove the maintained proxy against the real pinned mail server. These are
# disposable identities on the existing internal fixture network, not a claim
# about production Gateway trust or Cilium network-policy enforcement.
administration_cli query AllowedIp --fields id,address --json >"$scratch/allowed-before.jsonl"
jq -sS 'sort_by(.id)' "$scratch/allowed-before.jsonl" >"$scratch/allowed-before.json"
printf '%s\n' '{"@type":"create","object":"BlockedIp","value":{"proxy-client":{"address":"198.51.100.9","reason":"manual"},"proxy-client-ipv6":{"address":"2001:db8::9","reason":"manual"}}}' >"$scratch/proxy-block.ndjson"
administration_cli apply --file /proof/proxy-block.ndjson --json >"$scratch/proxy-block-apply.jsonl" 2>"$scratch/proxy-block-apply.err"
jq -se 'any(.[];.status=="ok") and all(.[];.status!="error")' "$scratch/proxy-block-apply.jsonl" >/dev/null

for proxy_enabled in true false; do
  docker stop --time 30 "$name" >/dev/null
  docker rm "$name" >/dev/null
  jq '.useForwardedClientIP=false | .useProxyClientIP=false' \
    "$scratch/inputs/settings.json" >"$scratch/inputs/settings.next"
  mv "$scratch/inputs/settings.next" "$scratch/inputs/settings.json"
  cp "$scratch/inputs/settings.json" "$scratch/proxy-settings-before.json"
  run_bootstrap --proxy-client-ip="$proxy_enabled"
  cmp "$scratch/proxy-settings-before.json" "$scratch/inputs/settings.json"
  start_server serving
  if [ "$proxy_enabled" = false ]; then
    docker exec "$name" /usr/bin/curl --disable --noproxy '*' --fail --silent --show-error \
      --max-time 3 http://127.0.0.1:18081/healthz/ready >/dev/null
    continue
  fi

  # Native direct access cannot skip the required PROXY envelope.
  direct_status=0
  docker exec "$name" /usr/bin/curl --disable --noproxy '*' --silent \
    --max-time 3 http://127.0.0.1:18081/healthz/ready >"$scratch/proxy-direct.out" 2>"$scratch/proxy-direct.err" || direct_status=$?
  [ "$direct_status" != 0 ] || { echo 'FAIL: native proxy listener accepted a request without its transport envelope' >&2; exit 1; }

  cp "$root/k8s/bases/apps/private-mail-proxy-identity/Caddyfile" "$scratch/ClientProxy.Caddyfile"
  proxy_started=1
  docker run --detach --name "$proxy_name" --network "container:$name" --read-only \
    --user "$fixture_user" --cpus 0.1 --memory 64m --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --mount "type=bind,source=$scratch/ClientProxy.Caddyfile,target=/etc/email-client-proxy/Caddyfile,readonly" \
    --entrypoint /usr/local/bin/caddy "$name-bootstrap-image" \
    run --config /etc/email-client-proxy/Caddyfile --adapter caddyfile >"$scratch/proxy-start.out" 2>"$scratch/proxy-start.err"
  proxy_ready=0
  for ((attempt=0; attempt<30; attempt++)); do
    if docker exec "$proxy_name" /usr/bin/curl --disable --noproxy '*' --fail --silent \
      --max-time 2 --header 'X-Forwarded-For: 127.0.0.1' http://127.0.0.1:8081/healthz/ready >/dev/null 2>&1; then
      proxy_ready=1; break
    fi
    sleep 1
  done
  [ "$proxy_ready" = 1 ] || { echo 'FAIL: native proxy-to-mail health did not become ready' >&2; exit 1; }
  verify_rendered_self_checks private-mail-proxy-identity client-proxy "$proxy_name" 127.0.0.1:8081

  for client_ip in 198.51.100.9 198.51.100.10 2001:db8::9 2001:db8::10 10.20.30.40 fd00::9; do
    expected=200
    case "$client_ip" in 198.51.100.9|2001:db8::9) expected=502 ;; esac
    status=$(protocol_curl --silent --show-error --max-time 5 --output /dev/null --write-out '%{http_code}' \
      --header "X-Forwarded-For: $client_ip" --header 'Forwarded: for=203.0.113.22' \
      --header 'X-Real-IP: 203.0.113.23' --header 'X-Email-Private-Canary: email-proxy-private-canary' \
      "http://$name:8081/healthz/ready?private=email-proxy-private-canary")
    [ "$status" = "$expected" ] || { echo 'FAIL: native mail ban or per-client identity did not survive proxying' >&2; exit 1; }
  done
  for malformed in missing duplicate chain invalid invalid-ipv6; do
    header_args=()
    case "$malformed" in
      duplicate) header_args=(--header 'X-Forwarded-For: 198.51.100.9' --header 'X-Forwarded-For: 198.51.100.10') ;;
      chain) header_args=(--header 'X-Forwarded-For: 198.51.100.9, 198.51.100.10') ;;
      invalid) header_args=(--header 'X-Forwarded-For: 999.999.999.999') ;;
      invalid-ipv6) header_args=(--header 'X-Forwarded-For: ::::') ;;
    esac
    status=$(protocol_curl --silent --show-error --max-time 5 --output /dev/null --write-out '%{http_code}' \
      "${header_args[@]}" "http://$name:8081/healthz/ready")
    [ "$status" = 400 ] || { echo 'FAIL: native proxy accepted missing or ambiguous identity metadata' >&2; exit 1; }
  done
  protocol_curl --fail --silent --show-error --max-time 5 --config /proof/administrator.curl \
    --header 'X-Forwarded-For: 198.51.100.10' "http://$name:8081/jmap/session" >"$scratch/proxy-administrator-session.json"
  jq -e '.accounts|type=="object" and length>0' "$scratch/proxy-administrator-session.json" >/dev/null
  docker logs "$proxy_name" >"$scratch/proxy-runtime.log" 2>&1
  if grep -Fq 'email-proxy-private-canary' "$scratch/proxy-runtime.log"; then
    echo 'FAIL: private request metadata reached the proxy log' >&2; exit 1
  fi
  administration_cli query AllowedIp --fields id,address --json >"$scratch/allowed-after.jsonl"
  jq -sS 'sort_by(.id)' "$scratch/allowed-after.jsonl" >"$scratch/allowed-after.json"
  cmp "$scratch/allowed-before.json" "$scratch/allowed-after.json"
  administration_cli query BlockedIp --fields id,address,reason,createdAt,expiresAt --json >"$scratch/proxy-retained-blocks.jsonl"
  jq -sS '[.[]|select(.address=="192.0.2.30" or .address=="192.0.2.31")]|sort_by(.id)' "$scratch/proxy-retained-blocks.jsonl" >"$scratch/proxy-retained-blocks.json"
  cmp "$scratch/protected-blocks-before.json" "$scratch/proxy-retained-blocks.json"
  docker rm -fv "$proxy_name" >/dev/null
  proxy_started=0
done
echo 'PASS: native mail configuration, receiving continuity, ownership refusal, cached UI upgrade, replay, TLS, account restrictions, per-client proxy bans and explicit rollback'
