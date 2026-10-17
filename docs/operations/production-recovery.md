# Recover production from current main

The merge-group heal records the exact `main` checkout before validation. Manual CD and disaster recovery use the workflow dispatch revision. Recovery requires that validated revision to be the current tip of `main`, with unchanged tracked source files.

The shared deployment checks freshness before changing production. Disaster recovery checks it before creating infrastructure. The publisher repeats the check immediately before moving the production artifact's `latest` tag, after signature, attestation and matcher verification. Each check reads the main ref through the existing read-only workflow token, with a 30-second request bound. Speculative merge-group deployment retains its candidate revision and does not use the recovery check.

`PROD_RECOVERY_SOURCE=STALE` means the checkout or main tip changed. Start a fresh recovery on `main` and run all validation again. Do not switch the existing job's checkout after validation or promote its staged artifact manually. `PROD_RECOVERY_SOURCE=UNKNOWN` means freshness could not be established; repair the failed read before retrying.

The production lock, artifact evidence checks, rollout guards and orphaned-object check remain required. Freshness is checked at each boundary; the GitHub ref read and registry promotion are separate operations. A passing freshness check does not replace the exact published-revision convergence receipt or prove cleanup succeeded.
