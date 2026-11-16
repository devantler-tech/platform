#!/usr/bin/env bash
# Real imapsync against disposable synthetic TLS peers; no external network.
set -euo pipefail
[[ $# == 1 && $1 =~ ^[a-z0-9][a-z0-9-]*$ ]] || exit 2
image=$1
fixture_dir=$(mktemp -d)
suffix="$$-$RANDOM"
network="email-history-$suffix"
fixture_image="email-history-fixture-$suffix"
source_id=""
target_id=""
cleanup() {
  for container in "$source_id" "$target_id"; do
    if [[ -n "$container" ]]; then docker stop --time 2 "$container" >/dev/null 2>&1 || true; docker rm "$container" >/dev/null 2>&1 || true; fi
  done
  docker network rm "$network" >/dev/null 2>&1 || true
  docker image rm "$fixture_image" >/dev/null 2>&1 || true
  rm -rf "$fixture_dir"
}
trap cleanup EXIT
go run ./scripts/private-mail-config/testdata/historyfixture certificates "$fixture_dir"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$fixture_dir/history-fixture" ./scripts/private-mail-config/testdata/historyfixture
printf '%s' owner@icloud.com > "$fixture_dir/source-username"
printf '%s' password > "$fixture_dir/source-password"
printf '%s' password > "$fixture_dir/destination-password"
printf '%s' ned@devantler.tech > "$fixture_dir/addresses"
cat > "$fixture_dir/imapsync-diagnostic" <<'WRAPPER'
#!/bin/sh
/usr/local/bin/imapsync-real "$@" > /tmp/synthetic-imapsync.log 2>&1
status=$?
cat /tmp/synthetic-imapsync.log
exit "$status"
WRAPPER
cat > "$fixture_dir/Dockerfile" <<'DOCKERFILE'
ARG IMPORT_IMAGE
FROM ${IMPORT_IMAGE}
USER 0
RUN mkdir -p /fixture /private/inputs && chown 2000:2000 /fixture /private /private/inputs && chmod 0700 /fixture /private /private/inputs
RUN mv /usr/local/bin/imapsync /usr/local/bin/imapsync-real
COPY --chmod=0555 imapsync-diagnostic /usr/local/bin/imapsync
COPY --chown=2000:2000 --chmod=0555 history-fixture /usr/local/bin/history-fixture
COPY --chmod=0444 ca.pem /etc/ssl/certs/ca-certificates.crt
COPY --chown=2000:2000 --chmod=0400 server.pem server.key /fixture/
COPY --chown=2000:2000 --chmod=0400 source-username source-password destination-password addresses /private/inputs/
USER 2000:2000
DOCKERFILE
docker buildx build --quiet --platform linux/amd64 --load --build-arg "IMPORT_IMAGE=$image" --tag "$fixture_image" "$fixture_dir" >/dev/null
docker network create --internal "$network" >/dev/null
isolated=(--platform linux/amd64 --network "$network" --read-only --cap-drop ALL --security-opt no-new-privileges --tmpfs "/tmp:rw,noexec,nosuid,mode=1777")
source_id=$(docker run -d "${isolated[@]}" --network-alias imap.mail.me.com --entrypoint /usr/local/bin/history-fixture "$fixture_image" serve source)
target_id=$(docker run -d "${isolated[@]}" --network-alias mail.devantler.tech --entrypoint /usr/local/bin/history-fixture "$fixture_image" serve destination)
for container in "$source_id" "$target_id"; do
  ready=0
  for ((attempt=0; attempt<30; attempt++)); do
    if docker exec "$container" /usr/local/bin/history-fixture probe ready >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  if [[ $ready != 1 ]]; then docker logs "$container" >&2; exit 1; fi
done
for ((pass=1; pass<=2; pass++)); do
  if ! receipt=$(docker run --rm "${isolated[@]}" --entrypoint /bin/sh "$fixture_image" -c '/usr/local/bin/private-mail-bootstrap history --input-dir /private/inputs --confirm copy-icloud-history; status=$?; if [ "$status" != 0 ]; then cat /tmp/synthetic-imapsync.log; fi; exit "$status"' 2>&1); then
    printf 'Synthetic importer did not complete: %s\n' "$receipt" >&2
    exit 1
  fi
  if [[ "$receipt" != 'MAIL_HISTORY_IMPORT=COMPLETE folders=3 messages=4 source=PRESERVED routing=UNCHANGED' ]]; then
    printf 'Unexpected synthetic receipt: %s\n' "$receipt" >&2
    exit 1
  fi
  docker exec "$source_id" /usr/local/bin/history-fixture probe assert
  docker exec "$target_id" /usr/local/bin/history-fixture probe assert
done
printf '%s\n' 'MAIL_HISTORY_NATIVE=PASS copy=VERIFIED duplicates=PRESERVED repeat=IDEMPOTENT source=UNCHANGED destination=PRESERVED'
