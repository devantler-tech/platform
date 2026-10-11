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
