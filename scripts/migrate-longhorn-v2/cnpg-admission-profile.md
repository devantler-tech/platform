# CNPG recovery admission projection

`matchRecoveryAdmission` compares an owned recovery request with the complete
admitted specification for **CNPG 1.30.2 and PostgreSQL 18**. It strips no fields
and returns no partial binding after failure. It neither submits a resource
nor proves source ownership, live readiness, storage allocation or isolation.

Before use, the protected workflow must independently bind the installed CRD
schema and operator image/process to the reviewed version, and establish the
process's startup-loaded configuration. The two monitoring flags are inputs
from that independent evidence, never conclusions drawn from admission output.
Missing facts hold creation.

The independent collector uses the complete native CRD specification, two ready
operator processes, their controlling ReplicaSet and Deployment, and their Node
and boot identities. It reads full namespace inventories before and after the
startup evidence. A missing Pod, partial list, different process, configuration
revision, schema or deployment returns no profile. The collector grants no
storage mutation or creation authority.

The release CRD specification has SHA-256
`55fa73c34cc318c7cd4a757163921e9626e9f604aab9a2308ff33426b574428a`
after Kubernetes adds its native `conversion.strategy: None` default. The hash
uses Go JSON encoding of the complete specification with `Decoder.UseNumber`;
numeric tokens retain their precision. No live fields select the expected hash,
and no native schema fields are removed before comparison. Established and
accepted native conditions and the stored `v1` version are checked separately.

The official `ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2` OCI index is
`sha256:f96f6be3d81ec93819b443c665c575c609342fca840df90d4d87dae314a5b913`.
The supported Linux manifests are
`sha256:7172ea6acc90b0af204e513ee7b5f05fb04a44dcf96a8666655de88511758d7f`
for amd64 and
`sha256:9be85561bd93beb1dfac8c1976562b4acb6585c9b58c0dfd5169fba8d6b85807`
for arm64. A mutable image tag alone does not satisfy the process binding.

The bounded startup prefix must contain the current process's release/build,
named configuration load, and loaded configuration within one minute of its
native start time. The prefix ends at that completed load; later operational
logs are outside this binding. The direct `/manager` command, its environment,
immutable image, container identity and restart count are bracketed separately.
Both processes must agree. This derives monitoring defaults from their actual
startup records rather than from an admitted Cluster or the current ConfigMap
alone.

The startup binding also retains the explicit `createAnyService` boolean and
the fixed `operatorPullSecretName`. This profile requires a successful native
absence read of `cnpg-system/cnpg-pull-secret` in both observations. An existing
credential, failed read or changed startup configuration holds the profile.
These inputs constrain the generated restore resources; they do not prove the
resources actually created by either operator.

The storage observation additionally binds the settled recovery Pod's executable
producer. The supported newly created instance uses the native TLS status-port
command and `info` logging, with the eight fixed PostgreSQL environment values.
One completed bootstrap init container must have the reviewed operator digest
and copy the manager into the same writable scratch volume mounted at
`/controller`. Extra programs, arguments, environments, producer retries and
executable or socket shadow mounts hold the observation. Unsupported operator
environment additions hold rather than selecting a different profile.

The init container's immutable image, container identity and execution times join
the storage observation. Both its epoch and the Pod specification remain bound
on both sides of recovered SQL reads. The native read-only service-account mount
may be present; its account, projection, generated Secrets and RBAC still require
separate qualification. These source-derived checks and synthetic fixtures do not
establish native generated-Pod admission, realized isolation, filesystem placement
or recovery acceptance.

The profile is derived from CNPG commit
`30cca36d32328a78dc96c51db966ce629d5b72db`:

- [CRD schema/defaults](https://github.com/cloudnative-pg/cloudnative-pg/blob/30cca36d32328a78dc96c51db966ce629d5b72db/config/crd/bases/postgresql.cnpg.io_clusters.yaml)
- [Go defaulting](https://github.com/cloudnative-pg/cloudnative-pg/blob/30cca36d32328a78dc96c51db966ce629d5b72db/api/v1/cluster_defaults.go)
- [PostgreSQL configuration construction](https://github.com/cloudnative-pg/cloudnative-pg/blob/30cca36d32328a78dc96c51db966ce629d5b72db/pkg/postgres/configuration.go)

The source-derived fixture
`testdata/synthetic-admitted-recovery-cnpg-1.30.2.json` is **handwritten synthetic
data, not captured admission or deployment evidence**. It covers the ten
renderer fields, native CRD and Go defaults, and 23 default PostgreSQL parameter
strings. CRD defaults run again after webhook patches, retaining schema-defaulted
zero/false fields omitted by Go serialization. Missing parent objects do not
acquire arbitrary documented child defaults.

`archive_mode=on` is a native default; admission does not add `archive_command`.
The separate disabled parameterless cluster Barman entry and absent legacy
backup settings remain mandatory. Actual recovery/archiver behavior still needs
its runtime checks; a configuration comparison alone is not proof of no writes.

Nonempty `MONITORING_QUERIES_CONFIGMAP` and `MONITORING_QUERIES_SECRET` each add
their corresponding custom-query reference. Its namespace-local name is always
`cnpg-default-monitoring` and key `queries`, irrespective of the configured source
object's name. Unexpected enabled `INCLUDE_PLUGINS` entries fail the complete
specification comparison.

The mandatory requested hostname selector is preserved while native preferred
anti-affinity is added. Its hostname/Node/boot/agent binding, complete inventory
uniqueness and actual recovery Pod placement must be proven independently.
Requested identities and labels and the absence of operational annotations,
owners and finalizers are checked. Server-managed metadata is not used as
ownership or readiness proof.

## Barman archive projection

The ObjectStore projection uses Barman Cloud 0.16.0 source commit
`cd3b9faeb18466bc1195688589dc20db2b247f15`. Its complete released CRD
specification, with the same Kubernetes conversion default, has canonical
SHA-256 `750db8dc45db1eeb4f190f3b2d4d46c78f96163790e444a151b4c56b979fe10b`.
The schema identity, stored version and established conditions are bound
separately; a Cluster CRD cannot substitute for an ObjectStore CRD.

The restore proposal contains only the original archive configuration and three
namespace-local credential references. Its absent optional sidecar parent has
no default. Nested interval and log-level defaults therefore add no fields to
this proposal. Admission must preserve its entire specification: a retention
policy, environment override or another unknown field holds creation.

The released maintenance cycle skips maintenance when the cluster backup plugin
is disabled. It also skips remote retention deletion when no retention policy
is specified. The separate external recovery source remains enabled. These
source properties and schema checks establish neither the installed plugin
process nor its actual restore behavior; those require independent native
binding and the protected recovery trial.

- [ObjectStore CRD](https://github.com/cloudnative-pg/plugin-barman-cloud/blob/cd3b9faeb18466bc1195688589dc20db2b247f15/config/crd/bases/barmancloud.cnpg.io_objectstores.yaml)
- [ObjectStore API](https://github.com/cloudnative-pg/plugin-barman-cloud/blob/cd3b9faeb18466bc1195688589dc20db2b247f15/api/v1/objectstore_types.go)
- [Archive maintenance](https://github.com/cloudnative-pg/plugin-barman-cloud/blob/cd3b9faeb18466bc1195688589dc20db2b247f15/internal/cnpgi/instance/retention.go)

The operator image and its explicit `SIDECAR_IMAGE` environment are pinned to
the independently verified 0.16.0 OCI indexes. A mutable ConfigMap lookup cannot
prove the value loaded by an existing process. Native Deployment, ReplicaSet,
Pod and Node identities, health, image, command, environment, container start
and boot identities must agree across complete observations. The sidecar's
actual image and storage graph still need verification after the restore starts.

Ordered creation records the independently observed CNPG and Barman admission
profiles before creating the ObjectStore or Cluster. Server admission must
match the complete reviewed projection before the actual create. Namespace,
credential, original archive and imported isolation policy are rebound before
each write. Each returned UID is recorded before checking its specification
and native readback. An uncertain response remains at the requested phase;
creation cannot replay or adopt a resource by name. These checkpoints establish
neither recovered data nor volume cleanup acceptance.
