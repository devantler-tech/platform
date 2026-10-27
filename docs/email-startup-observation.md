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
