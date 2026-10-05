# KSail analysis runners

The ARC controller and KSail analysis pool are prepared but inactive. Neither
component is referenced by a deployment aggregate, and both HelmReleases are
suspended. Merging their definitions registers no runner, reads no App credential
and provisions no server. This is the preparation slice of #4462, not its runtime
acceptance or a resolution of KSail #7131.

The controller chart and runner-set chart use the same immutable 0.15.0 artifacts.
The runner image is digest-pinned. The controller manages runner sets only in the
analysis namespace, with its listener in the controller namespace; the separate
pool registers only with `devantler-tech/ksail`. It has
zero idle runners and a maximum of one job runner. There is no container mode,
Docker socket, host volume, privileged container or runner API token. Container
jobs and Docker actions are deliberately unsupported.

The analysis toolchain source lives in the KSail repository's
`images/ksail-analysis-runner`. Its
digest-pinned upstream runner, frozen Ubuntu package snapshot and checksum-checked
Go and Node archives supply the desktop compiler and headers without job-time
root access. The publisher builds and exercises the image on pull requests, then
publishes, attests and signs only on a push to KSail main. Both Kyverno and Talos accept
that workflow identity only for the exact analysis image repository. The staged
pool still requires a verified published digest before activation.

The smoke test starts the copied runner and compiles a Go program against GTK
and WebKit as UID/GID 1001 with a read-only root filesystem and no capabilities.
The runner home and temporary files use disposable writable storage. Bootstrap
copies use `cp -R`; preserving the root directory's ownership and timestamps
would fail on a group-writable Kubernetes volume.

## Activation gates

Activation requires a separate reviewed change and all of these proofs:

1. Reuse the platform's existing GitHub management App from
   `infrastructure/github/app`. The `arc-ksail-app` ExternalSecret maps its
   `app_id`, `installation_id` and `pem` properties to ARC's three authentication
   fields. Its namespaced SecretStore authenticates as the dedicated
   `arc-ksail-app` credential-reader ServiceAccount. The OpenBao role grants read
   on that exact credential path; the shared ESO role keeps its existing access.
   Job runners use the chart's separate no-permission identity. Verify the
   current installation covers KSail and grants repository
   Administration read/write before activation. The pool's registration URL
   remains KSail-only; the existing App installation also serves other platform
   consumers. No new App, credential copy, key rotation or permission expansion
   is required. Do not print, check in or mount the private key in a job runner.
2. Establish explicitly approved, isolated analysis capacity. The runner selector
   and toleration name `platform.devantler.tech/ksail-analysis=enabled`; no current
   capacity is assumed to carry that label. New billable capacity requires its
   own approval and an exact deletion/readback plan. Do not label a busy production
   worker to make a pending analysis fit.
3. Verify scheduler reservations, node allocatable capacity, ephemeral storage,
   ResourceQuota and LimitRange. Provisional requests are 12Gi/3 CPU for one runner,
   with limits of 14Gi/3.5 CPU in the analysis namespace. The controller and
   listener each request 256Mi/100m in the separate controller namespace; their
   combined limits are 1Gi/1.25 CPU. These fit the
   current default namespace quota, but are **not a calibrated memory budget**.
   The observed roughly 9.7GB native extractor peak is not aggregate pod memory or
   proof of OOM. Measure total pod/cgroup peak and extraction duration; revise
   sizing and any narrowly justified quota change before admitting real traffic.
4. Render both pinned charts, including their CRDs and namespace-scoped RBAC.
   Confirm the App Secret is absent from runner volumes, the no-permission runner
   service account is used, and the chart preserves the listener name and labels.
   Test admission, API isolation and denied internal egress, not only YAML validity.
5. Prove the actual managed Go and JavaScript Code Quality jobs on an ephemeral
   runner. The official runner image is not a hosted-runner software clone. Check
   the required tools, GitHub proxy, network allowlist and non-root installation
   path; do not enable sudo/privilege escalation to make setup pass. If a dedicated
   reproducible tool image is required, deliver and verify it separately.
6. Read the repository-assigned runner registration, then change managed Code
   Quality to the verified `ksail-code-quality` label and read configuration back.
   Preserve all root, nested-module and desktop extraction/source guards. Meet
   KSail #7131's five consecutive successful managed runs and verify completed
   runner pods and their registration are removed before declaring that bug fixed.

For staged validation, run `go test ./scripts/tests/arc-staging` and build both
component directories directly with `kubectl kustomize`; normal local/prod trees
must continue to contain no ARC resources. The unconditional CI guard runs on
pull requests and merge groups. A deliberate activation revises that guard in
the same reviewed change, alongside its evidence; deleting the guard alone is
not activation proof.

## Rollout and recovery

After the gates above are approved, first reference and unsuspend the controller
in the controller layer and verify it is healthy. That component creates both
namespaces before the chart installs its namespace-scoped RBAC. The controller's
network policy also covers the listener, which ARC creates in that namespace.
Only then reference the pool in
the infrastructure layer, where its external secret store already exists. Keep
the unique analysis label out of ordinary build/test/provider workflows.

To stop admitting jobs, restore managed Code Quality's prior runner configuration
and verify the readback, then use a reviewed values change to set both runner
bounds to zero. Let the current job finish and prove there are no busy runners
before uninstalling the pool. **Suspending a HelmRelease alone does not drain an
installed scale set.** Namespace/release retirement follows the platform's
two-stage persistence protection; never delete a namespace to cancel a job.
Verify no listener, runner or GitHub registration remains before deleting approved
temporary capacity. Remove only the ARC ExternalSecret and its materialized
Secret; retain the shared App credential and its other platform consumers.

Official references: [ARC deployment and security guidance](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets)
and [App authentication](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api).
