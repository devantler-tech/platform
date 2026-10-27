#!/usr/bin/env bash
# Credentials arrive only through stdin; never enable tracing or export them.
set +x
set -euo pipefail
IFS= read -r task_authorization
IFS= read -r task_pod_ip
[[ "$task_authorization" =~ ^[A-Za-z0-9+/]+={0,2}$ && ${#task_authorization} -le 1024 ]]
[[ "$task_pod_ip" =~ ^[0-9a-fA-F:.]+$ && ${#task_pod_ip} -le 45 ]]
task_pod_url="http://$task_pod_ip:8080"
[[ "$task_pod_ip" != *:* ]] || task_pod_url="http://[$task_pod_ip]:8080"
# This source image was verified to include curl. No request follows redirects.
probe() {
  local task_status
  task_status="$(curl --disable --noproxy '*' --silent --output /dev/null --write-out '%{http_code}' --http1.1 --header 'Connection: close' --header 'Accept: */*' --user-agent 'kube-probe/1.36' --connect-timeout 1 --max-time 1 --proto '=http' "$1" 2>/dev/null)" || task_status=000
  [[ "$task_status" =~ ^[0-9]{3}$ ]]
  printf '%d' "$((10#$task_status))"
}
task_live_loopback="$(probe http://127.0.0.1:8080/healthz/live)"
task_ready_loopback="$(probe http://127.0.0.1:8080/healthz/ready)"
task_live_pod="$(probe "$task_pod_url/healthz/live")"
task_ready_pod="$(probe "$task_pod_url/healthz/ready")"
printf '{"live_loopback":%s,"ready_loopback":%s,"live_pod":%s,"ready_pod":%s,"security":' "$task_live_loopback" "$task_ready_loopback" "$task_live_pod" "$task_ready_pod"
# The authorization is shell memory and curl stdin, never argv, environment or a file.
# Only address/expiry are requested. No configuration, account, log or mail is read.
curl --disable --noproxy '*' --silent --fail --show-error --http1.1 --connect-timeout 1 --max-time 2 --max-filesize 65536 --proto '=http' --config - <<CONFIG
url = "http://127.0.0.1:8080/jmap"
request = "POST"
header = "Authorization: Basic $task_authorization"
header = "Content-Type: application/json"
data = "{\"using\":[\"urn:ietf:params:jmap:core\",\"urn:stalwart:jmap\"],\"methodCalls\":[[\"x:AllowedIp/query\",{\"limit\":257,\"calculateTotal\":true},\"allowIds\"],[\"x:AllowedIp/get\",{\"#ids\":{\"resultOf\":\"allowIds\",\"name\":\"x:AllowedIp/query\",\"path\":\"/ids\"},\"properties\":[\"address\",\"expiresAt\"]},\"allow\"],[\"x:BlockedIp/query\",{\"limit\":257,\"calculateTotal\":true},\"blockIds\"],[\"x:BlockedIp/get\",{\"#ids\":{\"resultOf\":\"blockIds\",\"name\":\"x:BlockedIp/query\",\"path\":\"/ids\"},\"properties\":[\"address\",\"expiresAt\"]},\"block\"]]}"
CONFIG
unset task_authorization
printf '}\n'
