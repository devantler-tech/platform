# Native ResourceQuota applicability correction

Trivy 0.74.0 evaluates KSV-0040 against unrelated documents when a scan also
contains a ResourceQuota. The native rule declares its kind selector in
metadata, but its deny body lacks the per-input condition. This adapter adds
only `input.kind == "ResourceQuota"` to that body in a private complete bundle.
It creates no ignore entries and filters no findings.

The wrapper obtains the current official policy bundle into a fresh cache and
verifies every content path and byte. The audited inventory contains 641 files
and 580 Rego modules. An unknown bundle, changed policy, missing disk load,
partial module count, actual policy skip, compiler failure or embedded fallback
refuses the scan. The original bundle remains untouched.

Before every complete native vulnerability and misconfiguration scan, real
engine controls exercise valid and invalid quotas, unrelated Pods, both document
orders, line offsets and mixed Kubernetes/Docker/Terraform inputs. Genuine quota
finding fields and every unrelated finding must remain identical; an invalid
quota must retain a requested native exit status. The complete scan retains its
source paths, reviewed data and ignore inputs, output and finding exit status.
An operational refusal also retains native diagnostics and partial findings.

Run `go test ./scripts/guard-trivy-policy` for local refusal controls. The full
wrapper always runs its native controls. An additional offline diagnostic can
use `PLATFORM_TRIVY_CONTROL_ENGINE` and `PLATFORM_TRIVY_CONTROL_CACHE` to point
at a verified native executable and untouched audited bundle.

The golden rule is an unchanged capture from the MIT-licensed
[Trivy checks](https://github.com/aquasecurity/trivy-checks) bundle. Its source
hash, corrected hash and complete inventory hashes are enforced in the command.
The upstream copyright and license are retained beside the golden input.
When upstream corrects the applicability, remove this adapter after proving the
same native controls. A changed bundle requires a fresh audit; never restore an
older bundle to make validation pass.
