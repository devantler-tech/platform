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
printf '%s' '{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator"}' >"$scratch/inputs/settings.json"
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
docker build --tag "$name-bootstrap-image" "$scratch/build" >"$scratch/build.log" 2>&1
[ "$(docker image inspect "$name-bootstrap-image" --format '{{.Config.User}}')" = '2000:2000' ] || {
  echo 'FAIL: bootstrap image lost its unprivileged deployment identity' >&2; exit 1;
}

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
  local mode=$1
  local -a recovery=()
  if [ "$mode" = preparation ]; then
    recovery=(--env STALWART_RECOVERY_MODE=1 --env STALWART_RECOVERY_ADMIN=fixture:private-mail-recovery-fixture-only)
  fi
  docker run --detach --name "$name" --network "$network" --read-only \
    --user "$fixture_user" --cpus 1 --memory 512m --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --publish 127.0.0.1::8080 --publish 127.0.0.1::1025 \
    --publish 127.0.0.1::1587 --publish 127.0.0.1::1993 \
    --mount "type=bind,source=$scratch/etc,target=/etc/stalwart,readonly" \
    --mount "type=bind,source=$scratch/data,target=/var/lib/stalwart" \
    --mount "type=bind,source=$scratch/tmp,target=/tmp" \
    "${recovery[@]}" "$server" >/dev/null
  for ((attempt=0;attempt<60;attempt++)); do
    if docker exec "$name" curl --fail --silent --max-time 2 http://127.0.0.1:8080/healthz/ready >/dev/null 2>&1; then
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
inventory() {
  docker exec -i "$name" curl --fail --silent --show-error --max-time 10 \
    --user fixture:private-mail-recovery-fixture-only --header 'Content-Type: application/json' \
    --data-binary @- http://127.0.0.1:8080/jmap <"$root/scripts/private-mail-config/inventory-request.json" \
    >"$scratch/inputs/listener-inventory.json"
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
run_bootstrap
start_server preparation
prepare_plan "$scratch/plan.ndjson"
apply_plan
native_cli query Account --fields id --json >"$scratch/accounts-before.jsonl"
native_cli query Certificate --fields id --json >"$scratch/certificates-before.jsonl"

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
for object in accounts certificates; do
  jq -s '[.[].id]|sort' "$scratch/$object-before.jsonl" >"$scratch/before.ids"
  jq -s '[.[].id]|sort' "$scratch/$object-after.jsonl" >"$scratch/after.ids"
  cmp "$scratch/before.ids" "$scratch/after.ids"
done
inventory
jq -e '.methodResponses[0][1].total==4' "$scratch/inputs/listener-inventory.json" >/dev/null
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
start_server serving
management_port=$(docker port "$name" 8080/tcp | sed 's/.*://')
submission_port=$(docker port "$name" 1587/tcp | sed 's/.*://')
inbound_port=$(docker port "$name" 1025/tcp | sed 's/.*://')
imap_port=$(docker port "$name" 1993/tcp | sed 's/.*://')
agent_password=$(cat "$scratch/inputs/agent-password")
human_password=$(cat "$scratch/inputs/maintainer-password")
administrator_password=$(cat "$scratch/inputs/administrator-password")
printf 'user = "agents@example.test:%s"\n' "$agent_password" >"$scratch/agent.curl"
printf 'user = "maintainer@example.test:%s"\n' "$human_password" >"$scratch/human.curl"
printf 'user = "operator@example.test:%s"\n' "$administrator_password" >"$scratch/administrator.curl"
curl --fail --silent --show-error --max-time 10 --config "$scratch/administrator.curl" "http://127.0.0.1:$management_port/jmap/session" >"$scratch/administrator-session.json"
printf 'From: agents@example.test\r\nTo: maintainer@example.test\r\nSubject: Native policy proof\r\nMessage-ID: <mail-policy@example.test>\r\n\r\nPreserved native mail fixture.\r\n' >"$scratch/known.eml"
smtp=(--silent --show-error --max-time 15 --cacert "$scratch/etc/tls/tls.crt" --resolve "mail.example.test:$submission_port:127.0.0.1" --ssl-reqd --url "smtp://mail.example.test:$submission_port" --mail-from agents@example.test --upload-file "$scratch/known.eml")
curl --fail "${smtp[@]}" --config "$scratch/agent.curl" --mail-rcpt maintainer@example.test >"$scratch/allowed.out" 2>"$scratch/allowed.err"
for recipient in operator@example.test someone@remote.example.test maintainer+tag@example.test; do
  if curl "${smtp[@]}" --config "$scratch/agent.curl" --mail-rcpt "$recipient" >"$scratch/denied.out" 2>"$scratch/denied.err"; then
    echo 'FAIL: agent recipient restriction was bypassed' >&2; exit 1
  fi
  grep -q '550' "$scratch/denied.err"
done
if curl "${smtp[@]}" --mail-rcpt maintainer@example.test >"$scratch/no-auth.out" 2>"$scratch/no-auth.err"; then
  echo 'FAIL: submission accepted an unauthenticated envelope' >&2; exit 1
fi
grep -q '503' "$scratch/no-auth.err"
if curl --silent --show-error --max-time 10 --url "smtp://127.0.0.1:$inbound_port" --mail-from sender@remote.example.test --mail-rcpt someone@other.example.test --upload-file "$scratch/known.eml" >"$scratch/relay.out" 2>"$scratch/relay.err"; then
  echo 'FAIL: unauthenticated relay was accepted' >&2; exit 1
fi
grep -q '550' "$scratch/relay.err"
# Human submission retains normal external sending. The isolated .test
# destination cannot leave this private fixture network.
printf 'From: maintainer@example.test\r\nTo: someone@remote.example.test\r\nSubject: Human submission proof\r\n\r\nDisposable fixture.\r\n' >"$scratch/human.eml"
curl --fail --silent --show-error --max-time 15 --config "$scratch/human.curl" \
  --cacert "$scratch/etc/tls/tls.crt" --resolve "mail.example.test:$submission_port:127.0.0.1" \
  --ssl-reqd --url "smtp://mail.example.test:$submission_port" --mail-from maintainer@example.test \
  --mail-rcpt someone@remote.example.test --upload-file "$scratch/human.eml" >"$scratch/human-send.out" 2>"$scratch/human-send.err"
if curl --fail --silent --show-error --max-time 10 --config "$scratch/agent.curl" \
  --cacert "$scratch/etc/tls/tls.crt" --resolve "mail.example.test:$inbound_port:127.0.0.1" \
  --ssl-reqd --url "smtp://mail.example.test:$inbound_port" --mail-from agents@example.test \
  --mail-rcpt maintainer@example.test --upload-file "$scratch/known.eml" >"$scratch/mx-auth.out" 2>"$scratch/mx-auth.err"; then
  echo 'FAIL: MX offered agent authentication' >&2; exit 1
fi
curl --fail --silent --show-error --max-time 10 --config "$scratch/agent.curl" "http://127.0.0.1:$management_port/jmap/session" >"$scratch/session.json"
account_id=$(jq -er '.primaryAccounts."urn:ietf:params:jmap:mail"' "$scratch/session.json")
jq -cn --arg account "$account_id" '{using:["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],methodCalls:[["Email/query",{accountId:$account},"query"],["Email/import",{accountId:$account,emails:{}},"import"],["Email/set",{accountId:$account,create:{fake:{subject:"private-canary"}}},"set"]]}' >"$scratch/write-request.json"
curl --fail --silent --show-error --max-time 10 --config "$scratch/agent.curl" --header 'Content-Type: application/json' --data-binary "@$scratch/write-request.json" "http://127.0.0.1:$management_port/jmap" >"$scratch/write-result.json"
jq -e '.methodResponses[0][0]=="Email/query" and .methodResponses[1][0]=="error" and .methodResponses[1][1].type=="forbidden" and .methodResponses[2][0]=="error" and .methodResponses[2][1].type=="forbidden"' "$scratch/write-result.json" >/dev/null
upload_status=$(curl --silent --max-time 10 --config "$scratch/agent.curl" --data-binary "@$scratch/known.eml" --output "$scratch/upload-result.json" --write-out '%{http_code}' "http://127.0.0.1:$management_port/jmap/upload/$account_id/")
[ "$upload_status" = 403 ]
curl --fail --silent --show-error --max-time 10 --config "$scratch/human.curl" --cacert "$scratch/etc/tls/tls.crt" --resolve "mail.example.test:$imap_port:127.0.0.1" "imaps://mail.example.test:$imap_port/INBOX;UID=1" >"$scratch/known-read.eml"
grep -q 'Preserved native mail fixture.' "$scratch/known-read.eml"
# Re-enter an explicitly owned recovery process and apply the configuration
# again after real mail exists; account IDs alone are not preservation proof.
docker stop --time 30 "$name" >/dev/null
docker rm "$name" >/dev/null
run_bootstrap
start_server serving
imap_port=$(docker port "$name" 1993/tcp | sed 's/.*://')
curl --fail --silent --show-error --max-time 10 --config "$scratch/human.curl" --cacert "$scratch/etc/tls/tls.crt" --resolve "mail.example.test:$imap_port:127.0.0.1" "imaps://mail.example.test:$imap_port/INBOX;UID=1" >"$scratch/preserved-read.eml"
grep -q 'Preserved native mail fixture.' "$scratch/preserved-read.eml"
docker logs "$name" >"$scratch/server.log" 2>&1
for secret in "$scratch/inputs/"*-password; do
  if grep -Fq -f "$secret" "$scratch/server.log"; then
    echo 'FAIL: a generated fixture credential appeared in server logs' >&2; exit 1
  fi
done
if grep -Eqi 'temporary administrator|private-mail-recovery-fixture-only' "$scratch/server.log"; then
  echo 'FAIL: a recovery credential appeared in server logs' >&2; exit 1
fi
echo 'PASS: native mail configuration, replay, TLS and account restrictions'
