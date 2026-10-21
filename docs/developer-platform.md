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

The stateless Backstage portal was retired on 18 October 2026
([#4819](https://github.com/devantler-tech/platform/issues/4819),
[#5042](https://github.com/devantler-tech/platform/issues/5042)). The completion
step removed the namespace retirement journal. The one-shot tooling that
assessed, fenced, performed and completed that retirement, and its admission
policy, were removed afterwards in
[#5159](https://github.com/devantler-tech/platform/issues/5159). Every rule of that
policy acted only while the journal existed. The
[lifecycle decision](adr/backstage-retirement-lifecycle.md) keeps the record of
how it worked.

The portal's database and its storage are retained in the `backstage` namespace
(`k8s/providers/hetzner/apps/retained-backstage-db`). The **Protect Retained
Backstage Storage** workflow and `internal/portalstorage` still guard them. Whether that
data is kept or retired is decided separately
([#5122](https://github.com/devantler-tech/platform/issues/5122)), and
[#4266](https://github.com/devantler-tech/platform/issues/4266) stays open until
then.
