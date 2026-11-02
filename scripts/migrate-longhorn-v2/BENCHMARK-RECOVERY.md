# Interrupted benchmark ownership

The existing confirmed benchmark workflow remains manual. Before adding node
tags it records the three Longhorn node UIDs. Before each scratch CREATE it
records an unresolved intent, then records the returned object UID before
clearing that intent. Lost responses or failed receipt writes preserve the
unresolved state. A name or label is never a substitute for a missing UID.

Benchmark source and run identities remain frozen. Recovery has a separate
source and run, bound by an atomic journal UID/version claim. Both the original
owner and any previous recovery owner must have a completed latest workflow
attempt before takeover. A stale original run cannot overwrite recovery progress
or turn recovered measurements into a new accepted baseline.

All mutations retain the current-main check. Client refresh continues to stop
when main moves. An interrupted benchmark likewise keeps its original evidence;
a separately confirmed recovery from new current main can use its own source
for cleanup while checking the earlier run against its original source. Moving
main never changes the provenance of measurements.

Recovery helpers currently support only recorded Pods and network policies.
They record exact-UID deletion intents before deletion, use UID/version
preconditions, and preserve those intents across interrupted recovery attempts.
Missing objects without an intent and replacement objects hold the procedure.
The storage graph validator requires the original claim, class, PV and backend
UIDs, their links, the expected engine and retained provisioning settings.

Full recovery is not dispatchable yet. Storage and namespace retirement, complete
foreign-content checks, receipt archival and dispatch integration remain tracked
by [#5665](https://github.com/devantler-tech/platform/issues/5665). Do not run a
benchmark on the assumption that these helpers provide a complete cleanup path.
