# KSail analysis runners

Production references the ARC controller and KSail analysis pool, with both
HelmReleases unsuspended. The protected deployment must prove registration,
isolation and cleanup at its exact published revision. Managed analysis remains
on its prior configuration until an actual registered KSail job passes. Activation
alone is not calibration or a resolution of KSail #7131.

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
that workflow identity only for the exact analysis image repository. The pool pins
`sha256:1ab01644fd6f67e0b1ab0b456e10b78f58be92ac67e5e06d103cd8c12bbb119b`,
published by KSail main revision
`dbefeb5d46e01b2ffb4ac41e8d94f692a0647323` in
[run 37402941837](https://github.com/devantler-tech/ksail/actions/runs/37402941837).
That run passed the restricted compiler smoke, runtime smoke, anonymous pull and
signature gates. Independent anonymous Cosign verification also bound the digest
to that exact main publisher revision. Publication does not prove registration.

The image smoke test executes the copied runner's version command and compiles a Go program against GTK
and WebKit as UID/GID 1001 with a read-only root filesystem and no capabilities.
The runner home and temporary files use disposable writable storage. Bootstrap
copies use `cp -R`; preserving the root directory's ownership and timestamps
would fail on a group-writable Kubernetes volume.

## Activation gates

Activation uses a separate reviewed change. Complete the preparation checks
first, then prove bounded runtime acceptance and actual job execution before
routing managed analysis:

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
2. Use the declared `autoscale-ksail-analysis` CX53 pool for isolated analysis
   capacity. It has a minimum of zero and maximum of one node, sharing the
   unchanged cluster ceiling of nine nodes and account ceiling of ten. The
   selector and NoSchedule taint both name
   `platform.devantler.tech/ksail-analysis=enabled`; ordinary workloads and the
   warm-capacity buffer do not tolerate that taint. Verify the label and taint
   on the actual autoscaled node. Do not label a busy production worker to make
   a pending analysis fit. After calibration, verify the pool scales back to
   zero; capacity outside these bounds requires its own approval.
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
   Runner storage consists only of a 40Gi disk emptyDir for its home and a 2Gi
   disk emptyDir for temporary files. A restricted init container copies the
   baked runner into the group-writable home volume as UID/GID 1001. Both the
   bootstrap and job container keep their root filesystems read-only and drop
   every capability. Verify those settings on the generated runner pod.
   Publish and verify the KSail-owned analysis image on main before replacing
   the staging image pin.
5. Reference and unsuspend the controller in production's controller layer and
   the pool in production's infrastructure layer. The existing layer dependency
   creates both namespaces and reconciles controller readiness before the pool.
   The protected deployment runs `scripts/verify-ksail-arc-runtime.sh --if-active`
   against its exact published revision. It requires the registered scale-set
   identity and the unchanged node/runner bounds, verifies the image's main
   signing identity and selected amd64 manifest, and starts one bounded probe
   from the actual runner template. The proof checks the complete admitted Pod
   spec and both runtime image identities, quota and allocatable reservations,
   toolchain compilation, absent API token and allowed external HTTPS. Restricted
   admission must reject privileged and host-volume controls. Healthy API and
   internal endpoints must remain unreachable from the probe, with each attempt
   correlated to an actual Cilium policy denial at its source, target, port,
   node and time. A timeout, observer loss or transport failure cannot pass.
   Cleanup atomically binds deletion to the created Pod UID and verifies the
   dedicated pool returns to zero. An unknown create outcome or replaced Pod
   fails cleanup; it never authorizes deletion of an unbound object.
6. Dispatch KSail's main-only `verify-ksail-arc-delivery.yaml` preflight with
   `enable_arc=true`. Join that actual registered job to its restricted runner
   Pod, signed image, allowed network path and cleanup before changing managed
   Code Quality. The image's version command and the platform probe do not prove
   an authenticated runner job. Read the repository-assigned registration,
   then change managed Code Quality to the verified `ksail-code-quality` label
   and read configuration back. Preserve the other setup fields. Check the
   required tools, GitHub proxy, network allowlist and non-root installation
   path; do not enable sudo or privilege escalation to make setup pass.
   Preserve all root, nested-module and desktop extraction/source guards. Meet
   KSail #7131's five consecutive successful managed runs and verify completed
   runner pods and their registration are removed before declaring that bug fixed.

For source validation, run `go test ./scripts/tests/arc-staging` and build both
component directories directly with `kubectl kustomize`. The local tree must
contain no ARC resources; production activation must stay inside the reviewed
envelope. The unconditional CI guard runs on pull
requests and merge groups. It accepts production activation only through both
explicit aggregate references, matching unsuspended releases and one verified
immutable KSail image pin. The protected verifier also renders every production
layer before deciding that source is inactive. Neither guard accepts a partial
activation, hidden legacy reference or waived runtime acceptance.

## Rollout and recovery

Production activation follows steps 1–5; actual job proof and managed calibration
follow in step 6. Managed analysis stays on its previous runner configuration
until the registered-job proof completes. The controller's network policy covers
the listener, which ARC creates in that namespace. Keep the unique analysis label
out of ordinary build/test/provider workflows. The runtime verifier has no live
access while both production references are absent, refuses partial activation,
and runs only inside the protected Platform deployment. Its offline regression
suite exercises the complete lifecycle and intercepted negative controls with
fixtures; those fixtures never count as live acceptance.

To stop admitting jobs, restore managed Code Quality's prior runner configuration
and verify the readback, then use a reviewed values change to set both runner
bounds to zero. Let the current job finish and prove there are no busy runners
before uninstalling the pool. **Suspending a HelmRelease alone does not drain an
installed scale set.** Namespace/release retirement follows the platform's
two-stage persistence protection; never delete a namespace to cancel a job.
Verify no listener, runner or GitHub registration remains before deleting approved
temporary capacity. Remove only the ARC ExternalSecret and its materialized
Secret; retain the shared App credential and its other platform consumers.

Healing to inactive main does not drain or remove installed ARC releases: their
pruning protection retains them. The orphan check must still report those
untracked resources, and the inactive verifier does not establish their runtime
cleanup. A failed or cancelled activation therefore remains on HOLD until a
reviewed protected change declares and drains the retained pool, verifies busy
jobs finish normally, and proves registration and capacity cleanup. Never remove
pruning protection, add an orphan exception or treat source absence as drain proof.

Official references: [ARC deployment and security guidance](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets)
and [App authentication](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api).
