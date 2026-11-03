# Benchmark deletion guard

The admission policy is disabled for every namespace unless it has
`storage.platform.devantler.tech/benchmark-delete-guard: v1`. Only a reviewed
benchmark namespace may receive that label. This change adds no such label and
creates no parameter ConfigMap. It enables no benchmark cleanup operation.

The label is the activation boundary. Kubernetes resolves parameters before
evaluating the secondary benchmark-name condition. Missing parameters therefore
deny DELETEs in any labelled namespace, including one with an unexpected name.
With parameters available, the secondary name condition limits evaluation to
the reviewed benchmark pattern. Never apply the opt-in label to an application
namespace.

In an opted-in benchmark namespace, each native built-in or CRD parent DELETE requires
the old object's UID in the fixed
`longhorn-system/longhorn-benchmark-delete-allowlist` ConfigMap. The value must
equal the current Namespace UID. Missing parameters, unknown objects and a
replaced Namespace deny deletion. The map is runtime-owned; GitOps does not
declare its contents. A future reviewed recovery operation must populate it
only from original private receipts and bind its writes to current source,
workflow ownership and object identities.

Kubernetes' native namespace cleanup uses collection deletion as well as
individual deletion. The guard uses `oldObject.metadata.uid`, since collection
requests have an empty request name. Native built-in and CRD collection
deletion evaluates admission for each object. An unrecorded object can therefore
keep a namespace terminating while recorded objects are removed. The future
recovery controller must treat that condition as HOLD, retain the evidence,
and never remove finalizers to force completion.

This policy does not establish coverage for a remote aggregated API backend.
Complete served-version discovery and metadata inventory must reject unexplained
remote deletion coverage before any Namespace DELETE. The future controller
must also prevent APIService roster changes throughout retirement, or provide
another proven control for that race; discovery readbacks alone do not establish
it. The inventory, default resource provenance, parameter resolution, live positive/negative admission
proof and durable cleanup journal remain required activation gates. A Namespace
inventory or resource-version precondition alone cannot prevent new children
from appearing between inventory and deletion.

The offline regression renders the actual controller profile and evaluates its
CEL expressions through the pinned Kubernetes 1.36.4 admission compiler and
validator, in both new-expression and stored-expression modes. It proves an
exact UID pair is allowed with an empty collection request name and adverse
identities are denied. It also checks the default-off selection and fixed,
fail-closed parameter binding. It starts no cluster and uses no credentials:

```bash
go test -count=1 -timeout=5m ./scripts/tests/longhorn-benchmark-delete-guard
```

The fixture has a separate Go module so the admission server's test dependencies
do not enter production commands. The dynamic Namespace UID field read is
intentional: this Kubernetes release declares `metadata.UID` in its Namespace
CEL type but activates the wire field `metadata.uid`. Direct lowercase access
does not compile; direct uppercase access fails evaluation. The native positive
case protects this workaround from becoming a deny-everything policy.

Native sources: [per-object collection deletion](https://github.com/kubernetes/apiserver/blob/v0.36.4/pkg/registry/generic/registry/store.go),
[CRD storage](https://github.com/kubernetes/apiextensions-apiserver/blob/v0.36.4/pkg/registry/customresource/etcd.go),
[Namespace CEL type](https://github.com/kubernetes/apiserver/blob/v0.36.4/pkg/admission/plugin/cel/compile.go),
[native validation](https://github.com/kubernetes/apiserver/blob/v0.36.4/pkg/admission/plugin/policy/validating/validator.go).
