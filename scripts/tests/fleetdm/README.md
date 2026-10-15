# Fleet image compatibility

The retained, opt-in Fleet app must prepare its database with the same image as
its server. The regression renders the committed Helm chart and inline values for
install and upgrade, then applies the authored post-renderer patches to regular
manifests with native Kustomize. Helm hooks are excluded, matching Flux 2.8.8's
post-rendering scope. It requires the preparation command and both Deployment image
references to exist and match. It does not start Fleet, contact a database, enable
the app, or establish database/schema or live provider compatibility. Every
charted Fleet image, including hook Jobs, must also match the rendered server;
hooks are checked unchanged rather than incorrectly post-rendered.
The retained disruption budget must also select the rendered server's Pod labels.

CI requires native Helm and kubectl. Run the same check locally with Helm 4.2.0:

```sh
FLEETDM_REQUIRE_NATIVE=1 FLEETDM_HELM_BIN=/path/to/helm \
  FLEETDM_KUBECTL_BIN=/path/to/kubectl \
  go test -race -count=1 ./scripts/tests/fleetdm
```

The chart pull requires network access. A failed pull, render or patch is a
failure, not empty coverage. Fleet's chart and preparation image share a Renovate
group, but grouping alone does not establish compatibility; this native check
remains the merge gate when upstream releases arrive independently.
