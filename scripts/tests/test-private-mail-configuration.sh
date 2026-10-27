#!/usr/bin/env bash
# Native protocol proof using disposable .test accounts and a private network.
set -Eeuo pipefail
trap 'printf "FAIL: native mail proof stopped at line %s\n" "$LINENO" >&2' ERR
umask 077
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture_user="$(id -u):$(id -g)"
[ "$(id -u)" != 0 ] || { echo 'FAIL: native mail proof requires an unprivileged fixture user' >&2; exit 1; }
scratch="$(mktemp -d)"
name="mail-config-$(basename "$scratch" | tr '[:upper:]' '[:lower:]')"
network="${name}-network"
server='ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c'
cli='ghcr.io/stalwartlabs/cli:1.0.13@sha256:9a1e07307d6f890a193322c4b737fc0868405f0f8486a19922cbf362f43884fd'
cleanup() {
  local rc=$? cleanup_rc=0
  trap - EXIT
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
  -ec 'sha256sum /usr/local/share/stalwart/webui-*.zip; test -r /usr/local/share/licenses/stalwart-webui/LICENSE; test -r /usr/local/share/licenses/stalwart-webui/SOURCE' >"$scratch/image-assets.txt"
[ "$(cut -d ' ' -f 1 "$scratch/image-assets.txt")" = "$(jq -er .sha256 "$root/scripts/private-mail-config/webui-pins.json")" ]

run_bootstrap() {
  # Bind-mounted files belong to the invoking unprivileged user. Keep one
  # identity through bootstrap and every restart, as the deployment requires.
  docker run --rm --name "$name-bootstrap" --network "$network" --read-only \
    --user "$fixture_user" --cpus 1 --memory 512m --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --mount "type=bind,source=$scratch/etc,target=/etc/stalwart,readonly" \
    --mount "type=bind,source=$scratch/inputs,target=/etc/private-mail,readonly" \
    --mount "type=bind,source=$scratch/data,target=/var/lib/stalwart" \
    --mount "type=bind,source=$scratch/tmp,target=/tmp" \
    "$name-bootstrap-image" >"$scratch/bootstrap.log" 2>&1
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
  local -a check_command=() refused_command=()
  kubectl kustomize "$root/k8s/bases/apps/private-mail-administration" >"$scratch/health-render.yaml"
  yq -o=json -I=0 'select(.kind == "StatefulSet" and .metadata.name == "private-mail").spec.template.spec.containers[] | select(.name == "private-mail")' \
    "$scratch/health-render.yaml" >"$scratch/health-container.json"
  for probe in startupProbe readinessProbe livenessProbe; do
    jq -e --arg probe "$probe" '.[$probe].exec.command | type=="array" and length>0 and all(.[];type=="string")' "$scratch/health-container.json" >/dev/null
    check_command=()
    while IFS= read -r -d '' arg; do check_command+=("$arg"); done < <(jq -j --arg probe "$probe" '.[$probe].exec.command[] | ., "\u0000"' "$scratch/health-container.json")
    docker exec "$name" "${check_command[@]}" >"$scratch/$probe.stdout" 2>"$scratch/$probe.stderr"
    [ ! -s "$scratch/$probe.stdout" ] || { echo 'FAIL: a self-check printed response content' >&2; exit 1; }
    refused_command=("${check_command[@]}")
    rewritten=0
    for i in "${!refused_command[@]}"; do
      refused_command[i]="${refused_command[i]//127.0.0.1:8080/127.0.0.1:1}"
      if [[ "${refused_command[i]}" != "${check_command[i]}" ]]; then rewritten=$((rewritten+1)); fi
    done
    [ "$rewritten" = 1 ] || { echo 'FAIL: self-check lacks its unique loopback target' >&2; exit 1; }
    status=0
    docker exec "$name" "${refused_command[@]}" >"$scratch/$probe-refused.stdout" 2>"$scratch/$probe-refused.stderr" || status=$?
    [ "$status" = 7 ] || { echo 'FAIL: the native connection-refusal control was not observed' >&2; exit 1; }
    docker exec "$name" "${check_command[@]}" >"$scratch/$probe-recovered.stdout" 2>"$scratch/$probe-recovered.stderr"
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
printf '%s\n' '{"@type":"upsert","object":"Domain","matchOn":["name"],"value":{"domain":{"name":"example.test"}}}' \
  '{"@type":"upsert","object":"Account","matchOn":["name","domainId"],"value":{"agents":{"@type":"User","name":"agents","domainId":"#domain","permissions":{"@type":"Replace","enabledPermissions":{"jmapEmailImport":true,"jmapEmailCreate":true}}}}}' \
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
for object in accounts certificates applications; do
  jq -s '[.[].id]|sort' "$scratch/$object-before.jsonl" >"$scratch/before.ids"
  jq -s '[.[].id]|sort' "$scratch/$object-after.jsonl" >"$scratch/after.ids"
  cmp "$scratch/before.ids" "$scratch/after.ids"
done
inventory
jq -e '.methodResponses[0][1].total==4' "$scratch/inputs/listener-inventory.json" >/dev/null
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
run_bootstrap
start_server serving
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
for secret in "$scratch/inputs/"*-password; do
  if grep -Fq -f "$secret" "$scratch/server.log"; then
    echo 'FAIL: a generated fixture credential appeared in server logs' >&2; exit 1
  fi
done
if grep -Eqi 'temporary administrator|private-mail-recovery-fixture-only' "$scratch/server.log"; then
  echo 'FAIL: a recovery credential appeared in server logs' >&2; exit 1
fi
echo 'PASS: native mail configuration, receiving continuity and ownership refusal, cached UI upgrade, replay, TLS and account restrictions'
