# judge-openbao-quorum

Decides from one captured Raft configuration read whether the next single-server OpenBao
replacement keeps the secrets service writable: every expected server is a voting member of one
cluster, exactly one member is leader, no unexpected member is present, and the voters left while
the named server is out still form a majority. The protected replacement procedure (#4970) runs it
beside [`judge-openbao-health`](../judge-openbao-health/README.md) before each step.

It reads no cluster and makes no request. The server names and the server to replace come only
from its arguments, which the calling procedure takes from the reviewed render. They never come
from the capture.

```sh
go run ./scripts/judge-openbao-quorum \
  --input capture.json \
  --expect-servers openbao-0,openbao-1,openbao-2 \
  --replace openbao-2 \
  --max-age 5m
```

| Exit | Verdict | Meaning |
|---|---|---|
| 0 | `PROVEN` | Every expected server is a voter, exactly one member is leader, nothing unexpected is a member, and quorum survives taking `--replace` out. |
| 1 | `HOLD` | A definite finding: an expected server that is missing or not a voter, an unexpected member, no leader or more than one, or a replacement that would leave fewer voters than quorum. |
| 2 | `UNKNOWN` | The evidence cannot decide: the capture is stale, from the future or has no start or completion time, the read did not return HTTP 200, or the body is malformed, partial, oversized or ambiguous, or a member is listed twice or without its node identity, voter or leader field. Invalid arguments also exit 2, including `-h`, an unknown flag, any flag given twice and a `--replace` that is not one of `--expect-servers`. |

`UNKNOWN` outranks `HOLD`, so incomplete evidence never reads as a narrower finding. The command
prints one JSON line naming each failed expected server and reason. It never prints a response
body, a member address, or the identity of a member it did not expect; unexpected members are
counted instead.

**Quorum here is arithmetic over the reported configuration.** With `n` voters, taking one voter
out leaves `n-1`, and the cluster stays writable while `n-1` is at least `n/2+1` (integer
division). Three voters tolerate one replacement; two do not. An unexpected voter still counts
towards `n`, because Raft counts it, and is reported as a `HOLD` on its own.

**Membership is not health.** A `PROVEN` verdict says the leader lists the expected servers as
voters. It does not say each server is unsealed and serving, which is what the health judge
proves, nor that storage is healthy or tenant secrets are delivered. The procedure needs both
verdicts, read from the same moment, before it replaces anything.

## Capture format

The capture is one JSON document. `configuration` is the unmodified body of an authenticated
`GET /v1/sys/storage/raft/configuration`, and `status` is its HTTP status. `startedAt` is written
before the read and `capturedAt` when it completes. Freshness is measured from `startedAt`. The
body carries no time of its own, so the envelope is the only freshness evidence.

```json
{
  "startedAt": "2026-10-17T19:58:00Z",
  "capturedAt": "2026-10-17T19:58:01Z",
  "status": 200,
  "configuration": {
    "data": {
      "config": {
        "index": 0,
        "servers": [
          { "node_id": "openbao-0", "address": "<address>", "leader": true, "voter": true, "protocol_version": "3" }
        ]
      }
    }
  }
}
```

Node identities equal pod names because the chart sets `setNodeId`, which makes each server's Raft
node ID its pod name.

The envelope must carry exactly these fields. The body may carry any other native fields, but a
duplicate member, or a case variant of a field the command reads, makes the capture `UNKNOWN`. The
strict-JSON check is shared with the health judge in `internal/strictjson`.

Run the regressions with `go test ./scripts/judge-openbao-quorum ./internal/strictjson`.
