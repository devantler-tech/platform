# judge-openbao-health

Decides from one captured observation whether every expected OpenBao server can serve secrets:
initialized, unsealed, on the expected release, in one cluster, with exactly one active server. It
is the health gate a protected single-server replacement procedure runs before each step (#4756).

It reads no cluster and makes no request. The server names and release it expects come only from
its arguments, which the calling procedure takes from the reviewed render. They never come from
the capture.

```sh
go run ./scripts/judge-openbao-health \
  --input capture.json \
  --expect-servers openbao-0,openbao-1,openbao-2 \
  --expect-version 2.6.3 \
  --max-age 5m
```

| Exit | Verdict | Meaning |
|---|---|---|
| 0 | `PROVEN` | Every expected server is healthy and the cluster has exactly one active server. |
| 1 | `HOLD` | A definite finding: a sealed or uninitialized server, the wrong release, split cluster identities, or no active server or more than one. |
| 2 | `UNKNOWN` | The evidence cannot decide: a server is missing, duplicated or unexpected, the capture is stale, from the future or has no start or completion time, or a body is malformed, partial, oversized or ambiguous. Invalid arguments also exit 2. |

`UNKNOWN` outranks `HOLD`, so incomplete evidence never reads as a narrower finding. The command
prints one JSON line naming each failed server and reason. It never prints a health body, a pod
identity or a cluster identity.

**Health is not quorum.** A `PROVEN` verdict says each server answered its health endpoint as
initialized and unsealed. It does not prove Raft membership, quorum or storage health. Those need an
authenticated read and are a separate step of #4756.

**Kubernetes Ready is not an input.** The readiness probe accepts sealed and uninitialized servers
on purpose, so that the initialization job can reach them during bootstrap. Ready therefore says
nothing about whether a server can serve. A successful HTTP status is not enough on its own either:
the body has to parse strictly and agree with it.

## Capture format

The capture is one JSON document. Each server's `health` is the unmodified body of
`GET /v1/sys/health?standbyok=true` read from that server, and `healthStatus` is its HTTP status.
`startedAt` is written before the first read and `capturedAt` when the last one completes. Freshness
is measured from `startedAt`, so even the oldest read in the capture is within `--max-age`.

```json
{
  "startedAt": "2026-10-16T19:57:50Z",
  "capturedAt": "2026-10-16T19:58:00Z",
  "servers": [
    {
      "name": "openbao-0",
      "podUID": "<the pod's UID>",
      "healthStatus": 200,
      "health": { "initialized": true, "sealed": false, "standby": false, "version": "2.6.3", "cluster_id": "<id>" }
    }
  ]
}
```

The judge requires every observation to carry a distinct pod UID, but it cannot see the cluster. The
calling procedure binds those UIDs to the live pods, for example by refusing to replace a server whose
pod UID changed after the capture.

The envelope must carry exactly these fields. A health body may carry any other native fields, but a
duplicate member, or a case variant of a field the judge reads, makes the capture `UNKNOWN`, because
a typed decode would read it ambiguously. The strict-JSON check is shared with the ARC transport
proof in `internal/strictjson`.

Run the regressions with `go test ./scripts/judge-openbao-health ./internal/strictjson`.
