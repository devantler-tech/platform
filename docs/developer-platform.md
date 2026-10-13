# Developer platform

GitHub is the portfolio's internal developer platform. An application's owning
repository is its catalog entry; there is no separate component catalog or
hosted portal to maintain.

- Find applications in the [organization's repositories](https://github.com/orgs/devantler-tech/repositories)
  and planned work on the [project board](https://github.com/orgs/devantler-tech/projects/5).
- Start projects from the portfolio's GitHub template repositories. Follow
  [tenant onboarding](TENANTS.md) when the project needs platform hosting.
- Run approved self-service operations from the owning repository's Actions tab,
  using workflows that expose `workflow_dispatch`. Their input validation and
  protected environments govern access to production operations.
- GitHub Actions evaluates application scorecard criteria. Scorecard badges
  belong at the top of each owning repository's `README.md` and link to the
  supporting Actions results. Required CI checks remain merge gates.

The applications-first navigation and central action entry point are tracked in
[the interface spike](https://github.com/devantler-tech/monorepo/issues/3703).
That investigation does not require another hosted portal or a parallel catalog.

## Retiring the previous portal

The **Assess Backstage Retirement** workflow is a read-only production check
for [the retirement issue](https://github.com/devantler-tech/platform/issues/4266).
Dispatch it on the current reviewed `main` commit. It shares the deployment lock
and protected production environment with other maintenance workflows.

The check reads Helm's stored last release, rather than rendering current chart
values. It refuses hooks, persistent or unknown rendered resource kinds,
termination commands and persistent volumes in the stored or live portal
workload. It also checks that retained claims have no owner other than the
separate database, and refuses changing snapshots or missing evidence. Helm's
derived related-PodList collection clock is excluded from the comparison because
unrelated writes advance it; every individual object version and content remains
checked. Every object required by the installed manifest must appear in Helm's
observed resources with a complete identity; a successful partial GET is not
accepted. Missing related-Pod evidence, empty lists and malformed derived lists
still stop the assessment. Output
contains only aggregate counts and fixed result codes; sensitive release values
remain in process memory.

`ASSESSED_STATELESS` describes this bounded footprint, not permission to
uninstall it. Removal still needs a reviewed procedure and live preservation
readback. Database, namespace and volume retirement remain **unassessed**:
this workflow establishes neither independent backup coverage nor offline
retention. It deletes nothing and does not change a reclaim policy.

Run the offline regressions with
`go test -race ./scripts/assess-backstage-retirement`.
`--stdin` evaluates synthetic JSON containing `Release` and `Live` objects,
without cluster access. It requires the same complete manifest and related-Pod evidence as
live assessment; only the live two-snapshot comparison removes collection clocks.
Never persist production release data as a fixture.

The separate **Retire Backstage Stateless Resources** workflow is a manually
confirmed, first-attempt operation on reviewed current `main`. It shares the
protected production environment and deployment lock. The supplied deployed
revision must be joined to an accepted production deployment and fresh OIDC
readback before dispatch; a matching source digest alone is not approval.

The operation re-runs the read-only assessment and additionally requires exactly
the reviewed Deployment, Service and app-config ConfigMap. It refuses a suspended
owner, a missing native uninstall finalizer, active Helm reconciliation, credential
resources in the release, stale source/inventory evidence, foreign chart consumers,
changed snapshots and incomplete retained-data coverage. The controller's latest
history snapshot must match the captured stored release's name, namespace, version
and deployed status, before any write and again before owner deletion. It binds uninstall controls
and deletion to the captured UID/resourceVersion, retains Helm history and disables
hooks. The native unsuspended HelmRelease lifecycle performs the uninstall; only
its separately bound, unreferenced generated HelmChart may be cleaned up afterward.

Two separated post-operation reads require workload/source-object absence and the
same healthy database, namespace, storage, backups and credentials. A bounded
failure or ambiguous write stays HOLD; there is no automatic replay, finalizer
bypass, force replacement or database cleanup. A run-scoped receipt consumes the
attempt before the first lifecycle mutation and stores only scope/identity metadata.
Raw release and Secret evidence remains in process memory, never logs or artifacts.
`RETIRED_STATELESS` is not data-retirement or independent-backup acceptance:
[#4266](https://github.com/devantler-tech/platform/issues/4266) remains open until
those separate requirements are fulfilled.

Run its offline regressions with `go test -race ./scripts/retire-backstage-stateless`.
The native characterization requires an explicitly checksum-verified Helm 4.2.0
binary in `PORTAL_HELM_TEST_BIN`; local default runs report that control as skipped.
CI and the protected workflow require it rather than accepting skipped coverage.
It builds the checksum-locked published controller SDK driver and executes its
actual uninstall/ConfigFactory adapter against a loopback API with synthetic
stored history, delete hooks and delayed resource disappearance. With the
unchanged production default, the controller's [SSA poller](https://github.com/fluxcd/helm-controller/blob/v1.5.5/internal/action/wait.go)
performs the termination wait; a different CLI legacy/watcher implementation is
not substituted. This proves native history/hook/wait behavior, not a real garbage
collector or production controller run. Its [unsuspended finalizer path](https://github.com/fluxcd/helm-controller/blob/v1.5.5/internal/controller/helmrelease_controller.go)
remains protected live acceptance work. The [test-only driver](../scripts/retire-backstage-stateless/poller-native/README.md)
explains the deliberately scoped module import prefix and exact dependency pins.

Reference: [Helm status](https://helm.sh/docs/helm/helm_status/)
reads the installed release; [Helm uninstall](https://helm.sh/docs/helm/helm_uninstall/)
removes that last release's resources. Custom chart extensions and Pod lifecycle
hooks are why disabling the chart's database dependency alone is insufficient.
