# Privileged RBAC enforcement

The `audit-privileged-rbac` Kyverno ClusterPolicy rejects Role and ClusterRole
rules that grant wildcard administration, RBAC `bind`/`escalate`, or identity
impersonation. Wildcard verbs, resources and API groups are evaluated as effective
permissions, so renaming a chart's administrative role does not hide it.

The policy runs in **Enforce** mode, set on its rule. A create or update that
introduces such a grant is refused at admission, whatever renders it, including
charts that Flux renders in-cluster and CI never sees. Background scanning stays on,
so violations appear in PolicyReports as `fail`. A `resourceNames` restriction is
not treated as proof that a powerful grant is safe.

Objects that legitimately need these grants are listed as **exact-identity
exclusions** in the policy: ClusterRoles by name, Roles by namespace and name. A
different object cannot borrow an exclusion by reusing a name in another namespace
or as another kind, and no exclusion keys on labels, namespace selectors or the
requesting user. The excluded identities are Kubernetes' built-in roles and the
Crossplane, KRO, KSail operator, Longhorn and Velero controllers. Adding one
means changing the policy and the pinned list in
`scripts/tests/test-audit-privileged-rbac.sh` in the same reviewed change.

The excluded names come from the charts and releases that create those roles. A
chart change that renames one of them, or adds a new privileged role, is refused at
admission. Flux dry-runs through the same webhook, so the whole Kustomization fails
until the new role is reviewed and added here.

Kyverno's own resource filters skip `kube-system`, `kube-public`,
`kube-node-lease`, `kyverno` and Kyverno's controller roles for every policy. A
privileged grant created there is not evaluated, so this policy does not cover
those namespaces.

Impersonation is checked against its actual Kubernetes resources: users, groups
and service accounts in the core API group, plus UIDs and user-extra subresources
in `authentication.k8s.io`. These differ from the `roles` and `clusterroles`
resources governing `bind` and `escalate`. Ordinary reads, non-resource health
checks, and empty or not-yet-populated aggregated roles do not trigger findings.
Namespaced Roles are evaluated at their effective scope: they can bind a
ClusterRole through a namespaced binding and impersonate service accounts, but
cannot authorize cluster-wide user impersonation or ClusterRole escalation.
See [Kubernetes impersonation](https://kubernetes.io/docs/reference/access-authn-authz/user-impersonation/)
and [RBAC privilege escalation](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#privilege-escalation-prevention-and-bootstrapping).

The reports controller receives only `get`, `list` and `watch` on Roles and
ClusterRoles. That grant is applied in the controllers layer before the policy's
infrastructure layer. It grants no binding, escalation, impersonation or resource
mutation.

The local regression suite checks ordinary grants, privileged grants and the
exclusion boundaries with the same Kyverno version used in CI and production. A
separate check requires the exact fixture census, the exact exclusion list and
the policy's complete enforcement shape. A policy that stops matching, widens an
exclusion, or weakens enforcement through another field cannot pass. Existing Kubescape scanner exceptions are not admission exceptions.
Production role and binding inventories remain private operator evidence.
