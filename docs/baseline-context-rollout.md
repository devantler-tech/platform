# Baseline security context rollout

The stored controller templates are the C-0211 measurement surface. Pod admission
defaults do not backfill them. Source-owned template changes travel through the
normal GitOps merge queue, which can deploy them while an engineer's interactive
production credentials remain read-only.

## Longhorn UI canary

The Longhorn HelmRelease supplies `fsGroupChangePolicy: OnRootMismatch` and an
empty container `seLinuxOptions` object through its existing `longhorn-ui`
post-renderer. The empty object leaves SELinux/MCS label selection to the runtime.
The UI has no `fsGroup` and uses only `emptyDir` volumes, so the ownership policy
has no effect on mounted data. Its existing numeric identity, dropped capabilities,
seccomp profile, replica count and volumes remain unchanged.

The real pinned chart test exercises the defaults present and absent, proves
that every other rendered resource is identical, and renders the source rollback.
It fails if a renamed chart target causes the post-renderer to miss the UI.

The shared production deploy action and disaster-recovery rebuild workflow
handle the evidence in deployment order:

1. Before publication, the read-only canary guard checks whether the desired
   source declares the defaults and the stored UI template has both fields plus
   a successful proof for its own UID. Fields alone never disarm verification:
   a failed first proof or a crash before recording must retry. A source rollback
   disarms before reading the workload; a successfully proven deployment does
   not freeze the UI's initial replica count or identity on later chart changes.
2. An existing canary must be Helm-owned, fully ready, non-root, have no init
   containers, and use only ephemeral volumes without an `fsGroup`. API failure is an error, never an
   empty population. A reachable API reporting an uninstalled UI allows the
   initial installation but still requires the next step.
3. After Flux reports the released revision Ready, the guard reads the stored
   defaults and complete rollout status, then watches the template for 30 seconds.
   Missing fields, unhealthy replicas, changed privilege settings or a rewriting
   owner fail the deployment. The output records the observed generation without
   emitting workload environment values or credentials.
4. Only after that proof succeeds, a separate workflow step records the
   `pod-security.devantler.tech/longhorn-ui-baseline-proof` metadata annotation.
   Its JSON Patch atomically tests the UID and resourceVersion from the final
   observed object before recording that UID. A concurrent change or replacement
   rejects the write; no earlier observation can certify a different object.
   The chart does not declare this annotation. Losing the receipt safely requires
   revalidation, and a replacement cannot inherit the prior Deployment's proof.
5. A failed merge-group deployment remains failed and the existing heal job
   restores the current `main` revision. Removing only the two source defaults
   restores the prior rendered resources; it preserves the existing UI hardening.
   A manual CD or rebuild failure requires the normal Git revert and recovery
   path; those workflows do not have the merge-group heal job.

An engineer can reproduce the read-only phases with
`bash scripts/guard-longhorn-ui-baseline-context.sh before-publish --context <read-only-context>`
and `after-reconcile` after deployment. An after-apply result is recorded after
the deploy; the activation commit does not claim evidence from a future rollout.

## Remaining controller population

The namespace-wide `baseline-context-controllers` admission switch remains off.
Enabling it changes every subsequently written eligible template in that
namespace. A blanket restart would roll storage managers and CSI workloads and
can introduce an admission/reconciliation loop for operator-owned resources.
The UI canary does not establish safety for those workloads.

Helm and Git templates have a source-owned route through values or exact
post-renderers. Operator-generated templates instead depend on supported owner
configuration and convergence: admission of a field absent from the owner's
desired state can cause repeated writes. Storage managers, CSI components and
operator-generated workloads are outside the UI canary's ephemeral-volume scope.
Permission to deploy does not prove those compatibility conditions.

For the Coroot operator's workloads, a narrower `baseline-context-coroot` namespace
label scopes the same controller rules to Deployments, StatefulSets and DaemonSets
labelled `app.kubernetes.io/managed-by: coroot-operator`, leaving the namespace's
Helm and Git workloads alone. The `observability` namespace carries it.

`scripts/guard-coroot-baseline-context.sh` is the read-only guard for that label.
The production deploy action runs it in both phases, with one write step between
them:

1. Before publication, it reads the desired `observability` namespace manifest.
   Without the label it disarms without contacting the cluster, so a label
   rollback does not depend on the operator's workloads. With the label, it
   requires the stored population to be exactly the six reviewed templates,
   each controller-owned by the `Coroot` resource and fully ready. An API
   failure is an error, never an empty population.
2. After Flux reports the released revision Ready,
   `scripts/admit-coroot-baseline-context.sh` writes each template that still
   lacks a field. Every read must return exactly the six reviewed templates,
   so it fails before writing anything outside them. The rules act on create
   and update only, and the operator
   writes a template only when its own desired state changes, so the step
   adds one metadata annotation, which sends the object through admission.
   A template written before the policy engine sees the namespace label is
   written again, at most three times in all. It then requires both fields to
   be present and waits up to 15 minutes for
   the rollout of every template it wrote in any round. A template that already carries both
   fields gets no write, so a later deploy restarts nothing.
3. The guard then waits a bounded time for
   all six templates to carry pod-level `fsGroupChangePolicy` and
   `seLinuxOptions` at pod level or on every container, then observes them
   three times over 30 seconds. A changed generation or UID, a removed field,
   lost readiness, or two writes to the same template fail the deployment.

Unlike the UI canary it records no receipt: while the label is declared, every
deployment re-proves convergence, because an operator upgrade can change its
desired state without any change in this repository. The disaster-recovery
rebuild does not run it yet; a fresh ClickHouse needs longer than the guard's
wait to become ready. On a fresh cluster the label exists before the operator
creates its templates, so admission applies at creation without a write.

The two universal gaps are closed: every one of the 38 scanned workloads in the
excluded namespaces carries both fields in its stored spec, measured against live
prod on 2026-09-25 and recorded with the C-0211 sizing in
`pod-security-mutations-unscoped.yaml`. What remains is the privilege-adjacent residual
that keeps C-0211 cluster-wide, tracked in
[issue #3522](https://github.com/devantler-tech/platform/issues/3522). Re-measure the
stored controllers and every regular and init container before narrowing it; a scanner
verdict alone does not show the residual.

The existing namespace inventory test pins the default-off controller rollout.
Its historical demand for post-rollout evidence inside the activation commit is
not a usable ordering for GitOps. A future namespace activation must replace
that assumption with the enforceable preflight and after-apply sequence above,
scoped to the owners it actually changes; changing the inventory pin alone does
not satisfy the rollout contract.
