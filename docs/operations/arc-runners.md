# Organization Linux runners

The organization Linux pool is prepared but inactive and remains outside the
deployment aggregate and its HelmRelease is suspended. Production reconciles the
scoped controller and the former analysis release in Flux inventory to preserve
ownership of resources left by failed activation. The former release has explicit
zero minimum and maximum runner bounds. Suspension stops Helm reconciliation,
does not drain an installed scale set, and prevents a failed release from recovering
its readiness status. Native Flux and Helm health checks stay enabled. The protected
deploy proves complete, empty metadata lists for organization runner sets, releases,
credential-sync resources, retained runner children, both runner namespaces' pods
and labeled listeners in the controller namespace before publication and after
reconciliation. The retained namespace may contain only the exact chart-declared
AutoscalingRunnerSet; Helm keeps that declaration with both runner bounds at zero.
After reconciliation the guard also proves the installed controller is fully
rolled out and excludes that retained namespace. Native empty `items: null` is
accepted only with a complete current-revision list; missing or paginated items
cannot prove absence. It requests no App credentials, JIT configuration or full
Pod responses. Registration, execution and cleanup still
need the separate proofs below before KSail #7131 can close.

The controller chart and runner-set chart use the same immutable 0.15.0 artifacts.
The runner image is digest-pinned. The controller manages runner sets only in the
`arc-runners` namespace, with its listener in `arc-systems`. The pool registers
with `devantler-tech` in the dedicated `platform` runner group and exposes the
`platform-linux` scale-set name. Repository access is opt-in through the group;
no existing workflow or managed analysis setting is changed. Other repositories
can opt in without another App or a repository-specific pool. KSail is a proposed
first consumer, not the scope of the capability.

The pool has zero idle runners and a maximum of one job runner **across all
opted-in repositories**, not one runner per repository. There is no container mode,
Docker socket, host volume, privileged container or runner API token. Container
jobs and Docker actions are deliberately unsupported.

KSail's proposed analysis toolchain lives in its `images/ksail-analysis-runner`
source. Its digest-pinned upstream runner, frozen Ubuntu package snapshot and
checksum-checked Go and Node archives provide compiler headers without job-time
root access. The publisher builds and exercises the image on pull requests,
then publishes, attests and signs only on a push to KSail main. Kyverno and Talos
accept that workflow identity only for the exact analysis image repository.
The pool does not select that image until a verified published digest and the
consumer's runtime proof are available.

The toolchain smoke test starts the copied runner and compiles a Go program
against GTK and WebKit as UID/GID 1001, with a read-only root filesystem and no
capabilities. Its runner home and temporary files use disposable writable
storage. Bootstrap copies use `cp -R`; preserving the root directory's ownership
and timestamps would fail on a group-writable Kubernetes volume.

## Activation gates

Activation requires a separate reviewed change and all of these proofs:

1. Reuse the production platform App used for GitHub sign-in, identified by
   `github_app_client_id` in the production bootstrap configuration. This is
   **not** the provider App described in [GitHub management](../github-management.md).
   Organization registration needs
   **Organization permissions → Self-hosted runners → Read and write**, plus the
   existing metadata read permission. Verify the installed permissions before
   requesting changes; preserve its other consumers' permissions. No new App or
   installation is required. Its OAuth client secret is not an App private key.
   The maintainer provisions the existing App's ID, organization installation ID
   and matching private key through the secret system at
   `secret/infrastructure/arc/github-app`. Reuse a securely stored key when
   available; do not assume that GitHub sign-in proves one is available to ARC.
   The `arc-github-app` ExternalSecret maps this entry's `app_id`,
   `installation_id` and `pem` to ARC's three authentication keys. Its namespaced
   SecretStore authenticates as `arc-secret-reader`, bound only to that service
   account in `arc-runners` and read-only access to the single App entry. The
   shared ESO and GitHub-management identities gain no access to this entry.
   Before activation, authenticate with the key and verify that GitHub reports
   the expected App client ID and organization installation. A successful secret
   synchronization alone does not prove the App's identity. Record only the
   verification outcome, never the key or tokens.
   Prove authenticated, encrypted credential transport and protection against
   observation by untrusted workloads, including workloads on the same node.
   Do not activate the store until this transport boundary is verified.
   Reuse does not narrow the shared App's authority: the runner group controls
   job access, not what the App credential can do. Never mount the private key in
   a job runner.
2. An authorized organization operator creates and verifies the `platform`
   runner group **before** enabling the pool. Use **Selected repositories**, not
   all repositories, and select only explicitly approved consumers. Repository
   access alone does not make arbitrary code trusted. Before admitting a public
   repository, prove the group's selected-workflow/ref restrictions admit only
   approved trusted jobs, including the actual managed analysis workflow when
   applicable. Do not guess that workflow's identity or enable public fork jobs.
   If the available policy cannot enforce the required boundary, keep that
   repository excluded. Do not weaken organization-wide runner restrictions or
   disable GitHub-hosted runners. A failed or unauthorized group read is unknown,
   not permission to use the unrestricted default group.
3. Use the declared `autoscale-arc-runners` CX53 pool for isolated organization
   runner capacity. It has a minimum of zero and maximum of one node, sharing the
   unchanged cluster ceiling of nine nodes and account ceiling of ten. The
   selector and NoSchedule taint both name
   `platform.devantler.tech/ci-runner=enabled`; ordinary workloads and the
   warm-capacity buffer do not tolerate that taint. Verify the label and taint
   on the actual autoscaled node. Do not label a busy production worker to make
   a pending analysis fit. After calibration, verify the pool scales back to
   zero. New billable capacity requires its own approval and an exact
   deletion/readback plan; capacity outside these bounds requires its own approval.
4. Verify scheduler reservations, node allocatable capacity, ephemeral storage,
   ResourceQuota and LimitRange. Provisional requests are 12Gi/3 CPU for one runner,
   with limits of 14Gi/3.5 CPU in the runner namespace. The controller and
   listener each request 256Mi/100m in the separate controller namespace; their
   combined limits are 1Gi/1.25 CPU. These fit the
   current default namespace quota, but are **not a calibrated memory budget**.
   The observed roughly 9.7GB native extractor peak is not aggregate pod memory or
   proof of OOM. Measure total pod/cgroup peak and extraction duration; revise
   sizing and any narrowly justified quota change before admitting real traffic.
5. Render both pinned charts, including their CRDs and namespace-scoped RBAC.
   Confirm the App Secret is absent from runner volumes, the no-permission runner
   service account is used, and the chart preserves the listener name and labels.
   Runner storage consists only of a 40Gi disk emptyDir for its home and a 2Gi
   disk emptyDir for temporary files. A restricted init container copies the
   baked runner into the group-writable home volume as UID/GID 1001. Both the
   bootstrap and job container keep their root filesystems read-only and drop
   every capability. Verify those settings on the generated runner pod.
   Test admission, API isolation and denied internal egress, not only YAML validity.
6. Prove each proposed first consumer's actual jobs on an ephemeral runner.
   The official runner image is not a hosted-runner software clone. Check its
   required tools, GitHub proxy, network allowlist and non-root installation
   path; do not enable sudo/privilege escalation to make setup pass. If a dedicated
   reproducible tool image is required, deliver and verify it separately. KSail's
   onboarding must exercise its managed Go and JavaScript Code Quality jobs;
   another consumer is evaluated against its own jobs, not the KSail toolchain.
7. Read back organization registration, scale-set/group membership and the
   group's selected repository/workflow policies. Prove an approved job is
   admitted and a non-selected repository/workflow is denied. Only then may
   an explicitly approved consumer select `platform-linux` for its chosen jobs.
   For KSail, change managed Code Quality to that verified name and read the
   configuration back. Preserve all its root, nested-module and desktop
   extraction/source guards. Meet KSail #7131's five consecutive successful
   managed runs and verify completed runner pods and their registration are
   removed before declaring that bug fixed. Those bug-specific acceptance
   criteria do not route or onboard other repositories automatically.

For staged validation, run `go test ./scripts/tests/arc-staging` and build both
component directories directly with `kubectl kustomize`. Local trees exclude ARC;
production retains the reconciled controller and drained protected legacy analysis resources
without including the organization pool. The unconditional CI guard runs on
pull requests and merge groups. A deliberate activation revises that guard in
the same reviewed change, alongside its evidence; deleting the guard alone is
not activation proof.

## Repository opt-in

Organization registration makes the capability reusable; it does not grant
every repository access or route every workflow to it. Each consumer needs an
explicit group policy entry and a reviewed workflow selection. Check its trusted
code boundary, toolchain, egress requirements and resource use before adding it.
The existing Go/npm allowlist is not a promise to support every build stack.
Docker actions and container jobs remain unsupported.

For an approved ordinary Linux job, the explicit selection is:

```yaml
runs-on: platform-linux
```

Leave other jobs on their existing runners. Managed analysis uses its own
runner setting rather than this workflow example. Adding a second consumer does
not increase the pool's global one-job bound; approve and validate capacity
changes separately. Removing a consumer means restoring its runner selection,
verifying the readback, removing its group access and letting its current job
finish. It does not require retiring the shared platform App.

## Rollout and recovery

After the gates above are approved, verify the scoped controller is healthy and
retire its `platform.devantler.tech/arc-recovery: drain-only` marker in the reviewed
activation change. The recovery guard intentionally refuses a credentialed pool
while that marker remains. That component creates both
namespaces before the chart installs its namespace-scoped RBAC. The controller's
network policy also covers the listener, which ARC creates in that namespace.
Only then reference the pool in
the infrastructure layer, which creates its dedicated secret store and reader
identity. Verify SecretStore and ExternalSecret readiness before admitting jobs. Keep
the opt-in runner name out of workflows that have not completed onboarding.

To stop admitting jobs, restore every consumer's prior runner configuration
and verify the readbacks, then use a reviewed values change to set both runner
bounds to zero. Let the current job finish and prove there are no busy runners
before uninstalling the pool. **Suspending a HelmRelease alone does not drain an
installed scale set.** Namespace/release retirement follows the platform's
two-stage persistence protection; never delete a namespace to cancel a job.
Verify no listener, runner or GitHub registration remains before deleting approved
temporary capacity. Retire only ARC's materialized Secret and dedicated reader
when the pool is removed. Do not revoke the reused platform App or its key without
accounting for its other consumers, including GitHub sign-in. GitHub management
uses a separate App and remains unchanged. Any shared credential change is
maintainer-owned and must account for every consumer.

Official references: [ARC deployment and security guidance](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets)
and [App authentication](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api).
The [runner-group access guide](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access)
describes selected repository/workflow access and the risks of public jobs.
