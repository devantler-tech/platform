# Organization Linux runners

The production definition includes the ARC controller and bounded organization
Linux pool. The protected deployment must prove the existing runtime App,
registration and isolated canary before activation can merge. Initial access
admits only KSail's main-branch delivery preflight. Managed analysis remains on its
existing route until actual runner delivery and full-source calibration pass;
declaring the pool does not resolve KSail #7131. The former analysis release
reconciles with explicit zero minimum and maximum runner bounds and remains
protected in Flux inventory. The scoped controller excludes its namespace.
Suspension does not drain an installed scale set and prevents failed Helm
readiness from recovering. Native Flux, Helm and orphan checks stay enabled.

The controller chart and runner-set chart use the same immutable 0.15.0 artifacts.
The runner image is digest-pinned. The controller manages runner sets only in the
`arc-runners` namespace, with its listener in `arc-systems`. The pool registers
with `devantler-tech` in the dedicated `platform` runner group and exposes the
`platform-linux` scale-set name. Repository access is opt-in through the group;
no existing workflow or managed analysis setting is changed. Other repositories
can opt in through reviewed access changes without another App or another pool.
KSail is the first selected consumer.

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
The pool selects the published KSail-owned image by immutable digest. The
protected canary verifies its signature, toolchain and resource limits before
any managed-analysis route changes.

The toolchain smoke test starts the copied runner and compiles a Go program
against GTK and WebKit as UID/GID 1001, with a read-only root filesystem and no
capabilities. Its runner home and temporary files use disposable writable
storage. Bootstrap copies use `cp -R`; preserving the root directory's ownership
and timestamps would fail on a group-writable Kubernetes volume.

## Activation gates

Production prepares credential transport separately from runner activation.
The dedicated native TLS listener is served by the already repaired highest
Raft ordinal; the held partition is unchanged. Standby requests use OpenBao's
authenticated cluster connection to reach the leader. The existing API listener,
Raft storage, audit configuration and unseal hook remain in the base configuration.
An additional listener file is appended through the pinned chart.

The TLS-enabled server template declares the
`platform.devantler.tech/arc-transport=tls` label. Both Cilium allow selectors
require this label alongside the server's application and instance identities;
the credential Service retains its exact ordinal selector. Cilium excludes
Kubernetes-generated ordinal labels from security identities, so those labels
cannot select a network-policy endpoint. The protected challenge binds the
native CiliumEndpoint to the current Pod UID and checks that the transport label
is retained before creating probes. Held server replicas without this label
remain outside the TLS allow selectors.

That ConfigMap contains only public settings and paths into a mounted Secret.
Kubescape's generic credential-text rule matches the mandatory `tls_key_file`
setting, so its disposition covers only the named ConfigMap and that control.
Both platform variants enforce its exact public content through admission; source
tests reject drift, and native scanner controls prove genuine credentials remain
findings. The protected transport challenge verifies the installed policy and
configuration, then proves server admission denies synthetic credentials without
writing them to the cluster.

The dedicated certificate authority is trusted only in the runner namespace.
Its issuance policy denies use outside OpenBao. The credential store requires
HTTPS and that authority, with hostname verification and no plaintext fallback.
Authentication staging includes only the dedicated reader ServiceAccount and
SecretStore; it does not synchronize an App key or install a runner pool.
OpenBao's same-identity reload helper has no credential volume or API token.
It signals the server after an unsealed health response so certificate renewal
does not depend on replacing the held Raft replicas.

The pinned-server regression exercises TLS, wrong-name and wrong-authority
rejection, obsolete-protocol rejection and actual certificate reload against
an empty synthetic local store. These checks are not production transport,
stored-key identity, secret synchronization or same-node runner-isolation proof.
Those observations remain mandatory before credential materialization and ARC
activation. The production deployment must confirm the listener and dedicated
store; the protected identity verifier then establishes the actual stored App.

Dispatch `Verify ARC Credential Transport` from the current reviewed main commit
before that identity verification. It uses the established protected production
path and serializes with deployments. The challenge verifies the dedicated
authority, hostname and actual initialized, unsealed canary, then places an
unprivileged, token-free probe on each current OpenBao-canary and secret-controller
node. Both legacy HTTP and native TLS connection attempts must time out and have
a corresponding Hubble `DROPPED / POLICY_DENIED` flow for the exact Pod, addresses,
node, port and attempt window. A certificate error, unreachable healthy control,
observer loss or changed workload identity fails the proof. Server admission
must reject privileged, host-volume, host-network, host-process and `NET_RAW`
variants. Only the owned probe Pods are created; UID-preconditioned deletion and
absence readback are mandatory. This workflow reads no App key and requests no
reader token. Its counts-only result is transport evidence, not stored-key,
runner-registration or managed-analysis evidence.

Activation and subsequent consumer admission require all of these proofs:

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
   Do not materialize the App key or activate runners until this transport
   boundary is verified. Authentication-only staging must use the declared TLS
   endpoint and its dedicated trust bundle.
   Reuse does not narrow the shared App's authority: the runner group controls
   job access, not what the App credential can do. Never mount the private key in
   a job runner.
2. The namespaced `arc-runtime-platform-app` ProviderConfig manages the
   `platform` group using the existing `arc-github-app` Secret in `arc-runners`.
   Its additional JSON credential key preserves ARC's three original keys;
   the private key never moves to the GitHub-management namespace. The provider
   name is distinct because the pinned provider caches configurations by name;
   the registration gate rejects a same-name configuration in another namespace
   or provider kind.
   Verify the observed group **before** delivering a job. Use **Selected repositories**, not
   all repositories, and select only explicitly approved consumers. Repository
   access alone does not make arbitrary code trusted. Before admitting a public
   repository, prove the group's selected-workflow/ref restrictions admit only
   approved trusted jobs, including the actual managed analysis workflow when
   applicable. Do not guess that workflow's identity or enable public fork jobs.
   If the available policy cannot enforce the required boundary, keep that
   repository excluded. Do not weaken organization-wide runner restrictions or
   disable GitHub-hosted runners. A failed or unauthorized group read is unknown,
   not permission to use the unrestricted default group.
   Initial selection is exactly KSail and
   `devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main`.
   The native dynamic Code Quality workflow is not admitted by this initial
   selection. Its identity, supported restrictions and intercepted fork-job
   denial require separate API and runtime proof before routing analysis.
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

For configuration validation, run `go test ./scripts/tests/arc-staging` and build
both component directories directly with `kubectl kustomize`. Local trees exclude
ARC. The unconditional guard permits activation only through the two named
production aggregates, with immutable images and the mandatory protected
runtime canary. It runs on pull requests and merge groups; deleting it is not activation
proof. Credential staging is absorbed by the full pool component at activation,
so the reader and encrypted store have exactly one reconciled declaration.

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

The controller layer creates both namespaces before namespace-scoped RBAC
reconciles. Its network policy also covers the listener. The infrastructure
layer declares the pool, dedicated credential reader and native runner group.
Verify the stored App identity and encrypted credential boundary before
activating this layer. The reviewed activation explicitly retires the controller's
`platform.devantler.tech/arc-recovery: drain-only` marker: that metadata guard
refuses a credentialed pool while the marker remains. Its implementation and
regression tests remain present. The protected canary joins the deployed Flux revision,
current provider/group observations, ARC registration and exact image before
it exercises admission, allowed connectivity, intercepted denials and cleanup.
Verify SecretStore and ExternalSecret readiness before admitting jobs. Keep the
opt-in runner name out of workflows that have not completed onboarding.

The protected canary reserves admission with the fixed `arc-runtime-admission`
ResourceQuota in the runner namespace. Its zero-pod `NotTerminating` scope blocks
new ordinary runner Pods while existing jobs finish naturally. The deadline-bound
probe is outside that scope. The verifier checks retained runner templates, proves
the real runner template is denied by this quota, and waits up to twenty minutes
for existing runners and the dedicated node to drain. It leaves the quota in place
until probe deletion and node cleanup are verified, then deletes only the
invocation-owned quota with UID and resource-version preconditions. It changes no
runner bounds, Helm reconciliation, App permission or capacity ceiling.

A pre-existing quota, ambiguous create, changed quota identity or incomplete probe
cleanup fails closed. An incomplete cleanup retains the fence for recovery. Before
removing a retained fence, verify its invocation ownership, prove that invocation's
probe is absent and the dedicated pool has drained, and use the current UID and
resource version as deletion preconditions. Never remove another invocation's
fence to make a deployment pass. Quota behavior and its deadline scopes are defined
in the [Kubernetes resource quota documentation](https://kubernetes.io/docs/concepts/policy/resource-quotas/).

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
