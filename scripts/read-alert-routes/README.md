# Read cluster alert-route configuration

This is the read-only configuration observation for Platform #4683. It does not
activate a publisher, change a detector or send a notification. Run the
**Read Alert Route Configuration** workflow from reviewed main, first attempt,
with confirmation `read-alert-routes`. It shares production deployment
serialization. Other invocations fail before Kubernetes access.

The observer reads the fixed Coroot resource, all Flux notification Providers
and Alerts, all CronJobs, and both fixed Alertmanager pods and loaded status
configurations. Only GET requests are used. No Secret is read. Configuration,
credential references, destination addresses and templates stay in memory.
Public output contains only a fixed verdict and counts.

Two complete observations must agree. Required empty or partial lists,
unreadable/malformed evidence, missing ownership references, unready or replaced
peers, differing loaded peer configurations and changes between observations
return `ALERT_ROUTE_INVENTORY=UNKNOWN` (exit 3). Every response is bounded at
8 MiB and every client process has a deadline. No partial successful receipt
is printed.

`OBSERVED scope=cluster-configuration` describes that bounded configuration
snapshot. The CronJob count covers all active CronJobs; `webhook_candidates` uses
references/text mentioning Slack or webhooks and is deliberately a candidate
count, not an authenticated destination inventory. The Coroot project count
comes from its declarative resource, not its server's internal database.
Private/runtime-local routes and the external heartbeat remain outside this
observer. Those limits must be resolved before claiming the complete live route
inventory or cutting over any route.

Matching Alertmanager loaded configurations is runtime evidence for its two
observed peers. It establishes neither delivery, the current Slack destination
behind a mounted credential, nor inbox/phone receipt. Preserve grouping,
inhibition and recovery behavior during the later reviewed cutover and use the
existing controlled notification proof in #2755.

Offline regression:

```sh
go test ./scripts/read-alert-routes
```
