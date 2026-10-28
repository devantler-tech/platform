# Observe an Email startup

For a bounded ingress investigation, dispatch **Read Email Ingress Source** from
reviewed `main`, first attempt only, with `read-email-ingress-source`. It retains
the probe observer's private credentials, production serialization and runtime
bindings. It makes two unauthenticated HTTPS readiness requests without redirects
or proxy configuration, then reads at most one bounded native Hubble result from
the pinned Cilium agent on the bound Email node. The filter is a conjunction of
the fixed application, its observed Pod address, TCP management port and reserved
ingress identity. Only ingress SYN flows within the observation window count;
post-SNAT addresses take precedence when the native flow supplies them.

The observer compares these actual peers with complete access-rule reads before
and after observation. Changed rules, agent or application replacement, partial
flows, loss notifications and expiry transitions remain inconclusive. Output
contains only peer classification counts, public HTTP 200/503 counts and the
existing local path classification. It never prints addresses, credentials,
security records or raw flows. An observed sample exits zero; this is evidence
collection and does not declare Email healthy. No matching flow is inconclusive.
The workflow changes no access rules, transport configuration or workload.

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

This observer sends one-second HTTP/1.1 health requests, with the native
Kubernetes 1.36 `kube-probe/1.36` User-Agent, to loopback and the bound Pod address
for investigating node-originating HTTP traffic. The bound
node must run a supported stable 1.36 kubelet before and after the observation;
a changed node identity or version is inconclusive. The header follows the
kubelet minor, independently of the Kubernetes client version. These requests
originate inside the Pod; they do not reproduce a kubelet's actual network path.
The serving self-checks described below execute locally, so these diagnostic
requests do not reproduce their process execution either.

Only after verifying the target does the protected workflow read the two named
settings and administrator Secrets. The administrator input retains its existing
`credential` and `password` keys; only `password` is extracted for authentication.
Missing or extra keys are inconclusive. It authenticates the fixed loopback API with
the existing administrator, requesting allowed address/expiry and blocked
address/reason/creation/expiry fields. Credentials travel in memory and stdin,
never command arguments,
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

The probe observer also reads the Pod's named CiliumNode before and after the
sample. Its sole Node owner must match the bound Node UID, its internal address
must match Pod status's host address, and it must have exactly one distinct,
unicast `CiliumInternalIP` of the Pod's address family. The CiliumNode UID,
resource version and candidate address must remain unchanged. Missing,
ambiguous or changed metadata makes the whole sample inconclusive. This adds
two fixed metadata reads; it does not execute a command in a Cilium container.

Public output contains `MAIL_PROBE_PATHS` (`ALL_LOCAL_TARGETS_HTTP_200`,
`LOOPBACK_PATH_FAILED` or `POD_ADDRESS_PATH_FAILED`), `MAIL_ACCESS_STATE`
(`STORED_HOST_BLOCK_MATCH`, `STORED_HOST_ALLOW_OVERRIDE`,
`NO_ACTIVE_STORED_BLOCKS` or `OTHER_ACTIVE_STORED_BLOCKS`),
`MAIL_CILIUM_ACCESS_STATE` (`STORED_CILIUM_BLOCK_MATCH`,
`STORED_CILIUM_ALLOW_OVERRIDE`, `NO_ACTIVE_STORED_BLOCKS` or
`OTHER_ACTIVE_STORED_BLOCKS`) and
`MAIL_KUBERNETES_READY` (`TRUE` or `FALSE`). A guard or collection failure emits
only `MAIL_PROBE_STATE=INCONCLUSIVE` and exits three. An observed problem exits
one; a ready sample with successful targets and no active stored blocks exits zero.

The host address is a candidate from bound Pod status; the Cilium local address
is a separate routing candidate. Both are classified against the same complete
API response at the same observation time. A stored rule match does
not establish the source of a failed kubelet connection or attest the application's
in-memory security cache. The API read is a bounded observation, not an atomic
transaction over concurrently changing application state. Allow rules override
matching blocks, and expired entries are excluded. No classification establishes
sustained recovery or authorizes clearing a block.

For an effective Cilium candidate match, four additional fixed outputs describe
the stored rule: `MAIL_CILIUM_BLOCK_REASON` (`RCPT_TO_FAILURE`, `AUTH_FAILURE`,
`LOITERING`, `PORT_SCANNING`, `MANUAL` or `OTHER`), `MAIL_CILIUM_BLOCK_SCOPE`
(`SINGLE_IP` or `NETWORK_PREFIX`), `MAIL_CILIUM_BLOCK_EXPIRY` (`FINITE` or
`NO_EXPIRY`) and `MAIL_CILIUM_BLOCK_CREATION` (`BEFORE_CURRENT_POD` or
`DURING_CURRENT_POD`). Creation is compared with the Pod API's creation timestamp,
which must also remain unchanged. Dates, addresses and object IDs are never
printed. A host-length prefix counts as a single IP.

Multiple active matching rules produce `MULTIPLE_MATCHES` on all four fields;
an absent or allow-overridden match produces `NOT_APPLICABLE`. Missing metadata,
an unknown reason or an invalid/future creation time is inconclusive, including
when it belongs to an unmatched rule in the requested inventory. Expired entries
are excluded after validating their metadata. The stored reason is administrator
editable and does not attest an author or an automatic event. Its timestamp is
also a stored observation, not evidence of an actual kubelet connection. These
categories guide the next investigation and authorize no access-rule change.

The native response fixture was captured from checksum-verified Stalwart 0.16.27
with a synthetic store and a deliberately blocked documentation address.
Regressions exercise the actual embedded shell over HTTP, complete native
query/get schemas, expiry/override precedence, credential framing, dispatch
guards and changed runtime/input identities. Production requires a bound result.

## Serving self-checks

Startup, readiness and liveness execute the existing health endpoints over
`127.0.0.1`. Each command starts with an empty environment, uses the image's
Bash and curl, requires HTTP 200 and gives the HTTP request one second. Kubernetes
allows two seconds for process execution, including that request deadline. The
startup period and failure budget remain five seconds and 24 failures.

This binds application self-checks to the serving process while its client-address
security state remains effective. Deployment acceptance also requires sustained
readiness, external reachability and authenticated administration. Native checks
exercise the rendered commands and verify that initialization replay retains
permanent client rules and existing application data.

## Replacing the obsolete probe Pod

An unready Pod can hold an `OrderedReady` StatefulSet on its old revision after
the template changes, as described in the [Kubernetes rollout guidance](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#forced-rollback).
The protected merge-group deployment handles only the transition from this
service's old HTTP probes to its reviewed local self-checks. It runs inside the
existing production deployment lock after a failed reconciliation.

The recovery requires the newly published digest to be adopted and verified,
one unchanged-image serving replica, an old unready revision with repeated
restarts, and the existing bound claim with retained storage and no Pod owner.
It re-reads those observations before requesting one graceful Pod deletion with
both [Kubernetes identity preconditions](https://kubernetes.io/docs/reference/kubernetes-api/definitions/delete-options-v1-meta/).
It neither changes storage nor relaxes the client access rules.

The replacement must have a new identity, the intended revision and probes,
and report ready while the same claim remains bound to the same volume. The
StatefulSet must finish observing that revision, and the application layer must
report both ready and healthy for the same published artifact with no active
reconciliation. Pod readiness alone does not establish that Flux has evaluated
the recovered application layer. Recovery allows up to six minutes for storage
attachment, startup and controller acknowledgement; the probe failure budget is
unchanged.
Unreadable or changed observations, conflicting deletion and timeout fail closed.
Only verified recovery permits one further full reconciliation; any remaining
failure still fails the deployment. Ordinary delivery, recovery and DR receive
no general Pod-replacement permission from this path.

A failed rollback can leave the StatefulSet's last successful revision pointing
at the intended probes while its actual Pod runs the obsolete HTTP checks.
Eligibility therefore binds the actual owned Pod revision and probe contract;
the last successful revision alone does not identify what is currently running.

The tests exercise refusal and retained-identity cases, the actual kubectl
request body, and both conflicting and matching deletion requests against the
existing isolated Kubernetes 1.36.2 API-server fixture. That fixture runs no
scheduler, kubelet or mail process. Sustained production readiness and external,
authenticated administration remain separate deployment acceptance criteria.
