# Native Coroot dashboard evidence

The hosted workflow runs `run.sh` against the released Coroot 1.22.2 image, an
ephemeral Prometheus and a loopback-only synthetic metrics server. It provisions
the versioned dashboard definitions twice, checks persisted configurations and
an unrelated dashboard, queries every panel through Coroot using the same
five-minute relative time range as the browser, and captures all
three real browser pages with the pinned Playwright CLI. HAR files verify the
page's panel requests returned HTTP 200. Inspect the PNGs before accepting UI
quality; synthetic results say nothing about production platform health.

`synthetic.prom` includes failures, coverage gaps, accepted/skipped outcomes and
unknown scan freshness/runtime delivery. The helper replaces the timestamp
placeholder for each run. It calls the actual shared `bind-project-metrics.sh`
against native Coroot discovery to add the default project's
`coroot_project_id` label, required by Coroot's global Prometheus selector, and
waits for that labeled scrape and actual Coroot range-query chart series before
testing the panels. This accounts for Coroot truncating range endpoints to the
refresh step. One-day net-change panels have no historical baseline
in this short fixture and may show no data; every other panel must return chart
series. No Kubernetes cluster or production credentials are used.

Sources checked for the exact runtime contract:

- [Coroot Docker entrypoint](https://github.com/coroot/coroot/blob/v1.22.2/Dockerfile)
- [Coroot flags](https://github.com/coroot/coroot/blob/v1.22.2/config/flags.go)
- [Coroot default-project bootstrap](https://github.com/coroot/coroot/blob/v1.22.2/config/bootstrap.go)
- [Project selector for the global backend](https://github.com/coroot/coroot/blob/v1.22.2/db/project.go)
- [Native API routes](https://github.com/coroot/coroot/blob/v1.22.2/main.go)
- [Panel response implementation](https://github.com/coroot/coroot/blob/v1.22.2/api/views/dashboards/panel.go)
- [Prometheus range alignment](https://github.com/coroot/coroot/blob/v1.22.2/prom/http.go)
- [Browser routes](https://github.com/coroot/coroot/blob/v1.22.2/front/src/main.js)
- [Playwright 1.56.1 CLI](https://github.com/microsoft/playwright/blob/v1.56.1/packages/playwright-core/src/cli/program.ts)
- [Pinned Playwright registry metadata](https://registry.npmjs.org/playwright/1.56.1)

Coroot listens on `127.0.0.1:18080`, Prometheus on `127.0.0.1:19090` and
synthetic metrics on `127.0.0.1:18081`. Coroot usage statistics and update checks
are disabled. Docker uses host networking only on the isolated hosted runner;
no port is published or bound to a public interface. Cleanup removes only this
invocation's named containers and temporary files, preserving diagnostic logs
and screenshots as seven-day workflow artifacts.

The binary version is verified from its real startup log. Coroot 1.22.2 does
not expose a `--version` option.
