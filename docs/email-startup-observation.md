# Observe an Email startup

The **Read Email Startup State** workflow takes one bounded sample from the existing
serving container. Use it when the container restarts without useful application
logs. It does not restart the service or change its configuration, data, probes,
network policy or credentials.

Dispatch the reviewed workflow from `main` and enter
`read-email-startup-state`. Only the first attempt is accepted. The workflow uses
the existing protected production environment and serializes with production
deployments. Its fixed target, executable, startup mount and immutable image must
match the contract in `scripts/diagnose-email-startup/main.go`; a later image
requires a reviewed observer update.

Before collecting, the observer waits at most eleven bounded reads for a running
container. It then executes one embedded read-only script and verifies that the
pod, node, container identity, image identity, restart count, start time and public
startup configuration stayed stable. It never retries an unsuccessful exec.

The probe checks process identity, a hash of the already validated public startup
file, effective access to required paths, the datastore lock's device and inode,
the process's whole-file POSIX write locks, management and recovery listener
counts, recovery-mode presence and one unauthenticated loopback readiness status.
Reading kernel lock metadata does not open or lock the datastore. It reads no
Secret, private-key content, mailbox content or environment value. The approved
image supplies the existing Bash, coreutils and curl executables.

Public output is one `MAIL_STARTUP_SAMPLE` value:

| Value | Meaning of this sample |
| --- | --- |
| `UNEXPECTED_RUNTIME_IDENTITY` | The expected process or effective user was absent. |
| `MOUNTED_CONFIGURATION_DIFFERS` | The mounted public startup file differed from the validated ConfigMap. |
| `REQUIRED_RUNTIME_INPUT_UNAVAILABLE` | A required path failed its effective access check. |
| `DATASTORE_NOT_OBSERVED_OPEN` | No matching whole-file datastore write lock was observed. |
| `UNEXPECTED_RECOVERY_MODE` | Recovery mode or its listener was present. |
| `MANAGEMENT_LISTENER_ABSENT` | The management listener was absent. |
| `LOCAL_HEALTH_NOT_READY` | Loopback readiness did not return HTTP 200. |
| `LOCAL_HEALTHY_KUBERNETES_UNREADY` | Loopback readiness passed while Kubernetes readiness was false. |
| `OBSERVED_HEALTHY_SAMPLE` | All of these checks passed for this sample. |
| `INCONCLUSIVE` | A guard, read, identity, completeness or output bound failed. |

Healthy-sample status exits zero. An observed problem exits one; inconclusive
collection exits three. An interrupted job provides no observation. Missing lock
metadata, unreadable kernel tables, truncated responses and changed identities
are inconclusive. A sample does not establish sustained availability, authenticated
administration, mail delivery, full filesystem integrity or platform security.
Use the result to select the next investigation, then verify recovery separately.

Native Linux regression coverage acquires only an owned temporary POSIX lock and
checks its real kernel identity. Other tests exercise the actual embedded probe
and workflow shell steps with synthetic files and credentials, including
replacement identities, finite waiting and private-output refusal.

## Compare health requests and stored access rules

**Read Email Probe State** is a separate opt-in workflow. Dispatch it from reviewed
`main`, first attempt only, with `read-email-probe-state`. It shares the production
deployment lock and retains the fixed serving image, command, mount and runtime
binding. The serving container must have no environment entries or imports,
preventing a shell startup hook from observing its stdin.

This observer sends the existing one-second HTTP/1.1 health requests, with the
native Kubernetes 1.36 `kube-probe/1.36` User-Agent, to loopback and the bound Pod address. The bound
node must run a supported stable 1.36 kubelet before and after the observation;
a changed node identity or version is inconclusive. The header follows the
kubelet minor, independently of the Kubernetes client version. These requests
originate inside the Pod; they do not reproduce a kubelet's actual network path.

Only after verifying the target does the protected workflow read the two named
settings and administrator Secrets. The administrator input retains its existing
`credential` and `password` keys; only `password` is extracted for authentication.
Missing or extra keys are inconclusive. It authenticates the fixed loopback API with
the existing administrator, requesting only allowed/blocked address and expiry
fields. Credentials travel in memory and stdin, never command arguments,
exported environment, files or artifacts. Native stdout/stderr is captured
privately; public output contains only the classifications below. The observer
never changes access rules or application data.

The request queries both address inventories with totals and a fixed limit,
then fetches only those IDs. The decoder refuses incomplete pages, totals over
256, missing or duplicate IDs, failed methods, missing objects, unrequested
fields, invalid expiry data and responses over 64 KiB. A bare get-all call
silently stops at the server's configured get limit, so it is insufficient proof.
Pod identity, targets, public configuration and both Secret identities/versions
are checked again after the single exec. An unsuccessful exec is never retried.

Public output contains `MAIL_PROBE_PATHS` (`ALL_LOCAL_TARGETS_HTTP_200`,
`LOOPBACK_PATH_FAILED` or `POD_ADDRESS_PATH_FAILED`), `MAIL_ACCESS_STATE`
(`STORED_HOST_BLOCK_MATCH`, `STORED_HOST_ALLOW_OVERRIDE`,
`NO_ACTIVE_STORED_BLOCKS` or `OTHER_ACTIVE_STORED_BLOCKS`) and
`MAIL_KUBERNETES_READY` (`TRUE` or `FALSE`). A guard or collection failure emits
only `MAIL_PROBE_STATE=INCONCLUSIVE` and exits three. An observed problem exits
one; a ready sample with successful targets and no active stored blocks exits zero.

The host address is a candidate from bound Pod status. A stored rule match does
not establish the source of a failed kubelet connection or attest the application's
in-memory security cache. The API read is a bounded observation, not an atomic
transaction over concurrently changing application state. Allow rules override
matching blocks, and expired entries are excluded. No classification establishes
sustained recovery or authorizes clearing a block.

The native response fixture was captured from checksum-verified Stalwart 0.16.27
with a synthetic store and a deliberately blocked documentation address.
Regressions exercise the actual embedded shell over HTTP, complete native
query/get schemas, expiry/override precedence, credential framing, dispatch
guards and changed runtime/input identities. Production requires a bound result.
