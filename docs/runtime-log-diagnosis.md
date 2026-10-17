# Finite runtime-log diagnosis

`scripts/analyze-cri-logs` is an offline operator diagnostic for containerd CRI
logs. It pairs `RunPodSandbox` and `PullImage` starts/results within each input
stream, reports slow pairs, and exposes evidence gaps. It does not collect logs,
contact a cluster, retrieve credentials, or change a workload. The experimental
gate is off by default; reassess it by 2026-10-09 after operator use, tracked in
[#4350](https://github.com/devantler-tech/platform/issues/4350).

## Collect a finite private sample

Use an existing authorized Talos reader context. Choose the node targets from a
fresh read-only Kubernetes node snapshot and retain that snapshot privately;
an input label is an operator assertion, not verified node identity. Preserve the
exact requested context, target, command, capture start/end, exit code and stderr
alongside each log. Recheck node identities after collection if an operation is
being joined to a live workload. Do not use an admin fallback or retrieve a new
credential to make the diagnostic work.

For each selected node, collect a finite tail with the normal reader:

```bash
umask 077
talosctl --context "$READER_CONTEXT" --nodes "$NODE_ADDRESS" \
  logs cri --tail 10000 > "$PRIVATE_LOG" 2> "$PRIVATE_STDERR"
```

Record the command's exit code immediately. A failed read must remain a failed
capture, rather than an empty log interpreted as clearance. Use a fresh private
directory; stop a stalled capture rather than starting another watcher. Retain
only the minimum bounded sample needed for the investigation. Never publish raw
logs, roster files, workload identities, registry URLs or detailed reports.

## Analyze the captured files

The command requires explicit opt-in, a window of at most seven days, at least
one unique opaque node label, and a new output file. Use RFC3339 timestamps.
Input files must be private regular files (no group/world permissions or final
symlinks). Limits are 20 files, 8 MiB per file, 64 MiB total, and 1 MiB per line.
The report is created exclusively with mode `0600`; existing output is preserved.

```bash
go run ./scripts/analyze-cri-logs --experimental \
  --from 2026-10-02T10:00:00Z --to 2026-10-02T11:00:00Z \
  --input node-1="$PRIVATE_LOG_1" --input node-2="$PRIVATE_LOG_2" \
  --slow 5s --output "$NEW_PRIVATE_REPORT"
```

`--slow` controls which pairs appear in the sample, with a default of 500 ms.
It is not an SLO setting and changes no alert threshold. Successful and failed
pair totals cover the entire input; `WindowPairs` and `SlowPairs` count returns
inside the requested inclusive window. Starts preceding the window are retained
for pairing. At most 1,000 slow/failed samples are included; truncation is explicit
and full pair counts are preserved. Targets are hashed rather than emitted as
raw messages or image references. Sandbox samples include their Pod UID for
private correlation; verify the exact Pod UID and each controller-owner UID when
joining to a Job or CronJob. A name prefix is insufficient.

Exit codes are:

| Code | Meaning |
| --- | --- |
| 0 | Finite supported records processed without the reported pairing/format/range gaps. |
| 1 | Report written, with gaps or sample truncation. Examine every node's counts. |
| 2 | Analysis refused or input/output failed; no new report is retained. |

JSON envelopes and `time=... level=... msg="..."` logfmt records are supported,
including Talos's node-address prefix. Mixed node prefixes or mixed prefixed and
unprefixed envelopes in one file are refused. A timestamp found only inside a
message does not count. Unrecognized records, known unsupported CRI methods, overlapping
identities, unmatched results/starts and timestamp regressions are counted.
Overlapping requests remain ambiguous until every outstanding result drains;
the analyzer never guesses FIFO ordering. A clock regression invalidates all
pending starts. Malformed records or unrecognized records for the pairable
methods also invalidate pending starts; timings cannot bridge those gaps. Their
identities remain quarantined for the rest of that input, with later starts
counted as `QuarantinedStarts` and results as ambiguous. A retry cannot absorb
the delayed result of an earlier invalidated request. Use a new independent
finite capture for fresh evidence rather than clearing this uncertainty.
Retries after a completed pair can form new independent pairs. The known CRI
method inventory is explicit in the parser; an otherwise structured message
outside that inventory is not classified as a CRI operation.

`Earliest`/`Latest` and `WindowBracketed` describe observed envelope timestamps.
Even a bracketed window does not establish complete capture, absence of dropped
logs, or complete request coverage. Input SHA256 digests bind the report to the
captured bytes, not to an authoritative live node or incident.

## Evaluate as an operator

Compare the finite report with an independent sample or known synthetic timing
before using it to select a repair. A slow CRI RPC is not necessarily the HTTP
request measured by Coroot. Every report therefore states
`CorootAttribution: UNPROVEN`, including an otherwise complete analysis. Request
identity, dependency attribution, the cause, and recovery against the existing
objective require separate evidence. Do not mark an incident resolved, adjust
its objective, or tune runtime configuration based solely on these timings.

Run the regressions with `go test ./scripts/analyze-cri-logs`; required CI runs
the same tests on PR and merge-group revisions.
