# Organization Linux runners

The ARC controller and organization Linux pool are prepared but inactive. Neither
component is referenced by a deployment aggregate, and both HelmReleases are
suspended. Merging their definitions registers no runner, reads no App credential
and provisions no server. This is preparation for opt-in organization use (#4529),
not runtime acceptance of #4462 or a resolution of KSail #7131.

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

## Activation gates

Activation requires a separate reviewed change and all of these proofs:

1. Reuse the existing platform GitHub-management App described in
   [GitHub management](../github-management.md). Organization registration needs
   **Organization permissions → Self-hosted runners → Read and write**, plus the
   existing metadata read permission. The maintainer updates the existing App and
   approves its installation's permission request; preserve its other consumers'
   permissions. No new App, installation, private key or secret value is required.
   The `arc-github-app` ExternalSecret reads the existing
   `secret/infrastructure/github/app` KV entry and maps `app_id`,
   `installation_id` and `pem` to ARC's three authentication keys. Its namespaced
   SecretStore authenticates as `arc-secret-reader`, bound only to that service
   account in `arc-runners` and read-only access to the single App entry. The
   shared ESO identity gains no GitHub credential access. Verify the
   installed permission and secret synchronization without printing credentials.
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
3. Establish explicitly approved, isolated CI capacity. The runner selector
   and toleration name `platform.devantler.tech/ci-runner=enabled`; no current
   capacity is assumed to carry that label. New billable capacity requires its
   own approval and an exact deletion/readback plan. Do not label a busy production
   worker to make a pending analysis fit.
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
component directories directly with `kubectl kustomize`; normal local/prod trees
must continue to contain no ARC resources. The unconditional CI guard runs on
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

After the gates above are approved, first reference and unsuspend the controller
in the controller layer and verify it is healthy. That component creates both
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
temporary capacity. Do not revoke or delete the reused platform App credential:
GitHub management remains an independent consumer. Any shared credential change
is maintainer-owned and must account for every consumer.

Official references: [ARC deployment and security guidance](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/deploy-runner-scale-sets)
and [App authentication](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api).
The [runner-group access guide](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access)
describes selected repository/workflow access and the risks of public jobs.
