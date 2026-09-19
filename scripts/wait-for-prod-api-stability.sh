#!/usr/bin/env bash
set -euo pipefail

# A Talos NO_REBOOT machine-config apply returns before every affected static
# control-plane pod has necessarily restarted. A single successful /readyz read
# can therefore observe the old API process just before it disappears. Require a
# sustained ready window before the deploy resumes Kubernetes mutations.
kubectl_bin="${PROD_API_KUBECTL_BIN:-kubectl}"
attempts="${PROD_API_STABILITY_ATTEMPTS:-60}"
required_successes="${PROD_API_STABILITY_CONSECUTIVE_SUCCESSES:-20}"
interval="${PROD_API_STABILITY_INTERVAL:-2}"

if ! [[ "${attempts}" =~ ^[1-9][0-9]*$ ]] ||
  ! [[ "${required_successes}" =~ ^[1-9][0-9]*$ ]] ||
  ((required_successes > attempts)) ||
  ! [[ "${interval}" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "::error::Production API stability settings must use positive whole-number attempts/successes (successes <= attempts) and a non-negative interval."
  exit 1
fi

result_file="$(mktemp)"
trap 'rm -f "${result_file}"' EXIT

consecutive_successes=0
for ((attempt = 1; attempt <= attempts; attempt++)); do
  if "${kubectl_bin}" \
    --context admin@prod \
    get --raw=/readyz \
    --request-timeout=5s \
    >"${result_file}" 2>&1; then
    consecutive_successes=$((consecutive_successes + 1))
    if ((consecutive_successes >= required_successes)); then
      echo "✅ Production Kubernetes API remained ready for ${required_successes} consecutive probes."
      exit 0
    fi
  else
    if ((consecutive_successes > 0)); then
      echo "::warning::Production Kubernetes API readiness was interrupted after ${consecutive_successes} consecutive probe(s); restarting the stability window."
    fi
    consecutive_successes=0
  fi

  if ((attempt < attempts)); then
    sleep "${interval}"
  fi
done

echo "::error::Production Kubernetes API did not reach a stable ready window (${required_successes} consecutive probes within ${attempts} attempts) after the Talos machine-config update."
exit 1
