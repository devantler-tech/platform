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
checked. Missing related-Pod evidence, empty lists and malformed derived lists
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
without cluster access. It requires the same complete related-Pod evidence as
live assessment; only the live two-snapshot comparison removes collection clocks.
Never persist production release data as a fixture.

Reference: [Helm status](https://helm.sh/docs/helm/helm_status/)
reads the installed release; [Helm uninstall](https://helm.sh/docs/helm/helm_uninstall/)
removes that last release's resources. Custom chart extensions and Pod lifecycle
hooks are why disabling the chart's database dependency alone is insufficient.
