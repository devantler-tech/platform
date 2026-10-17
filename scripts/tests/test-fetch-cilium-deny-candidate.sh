#!/usr/bin/env bash
# The candidate is data. Unknown authors, forks or a moved head must prevent
# any manifest download, not merely prevent cluster startup later.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "${work}/bin"
cat >"${work}/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
endpoint="$2"
echo "$endpoint" >>"${FAKE_STATE}/requests"
if [[ "$endpoint" == repos/devantler-tech/platform/pulls/4381 ]]; then
  author=devantler; repo=devantler-tech/platform; head=01e40ecf795a3eaf6c0e30ba461a7253afb36462; state=open
  case "$FAKE_CASE" in
    author) author=untrusted;; fork) repo=external/fork;; moved) head=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb;; closed) state=closed;; read_error) exit 1;;
    after_read) if grep -Fq /contents/ "${FAKE_STATE}/requests"; then head=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; fi;;
    reread_error) if grep -Fq /contents/ "${FAKE_STATE}/requests"; then exit 1; fi;;
  esac
  jq -n --arg author "$author" --arg repo "$repo" --arg head "$head" --arg state "$state" '{state:$state,user:{login:$author},head:{sha:$head,repo:{full_name:$repo}}}'
elif [[ "$endpoint" == *'/contents/'* ]]; then
  if [[ "$endpoint" == *'add-default-deny.yaml'* ]]; then file="${TEST_ROOT}/k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml"; else file="${TEST_ROOT}/k8s/bases/infrastructure/controllers/oauth2-proxy/cilium-network-policy-default-deny.yaml"; fi
  jq -n --arg content "$(base64 <"$file")" '{type:"file",encoding:"base64",content:$content}'
else exit 90; fi
STUB
chmod +x "${work}/bin/gh"
failures=0
for name in success author fork moved closed read_error after_read reread_error; do
  mkdir -p "${work}/${name}/temp"
  result=0
  PATH="${work}/bin:${PATH}" TEST_ROOT="$root" FAKE_STATE="${work}/${name}" FAKE_CASE="$name" \
    RUNNER_TEMP="${work}/${name}/temp" CANDIDATE_PR=4381 CANDIDATE_HEAD=01e40ecf795a3eaf6c0e30ba461a7253afb36462 \
    bash "${root}/.github/scripts/fetch-cilium-deny-candidate.sh" >"${work}/${name}/output" 2>&1 || result=$?
  expected=1; [[ "$name" != success ]] || expected=0
  if [[ "$result" != "$expected" ]]; then echo "FAIL $name: expected $expected, got $result"; cat "${work}/${name}/output"; failures=$((failures + 1)); fi
  if [[ "$name" == success ]]; then
    receipt="${work}/${name}/temp/cilium-deny-candidate/candidate.json"
    if ! jq -e '.head == "01e40ecf795a3eaf6c0e30ba461a7253afb36462" and .author == "devantler" and (.generator_sha256|length)==64 and (.flux_copy_sha256|length)==64' "$receipt" >/dev/null; then echo 'FAIL candidate receipt'; failures=$((failures + 1)); fi
  elif [[ "$name" != after_read && "$name" != reread_error && -f "${work}/${name}/requests" ]] && grep -Fq '/contents/' "${work}/${name}/requests"; then echo "FAIL $name: fetched before trust/head gate"; failures=$((failures + 1)); fi
  if [[ "$name" != success && -f "${work}/${name}/temp/cilium-deny-candidate/candidate.json" ]]; then echo "FAIL $name: rejected candidate got a receipt"; failures=$((failures + 1)); fi
done
[[ "$failures" == 0 ]] || exit 1
echo 'PASS: only exact same-repo trusted current heads reach fixed candidate data reads'
