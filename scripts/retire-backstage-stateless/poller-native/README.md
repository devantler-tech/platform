# Native Flux uninstall conformance driver

This is local test support, not an upstream fork, contribution, controller
deployment or production command. The parent regression suite supplies only a
loopback API with synthetic Helm storage and workload objects. The driver refuses
non-loopback destinations and creates no cluster.

The module import prefix deliberately allows the test to call the published
controller's internal `action.Uninstall` and `ConfigFactory` adapter. No upstream
checkout, branch or source overlay is executed. Dependencies are pinned in
`go.mod` and checksum-locked in `go.sum`; runtime build information also verifies
controller/API 1.5.5, Helm 4.2.0 and SSA 0.67.6.

With an unset HelmRelease `waitStrategy`, `disableWait: false` and the default
`UseHelm3Defaults: false`, the real controller selects its SSA poller. Its native
`WaitForDelete` calls `ResourceManager.WaitForTermination` at two-second intervals.
The suite requires that callback to execute on both successful asynchronous
deletion and the timeout control. It separately verifies disabled delete hooks,
retained Helm history, foreground deletion and exact target coverage.

The API simulates delayed disappearance, not a real Kubernetes garbage collector.
This control does not prove a live controller rollout or production retirement.
The required CI/protected test invocation is the parent suite with
`PORTAL_REQUIRE_NATIVE_CONTROL=1` and a checksum-verified Helm 4.2.0 binary in
`PORTAL_HELM_TEST_BIN`; absent required native coverage fails instead of skipping.
