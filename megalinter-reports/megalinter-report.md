## ✅⚠️[MegaLinter](https://megalinter.io/9.6.0) analysis: Success with warnings

<details>
<summary>⚠️ BASH / bash-exec - 4 errors</summary>

```
Results of bash-exec linter (version 5.3.9)
See documentation on https://megalinter.io/9.6.0/descriptors/bash_bash_exec/
-----------------------------------------------

❌ [ERROR] .agents/skills/gitops-repo-audit/scripts/check-deprecated.sh
    Error: File:[.agents/skills/gitops-repo-audit/scripts/check-deprecated.sh] is not executable

❌ [ERROR] .agents/skills/gitops-repo-audit/scripts/discover.sh
    Error: File:[.agents/skills/gitops-repo-audit/scripts/discover.sh] is not executable

❌ [ERROR] .agents/skills/gitops-repo-audit/scripts/validate.sh
    Error: File:[.agents/skills/gitops-repo-audit/scripts/validate.sh] is not executable

❌ [ERROR] scripts/ghcr-auth-lib.sh
    Error: File:[scripts/ghcr-auth-lib.sh] is not executable

✅ [SUCCESS] scripts/refresh-flux-ghcr-auth.sh
✅ [SUCCESS] scripts/run-ksail-prod-with-pull-auth.sh
✅ [SUCCESS] scripts/tests/test-cilium-bandwidth-manager-component.sh
```

</details>

<details>
<summary>⚠️ REPOSITORY / checkov - 70 errors</summary>

```
2026-07-21 18:46:55,636 [MainThread  ] [ERROR]  YAML error parsing k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml: expected a single document in the stream
  in "<unicode string>", line 2, column 1
but found another document
  in "<unicode string>", line 9, column 1
cloudformation scan results:

Passed checks: 0, Failed checks: 0, Skipped checks: 0, Parsing errors: 1

kubernetes scan results:

Passed checks: 1660, Failed checks: 39, Skipped checks: 0

Check: CKV_K8S_23: "Minimize the admission of root containers"
	FAILED for resource: Deployment.kube-system.coredns
	File: /k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml:2-125
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-22

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: Deployment.kube-system.coredns
	File: /k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml:2-125
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_11: "CPU limits should be set"
	FAILED for resource: Deployment.kube-system.coredns
	File: /k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml:2-125
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-10

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_25: "Minimize the admission of containers with added capability"
	FAILED for resource: Deployment.kube-system.coredns
	File: /k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml:2-125
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-24

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Deployment.kube-system.coredns
	File: /k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml:2-125
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_35: "Prefer using secrets as files over secrets as environment variables"
	FAILED for resource: Deployment.minio.minio
	File: /k8s/providers/docker/infrastructure/controllers/minio/deployment.yaml:12-93
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-33

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Deployment.minio.minio
	File: /k8s/providers/docker/infrastructure/controllers/minio/deployment.yaml:12-93
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_35: "Prefer using secrets as files over secrets as environment variables"
	FAILED for resource: Job.minio.minio-create-bucket
	File: /k8s/providers/docker/infrastructure/controllers/minio/job.yaml:4-69
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-33

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Job.minio.minio-create-bucket
	File: /k8s/providers/docker/infrastructure/controllers/minio/job.yaml:4-69
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: CronJob.longhorn-system.longhorn-stale-node-cleanup
	File: /k8s/providers/hetzner/infrastructure/controllers/longhorn/cron-job-stale-node-cleanup.yaml:30-188
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_8: "Liveness Probe Should be Configured"
	FAILED for resource: PodTemplate.overprovisioning.overprovisioning
	File: /k8s/providers/hetzner/infrastructure/overprovisioning/pod-template.yaml:29-86
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-7

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_9: "Readiness Probe Should be Configured"
	FAILED for resource: PodTemplate.overprovisioning.overprovisioning
	File: /k8s/providers/hetzner/infrastructure/overprovisioning/pod-template.yaml:29-86
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-8

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Job.userns-longhorn-smoke.userns-longhorn-smoke
	File: /k8s/providers/hetzner/apps/userns-longhorn-smoke/job.yaml:5-154
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_155: "Minimize ClusterRoles that grant control over validating or mutating admission webhook configurations"
	FAILED for resource: ClusterRole.default.cdi-operator-cluster
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5245-5631
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/ensure-clusterroles-that-grant-control-over-validating-or-mutating-admission-webhook-configurations-are-minimized

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_11: "CPU limits should be set"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-10

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_43: "Image should use digest"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-39

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_13: "Memory limits should be set"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-12

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_22: "Use read-only filesystem for containers where possible"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-21

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_15: "Image Pull Policy should be Always"
	FAILED for resource: Deployment.cdi.cdi-operator
	File: /k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml:5855-5963
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-14

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_155: "Minimize ClusterRoles that grant control over validating or mutating admission webhook configurations"
	FAILED for resource: ClusterRole.default.kubevirt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:7390-8724
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/ensure-clusterroles-that-grant-control-over-validating-or-mutating-admission-webhook-configurations-are-minimized

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_11: "CPU limits should be set"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-10

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_43: "Image should use digest"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-39

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_13: "Memory limits should be set"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-12

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_22: "Use read-only filesystem for containers where possible"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-21

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_15: "Image Pull Policy should be Always"
	FAILED for resource: Deployment.kubevirt.virt-operator
	File: /k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml:8741-8875
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-14

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: Job.openbao.vault-snapshot-init
	File: /k8s/bases/infrastructure/vault-backup/job.yaml:23-185
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Job.openbao.vault-snapshot-init
	File: /k8s/bases/infrastructure/vault-backup/job.yaml:23-185
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: CronJob.openbao.vault-snapshot
	File: /k8s/bases/infrastructure/vault-backup/cron-job.yaml:23-190
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: CronJob.openbao.vault-snapshot
	File: /k8s/bases/infrastructure/vault-backup/cron-job.yaml:23-190
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_38: "Ensure that Service Account Tokens are only mounted where necessary"
	FAILED for resource: Job.openbao.vault-config
	File: /k8s/bases/infrastructure/vault-config/job.yaml:36-1182
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-35

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: Job.openbao.vault-config
	File: /k8s/bases/infrastructure/vault-config/job.yaml:36-1182
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_49: "Minimize wildcard use in Roles and ClusterRoles"
	FAILED for resource: Role.github-config.github-config-managed-resources
	File: /k8s/bases/apps/github-config/role.yaml:20-55
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/ensure-minimized-wildcard-use-in-roles-and-clusterroles

		20 | apiVersion: rbac.authorization.k8s.io/v1
		21 | kind: Role
		22 | metadata:
		23 |   name: github-config-managed-resources
		24 |   namespace: github-config
		25 |   labels:
		26 |     app.kubernetes.io/managed-by: ksail
		27 | rules:
		28 |   - apiGroups:
		29 |       - repo.github.m.upbound.io
		30 |       - team.github.m.upbound.io
		31 |       - actions.github.m.upbound.io
		32 |       - enterprise.github.m.upbound.io
		33 |       - github.m.upbound.io
		34 |     resources:
		35 |       - "*"
		36 |     verbs:
		37 |       - get
		38 |       - list
		39 |       - watch
		40 |       - create
		41 |       - update
		42 |       - patch
		43 |       - delete
		44 |   - apiGroups:
		45 |       - external-secrets.io
		46 |     resources:
		47 |       - externalsecrets
		48 |     verbs:
		49 |       - get
		50 |       - list
		51 |       - watch
		52 |       - create
		53 |       - update
		54 |       - patch
		55 |       - delete

Check: CKV_K8S_35: "Prefer using secrets as files over secrets as environment variables"
	FAILED for resource: CronJob.umami.umami-provision-tenants
	File: /k8s/bases/apps/umami/cron-job.yaml:44-311
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-33

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_40: "Containers should run as a high UID to avoid host conflict"
	FAILED for resource: CronJob.umami.umami-provision-tenants
	File: /k8s/bases/apps/umami/cron-job.yaml:44-311
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-37

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
Check: CKV_K8S_22: "Use read-only filesystem for containers where possible"
	FAILED for resource: CronJob.umami.umami-provision-tenants
	File: /k8s/bases/apps/umami/cron-job.yaml:44-311
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/kubernetes-policies/kubernetes-policy-index/bc-k8s-21

		Code lines for this resource are too many. Please use IDE of your choice to review the file.
secrets scan results:

Passed checks: 0, Failed checks: 31, Skipped checks: 0

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/actual-budget/external-secret-enablebanking.yaml:18-19
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		18 |     - secretKey: app**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/actual-budget/external-secret.yaml:15-16
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		15 |     - secretKey: cli**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/actual-budget/helm-release.yaml:227-228
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		227 |           clientSecretKey: cli**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/ascoachingogvaner/external-secret.yaml:34-35
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		34 |     - secretKey: dock**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/crossview/external-secret.yaml:44-45
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		44 |     - secretKey: db**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/crossview/external-secret.yaml:52-53
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		52 |     - secretKey: adm**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/crossview/external-secret.yaml:56-57
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		56 |     - secretKey: oidc**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/fleetdm/external-secret-fleet-license.yaml:16-17
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		16 |     - secretKey: li**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/fleetdm/external-secret-mysql.yaml:19-20
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		19 |     - secretKey: mysq**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/fleetdm/external-secret-mysql.yaml:23-24
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		23 |     - secretKey: mysql-**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/fleetdm/external-secret-mysql.yaml:27-28
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		27 |     - secretKey: mys**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/fleetdm/external-secret-redis.yaml:16-17
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		16 |     - secretKey: red**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/fleetdm/helm-release.yaml:328-329
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		328 |         existingSecretPasswordKey: red**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/headlamp/external-secret.yaml:15-16
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		15 |     - secretKey: cli**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/umami/helm-release.yaml:118-119
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		118 |         existingSecret: umam**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/apps/wedding-app/external-secret-ghcr-auth.yaml:34-35
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		34 |     - secretKey: dock**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/controllers/dex/external-secret.yaml:30-31
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		30 |     - secretKey: cli**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/controllers/velero/helm-release.yaml:108-109
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		108 |       existingSecret: veler**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/external-secrets/ghcr-auth.yaml:25-26
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		25 |     - secretKey: dock**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml:166-167
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		166 |             - secretKey: dock**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-config/external-secret.yaml:15-16
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		15 |     - secretKey: dex-**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-seed/push-secret-seed-actual-budget-enablebanking.yaml:26-27
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		26 |         secretKey: app**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-seed/push-secret-seed-oauth2-proxy-cookie-secret.yaml:17-18
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		17 |         secretKey: oauth2**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-seed/push-secret-seed-r2-credentials.yaml:44-45
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		44 |         secretKey: r2_a**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-seed/push-secret-seed-r2-credentials.yaml:49-50
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		49 |         secretKey: r2_se**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-seed/push-secret-seed-velero-repo-credentials.yaml:36-37
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		36 |         secretKey: repo**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/bases/infrastructure/vault-seed/secret-actual-budget-encryption-placeholder.yaml:25-26
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		25 |   password: PL**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/providers/hetzner/apps/unifi/external-secret-wireguard.yaml:26-27
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		26 |     - secretKey: pr**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/providers/hetzner/apps/unifi/external-secret-wireguard.yaml:30-31
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		30 |     - secretKey: pee**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/providers/hetzner/infrastructure/cluster-issuers/cloudflare-api-token-external-secret.yaml:15-16
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		15 |     - secretKey: ap**********

Check: CKV_SECRET_6: "Base64 High Entropy String"
	FAILED for resource: HIDDEN_BY_MEGALINTER	File: /k8s/providers/hetzner/infrastructure/external-dns/external-secret.yaml:24-25
	Guide: https://docs.prismacloud.io/en/enterprise-edition/policy-reference/secrets-policies/secrets-policy-index/git-secrets-6

		24 |     - secretKey: ap**********

github_actions scan results:

Passed checks: 131, Failed checks: 0, Skipped checks: 1
```

</details>

<details>
<summary>⚠️ SPELL / cspell - 1624 errors</summary>

```
e]
docs/github-management.md:6:11      - Unknown word (upjet)      -- [provider-upjet-github](https://github
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
docs/github-management.md:7:62      - Unknown word (Crossplane) -- manifest, merge, and Flux + Crossplane converge
	 Suggestions: [Cropland, Crosspiece]
docs/github-management.md:11:3      - Unknown word (devantler)  -- [`devantler-tech/.github`](https
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/github-management.md:14:2      - Unknown word (Kustomization) -- `Kustomization`) — only the artifact
	 Suggestions: [Customization]
docs/github-management.md:15:40     - Unknown word (Kustomization) -- **no** bespoke Flux Kustomization for GitHub; it rides
	 Suggestions: [Customization]
docs/github-management.md:21:15     - Unknown word (upbound)       -- (`repo.github.upbound.io`) and **namespaced
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
docs/github-management.md:21:63     - Unknown word (upbound)       -- namespaced** (`repo.github.m.upbound.io`,
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
docs/github-management.md:22:1      - Unknown word (Crossplane)    -- Crossplane v2). We use the **namespaced
	 Suggestions: [Cropland, Crosspiece]
docs/github-management.md:36:3      - Unknown word (Crossplane)    -- | Crossplane core
	 Suggestions: [Cropland, Crosspiece]
docs/github-management.md:36:102    - Unknown word (crossplane)    -- infrastructure/controllers/crossplane/`
	 Suggestions: [cropland, crosspiece]
docs/github-management.md:36:246    - Unknown word (crossplane)    -- inactive. Installs the pkg.crossplane.io CRDs.
	 Suggestions: [cropland, crosspiece]
docs/github-management.md:37:90     - Unknown word (crossplane)    -- hetzner/infrastructure/crossplane/`
	 Suggestions: [cropland, crosspiece]
docs/github-management.md:37:298    - Unknown word (Coroot)        -- needs its CRDs), like Coroot/Flagger CRs. Establishes
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/github-management.md:38:246    - Unknown word (Kustomization) -- verified `OCIRepository` + `Kustomization`. Applied by the existing
	 Suggestions: [Customization]
docs/github-management.md:38:298    - Unknown word (Kustomization) -- existing `apps` Flux Kustomization. Prod-only in practice
	 Suggestions: [Customization]
docs/github-management.md:39:54     - Unknown word (devantler)     -- | [`devantler-tech/.github`](https
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/github-management.md:39:376    - Unknown word (devantler)     -- artifact to `ghcr.io/devantler-tech/github-config/manifests
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/github-management.md:45:26     - Unknown word (Kustomization) -- fresh install the app's `Kustomization` retries benignly until
	 Suggestions: [Customization]
docs/github-management.md:52:1      - Unknown word (Velero)        -- Velero), so the manually-set
	 Suggestions: [valero, Valero, velcro, Velcro, Veer]
docs/github-management.md:56:35     - Unknown word (devantler)     -- GitHub App** on the devantler-tech org (Settings
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/github-management.md:83:5      - Unknown word (Crossplane)    -- let Crossplane *create* what already
	 Suggestions: [Cropland, Crosspiece]
docs/github-management.md:86:45     - Unknown word (upbound)       -- apiVersion: repo.github.m.upbound.io/v1alpha1`), external
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
docs/github-management.md:108:42    - Unknown word (crossplane)    -- hetzner/infrastructure/crossplane/managed-resource-activation
	 Suggestions: [cropland, crosspiece]
docs/github-management.md:109:30    - Unknown word (repositoryrulesets) -- plural.group form, e.g. `repositoryrulesets.repo.github.m.upbound
	 Suggestions: []
docs/github-management.md:109:63    - Unknown word (upbound)            -- ositoryrulesets.repo.github.m.upbound.io`); add
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
docs/github-management.md:113:71    - Unknown word (apiserver)          -- active MRD costs the apiserver
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, iserver]
docs/github-management.md:129:19    - Unknown word (openbao)            -- cluster-scoped `openbao` ClusterSecretStore
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/github-management.md:130:3     - Unknown word (Kyverno)            -- Kyverno policy enforces this
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/github-management.md:133:4     - Unknown word (crossplane)         -- `crossplane-system` CiliumNetworkPolicy
	 Suggestions: [cropland, crosspiece]
docs/node-autoscaling.md:8:60      - Unknown word (ksail)      -- true` is set in the ksail config.
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/node-autoscaling.md:28:16     - Unknown word (cooldown)   -- configurable cooldown.
	 Suggestions: [comedown, codedown, codeDown, Codedown, CodeDown]
docs/node-autoscaling.md:42:4      - Unknown word (ksail)      -- `ksail.prod.yaml`, not in Flux
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/node-autoscaling.md:63:40     - Unknown word (ksail)      -- configuration lives in `ksail.prod.yaml` under
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/node-autoscaling.md:92:120    - Unknown word (ksail)      -- static baseline (see [ksail#5017](https://github
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/node-autoscaling.md:118:4     - Unknown word (ksail)      -- [ksail#6172](https://github
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/node-autoscaling.md:125:61    - Unknown word (KSAIL)      -- list, so the pinned `KSAIL_VERSION` must
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
docs/node-autoscaling.md:137:37    - Unknown word (descheduler) -- rebalancing & consolidation (descheduler)
	 Suggestions: []
docs/node-autoscaling.md:143:1     - Unknown word (Karpenter)   -- Karpenter that gap is closed by
	 Suggestions: []
docs/node-autoscaling.md:143:68    - Unknown word (Karpenter)   -- consolidation* feature, but **Karpenter has
	 Suggestions: []
docs/node-autoscaling.md:145:17    - Unknown word (Descheduler) -- [Kubernetes SIG Descheduler](https://github.com
	 Suggestions: [Scheduler]
docs/node-autoscaling.md:146:14    - Unknown word (descheduler) -- instead: the descheduler evicts misplaced pods
	 Suggestions: []
docs/node-autoscaling.md:151:52    - Unknown word (descheduler) -- infrastructure/controllers/descheduler/`), as a
	 Suggestions: []
docs/node-autoscaling.md:152:42    - Unknown word (descheduling) -- Deployment on a 30-minute descheduling loop. Enabled strategies
	 Suggestions: []
docs/node-autoscaling.md:170:69    - Unknown word (CNPG)         -- PodsWithPVC` protection): CNPG
	 Suggestions: [cnp, CNP, NCP, PNG, CAPE]
docs/node-autoscaling.md:171:42    - Unknown word (openbao)      -- attached workloads, openbao, spire-server. Stateful
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/node-autoscaling.md:179:52    - Unknown word (Coroot)       -- Kubernetes Events (visible in Coroot); watch for pods
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/node-autoscaling.md:207:1     - Unknown word (talosctl)     -- talosctl -n <node-ip> health
	 Suggestions: [talos]
docs/node-autoscaling.md:210:1     - Unknown word (talosctl)     -- talosctl validate --config worker
	 Suggestions: [talos]
docs/node-autoscaling.md:248:35    - Unknown word (burstable)    -- realistic for compute-only/burstable workloads.
	 Suggestions: [burnable, burble, bursae, bursal, bustle]
docs/oidc-kubectl.md:3:34      - Unknown word (kubelogin)  -- explains how to use [`kubelogin`](https://github.com
	 Suggestions: []
docs/oidc-kubectl.md:5:191     - Unknown word (kubeconfig) -- root client-certificate kubeconfig kept in the vault (see
	 Suggestions: []
docs/oidc-kubectl.md:9:26      - Unknown word (kubeconfig) -- Access to the cluster (kubeconfig with server address
	 Suggestions: []
docs/oidc-kubectl.md:10:46     - Unknown word (devantler)  -- is a member of the [`devantler-tech`](https://github
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/oidc-kubectl.md:13:16     - Unknown word (kubelogin)  -- ## 1 — Install kubelogin
	 Suggestions: []
docs/oidc-kubectl.md:17:21     - Unknown word (kubelogin)  -- brew install int128/kubelogin/kubelogin
	 Suggestions: []
docs/oidc-kubectl.md:17:31     - Unknown word (kubelogin)  -- install int128/kubelogin/kubelogin
	 Suggestions: []
docs/oidc-kubectl.md:19:11     - Unknown word (krew)       -- # Or with krew
	 Suggestions: [knew, kew, brew, crew, drew]
docs/oidc-kubectl.md:20:9      - Unknown word (krew)       -- kubectl krew install oidc-login
	 Suggestions: [knew, kew, brew, crew, drew]
docs/oidc-kubectl.md:25:28     - Unknown word (kubeconfig) -- Add an OIDC user to kubeconfig
	 Suggestions: []
docs/oidc-kubectl.md:40:60     - Unknown word (CAROOT)     -- data=$(cat "$(mkcert -CAROOT)/rootCA.pem" | base
	 Suggestions: [CAHOOT, CARLOT, CARNOT, CARROT, CHROOT]
docs/oidc-kubectl.md:45:14     - Unknown word (CAROOT)     -- > `$(mkcert -CAROOT)/rootCA.pem`.
	 Suggestions: [CAHOOT, CARLOT, CARNOT, CARROT, CHROOT]
docs/oidc-kubectl.md:47:35     - Unknown word (devantler)  -- Production cluster (`platform.devantler.tech`)
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/oidc-kubectl.md:89:20     - Unknown word (kubelogin)  -- On the first run, `kubelogin` opens a browser window
	 Suggestions: []
docs/oidc-kubectl.md:113:6     - Unknown word (apiserver)  -- kube-apiserver validates token
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, iserver]
docs/oidc-kubectl.md:146:7     - Unknown word (portforward) -- `pods/portforward`). The `cluster-reader
	 Suggestions: []
docs/oidc-kubectl.md:151:7     - Unknown word (devantler)   -- `oidc:devantler-tech:platform`).
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/oidc-kubectl.md:157:24    - Unknown word (kubeconfig)  -- client-certificate** kubeconfig stored in the vault
	 Suggestions: []
docs/oidc-kubectl.md:159:22    - Unknown word (kubeconfig)  -- . Retrieve the root kubeconfig from the vault and point
	 Suggestions: []
docs/oidc-kubectl.md:159:59    - Unknown word (KUBECONFIG)  -- the vault and point `KUBECONFIG` at it
	 Suggestions: []
docs/oidc-kubectl.md:163:11    - Unknown word (KUBECONFIG)  -- export KUBECONFIG=/path/to/root-kubeconfig
	 Suggestions: []
docs/oidc-kubectl.md:170:10    - Unknown word (KUBECONFIG)  -- unset KUBECONFIG
	 Suggestions: []
docs/oidc-kubectl.md:180:2     - Unknown word (talosctl)    -- `talosctl --talosconfig <admin
	 Suggestions: [talos]
docs/oidc-kubectl.md:180:13    - Unknown word (talosconfig) -- `talosctl --talosconfig <admin-talosconfig>
	 Suggestions: [tsconfig]
docs/oidc-kubectl.md:180:32    - Unknown word (talosconfig) -- -talosconfig <admin-talosconfig> kubeconfig`.
	 Suggestions: [tsconfig]
docs/oidc-kubectl.md:188:6     - Unknown word (apiserver's) -- kube-apiserver's `--oidc-client-id` flag
	 Suggestions: []
docs/policy-reports.md:1:3       - Unknown word (Kyverno)    -- # Kyverno policy-report refresh
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/policy-reports.md:3:1       - Unknown word (Kyverno)    -- Kyverno policy reports are derived
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/policy-reports.md:20:24     - Unknown word (Kyverno)    -- The policy's committed Kyverno test pins the supported
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/policy-reports.md:23:1      - Unknown word (kyverno)    -- kyverno test tests/validate
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
docs/policy-reports.md:34:12     - Unknown word (Kyverno)    -- 2. Run the Kyverno test above and the normal
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/policy-reports.md:41:37     - Unknown word (policyreports) -- context=admin@prod get policyreports.wgpolicyk8s.io -A -o
	 Suggestions: []
docs/policy-reports.md:41:51     - Unknown word (wgpolicyk)     -- prod get policyreports.wgpolicyk8s.io -A -o json \
	 Suggestions: []
docs/policy-reports.md:52:1      - Unknown word (Kyverno)       -- Kyverno 1.18.1 exposes no supported
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/progressive-delivery.md:13:49     - Unknown word (Coroot)     -- Cilium Gateway API + Coroot stack.
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/progressive-delivery.md:22:42     - Unknown word (Coroot's)   -- MetricTemplate`s query **Coroot's bundled Prometheus*
	 Suggestions: [corot's, Corot's, Coot's, croat's, Croat's]
docs/progressive-delivery.md:23:28     - Unknown word (coroot)     -- endpoint OpenCost uses, `coroot-prometheus.observability
	 Suggestions: [corot, Corot, chroot, coot, coopt]
docs/progressive-delivery.md:23:72     - Unknown word (Coroot's)   -- observability.svc:9090`). Coroot's eBPF
	 Suggestions: [corot's, Corot's, Coot's, croat's, Croat's]
docs/progressive-delivery.md:29:27     - Unknown word (loadtester) -- Webhooks** — `flagger-loadtester` runs an acceptance
	 Suggestions: [loadstar, lodestar]
docs/progressive-delivery.md:30:57     - Unknown word (Coroot)     -- during analysis (so Coroot has requests to
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/progressive-delivery.md:37:26     - Unknown word (gatewayapi) -- Weighted canary** | `gatewayapi:v1` | App is routed
	 Suggestions: [gatewayzip, gatewayZip, Gatewayzip, GatewayZip, gatewayup]
docs/progressive-delivery.md:38:191    - Unknown word (repointed)  -- the apex Service is repointed |
	 Suggestions: [repainted, reprinted, repined, recoined, rejoined]
docs/progressive-delivery.md:44:35     - Unknown word (loadtester) -- controller + `flagger-loadtester`
	 Suggestions: [loadstar, lodestar]
docs/progressive-delivery.md:45:4      - Unknown word (coroot)     -- | `coroot-request-success-rate
	 Suggestions: [corot, Corot, chroot, coot, coopt]
docs/progressive-delivery.md:45:36     - Unknown word (coroot)     -- request-success-rate` / `coroot-request-duration` `MetricTemp
	 Suggestions: [corot, Corot, chroot, coot, coopt]
docs/progressive-delivery.md:46:5      - Unknown word (umami)      -- | **umami** Canary (weighted)
	 Suggestions: [maim, tammi, Tammi, umacr, umask]
docs/progressive-delivery.md:46:108    - Unknown word (umami)      -- | [`apps/umami/canary.yaml`](.k8s
	 Suggestions: [maim, tammi, Tammi, umacr, umask]
docs/progressive-delivery.md:48:5      - Unknown word (opencost)   -- | **opencost** Canary (blue/green
	 Suggestions: [opencast, openest, opens, opec's, open's]
docs/progressive-delivery.md:48:133    - Unknown word (opencost)   -- infrastructure/flagger/canary-opencost.yaml`](.k8s/bases
	 Suggestions: [opencast, openest, opens, opec's, open's]
docs/progressive-delivery.md:52:8      - Unknown word (Kustomization) -- > Flux Kustomization fails the server-side
	 Suggestions: [Customization]
docs/progressive-delivery.md:54:5      - Unknown word (opencost)      -- > **opencost** Canary + the MetricTemplate
	 Suggestions: [opencast, openest, opens, opec's, open's]
docs/progressive-delivery.md:56:20     - Unknown word (coroot)        -- > [`infrastructure/coroot/coroot.yaml`](.k8s
	 Suggestions: [corot, Corot, chroot, coot, coopt]
docs/progressive-delivery.md:56:27     - Unknown word (coroot)        -- infrastructure/coroot/coroot.yaml`](.k8s/bases
	 Suggestions: [corot, Corot, chroot, coot, coopt]
docs/progressive-delivery.md:60:5      - Unknown word (umami)         -- - **umami** — weighted Gateway
	 Suggestions: [maim, tammi, Tammi, umacr, umask]
docs/progressive-delivery.md:64:28     - Unknown word (HSTS)          -- canary). Its old route's HSTS header is re-added via
	 Suggestions: [HATS, HITS, HOTS, HSMS, HUTS]
docs/progressive-delivery.md:65:4      - Unknown word (gethomepage)   -- `gethomepage.dev/*` tile annotations
	 Suggestions: []
docs/progressive-delivery.md:68:26     - Unknown word (Coroot)        -- primary scales 2-3 on Coroot request rate — see
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/progressive-delivery.md:69:5      - Unknown word (opencost)      -- - **opencost** — blue/green infra
	 Suggestions: [opencast, openest, opens, opec's, open's]
docs/progressive-delivery.md:70:4      - Unknown word (opencost)      -- `opencost:http-ui` **named** port
	 Suggestions: [opencast, openest, opens, opec's, open's]
docs/progressive-delivery.md:70:45     - Unknown word (apiserver)     -- named** port via the apiserver proxy; `portDiscovery
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, iserver]
docs/progressive-delivery.md:77:36     - Unknown word (fleetdm)       -- headlamp, actual-budget, fleetdm (parked), hubble-ui
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/progressive-delivery.md:77:285    - Unknown word (fleetdm)       -- concurrent canary pods. fleetdm is disabled since 2
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/progressive-delivery.md:78:3      - Unknown word (openbao)       -- | openbao
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/progressive-delivery.md:95:32     - Unknown word (umami's)       -- the `Canary`** — copy umami's (weighted) or homepage
	 Suggestions: [tammi's, Tammi's, mai's, imam's, magi's]
docs/progressive-delivery.md:96:48     - Unknown word (loadtester)    -- MetricTemplate`s and a loadtester acceptance + load-test
	 Suggestions: [loadstar, lodestar]
docs/progressive-delivery.md:98:15     - Unknown word (netpols)       -- 5. **Open the netpols** — app namespace: ingress
	 Suggestions: [netplot, netPlot, Netplot, NetPlot, nepos]
docs/progressive-delivery.md:104:35    - Unknown word (umami)         -- (used by homepage + umami)
	 Suggestions: [maim, tammi, Tammi, umacr, umask]
docs/progressive-delivery.md:107:14    - Unknown word (Umami)         -- Homepage and Umami use it: each app's
	 Suggestions: [Maim, tammi, Tammi, umacr, Umacr]
docs/progressive-delivery.md:108:4     - Unknown word (Coroot's)      -- on Coroot's inbound request-rate
	 Suggestions: [corot's, Corot's, Coot's, croat's, Croat's]
docs/progressive-delivery.md:118:11    - Unknown word (myapp)         -- name: myapp-so # Flagger
	 Suggestions: [maypop, mcap, myup, myUp, Myup]
docs/progressive-delivery.md:118:52    - Unknown word (myapp)         -- Flagger clones it to myapp-so-primary and pauses
	 Suggestions: [maypop, mcap, myup, myUp, Myup]
docs/progressive-delivery.md:119:64    - Unknown word (rollouts)      -- scaler at 0 between rollouts.
	 Suggestions: [rollout, rollo's, Rollo's, rollo, rolls]
docs/progressive-delivery.md:130:31    - Unknown word (Coroot)        -- deployment), not the primary. Coroot labels metrics by pod
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/progressive-delivery.md:133:29    - Unknown word (bcdfghjklmnpqrstvwxz) -- are **vowel-free** (`bcdfghjklmnpqrstvwxz2456789`): `[bcdfghjklmnpqrstv
	 Suggestions: []
docs/progressive-delivery.md:133:62    - Unknown word (bcdfghjklmnpqrstvwxz) -- fghjklmnpqrstvwxz2456789`): `[bcdfghjklmnpqrstvwxz2-9]+`
	 Suggestions: []
docs/progressive-delivery.md:136:34    - Unknown word (Coroot's)             -- PromQL is written against Coroot's documented schema but
	 Suggestions: [corot's, Corot's, Coot's, croat's, Croat's]
docs/progressive-delivery.md:145:4     - Unknown word (Coroot)               -- - [Coroot node-agent metrics]
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/runtime-security.md:4:17      - Unknown word (behaviour)  -- stops malicious behaviour *while workloads are
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
docs/runtime-security.md:5:19      - Unknown word (Kyverno)    -- it at admission ([Kyverno](.k8s/bases/infrastructure
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/runtime-security.md:9:30      - Unknown word (Kubescape's) -- based runtime sensors — Kubescape's node-agent *and* Tetragon
	 Suggestions: []
docs/runtime-security.md:28:3      - Unknown word (Syscall)     -- | Syscall filter      | **seccomp
	 Suggestions: [Sysctl, Stdcall, stdCall, myscale, Myscale]
docs/runtime-security.md:28:27     - Unknown word (seccomp)     -- Syscall filter      | **seccomp `RuntimeDefault`**
	 Suggestions: [secco, scop, scamp, scoop, second]
docs/runtime-security.md:28:78     - Unknown word (Kyverno)     -- mutated + enforced by [Kyverno](.k8s/bases/infrastructure
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/runtime-security.md:28:199    - Unknown word (syscall)     -- Blocks the dangerous-syscall tail every container
	 Suggestions: [sysctl, stdcall, stdCall, myscale, Myscale]
docs/runtime-security.md:29:27     - Unknown word (sysctls)     -- Kernel hardening    | **sysctls** ([`talos/cluster/harden
	 Suggestions: [sysctl, syst, sects, syncs, sysco]
docs/runtime-security.md:29:68     - Unknown word (sysctls)     -- cluster/harden-kernel-sysctls.yaml`](.talos/cluster
	 Suggestions: [sysctl, syst, sects, syncs, sysco]
docs/runtime-security.md:29:179    - Unknown word (kptr)        -- | `kptr_restrict`, `ptrace_scope
	 Suggestions: [tptr, tPtr, kart, kurt, Kurt]
docs/runtime-security.md:29:259    - Unknown word (privesc)     -- — shrinks the local-privesc surface
	 Suggestions: [pries, prices, prides, priest, primes]
docs/runtime-security.md:31:27     - Unknown word (Kubescape)   -- Runtime detection   | **Kubescape node-agent**
	 Suggestions: [Unescape, Kubespec]
docs/runtime-security.md:31:186    - Unknown word (behaviour)   -- | Learned-behaviour anomaly detection, correlated
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
docs/runtime-security.md:41:5      - Unknown word (Kubescape)   -- ### Kubescape node-agent — *detection
	 Suggestions: [Unescape, Kubespec]
docs/runtime-security.md:43:14     - Unknown word (kubescape)   -- Enabled in [`kubescape/helm-release.yaml`]
	 Suggestions: [unescape, kubespec]
docs/runtime-security.md:49:45     - Unknown word (kubevuln)    -- enable # image CVEs (kubevuln)
	 Suggestions: [kuenlun, Kuenlun, kubectl]
docs/runtime-security.md:59:1      - Unknown word (Kubescape)   -- Kubescape is a **posture platform
	 Suggestions: [Unescape, Kubespec]
docs/runtime-security.md:60:63     - Unknown word (syscall)     -- each workload's normal syscall/file/
	 Suggestions: [sysctl, stdcall, stdCall, myscale, Myscale]
docs/runtime-security.md:61:9      - Unknown word (behaviour)   -- network behaviour and flags deviations
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
docs/runtime-security.md:77:38     - Unknown word (Kubescape's) -- here is the one thing Kubescape's node-agent cannot do
	 Suggestions: []
docs/runtime-security.md:79:1      - Unknown word (Kubescape's) -- Kubescape's runtime *detection*
	 Suggestions: []
docs/runtime-security.md:80:44     - Unknown word (Kubescape's) -- correlation) — that is Kubescape's domain and it does it
	 Suggestions: []
docs/runtime-security.md:86:33     - Unknown word (syscall)     -- termination — the triggering syscall may already have completed
	 Suggestions: [sysctl, stdcall, stdCall, myscale, Myscale]
docs/runtime-security.md:88:14     - Unknown word (syscall)     -- block the syscall itself where the kernel
	 Suggestions: [sysctl, stdcall, stdCall, myscale, Myscale]
docs/runtime-security.md:95:4      - Unknown word (kprobes)     -- kprobes/tracepoints (a syscall
	 Suggestions: [probes, probe, probed, prober, proles]
docs/runtime-security.md:95:12     - Unknown word (tracepoints) -- kprobes/tracepoints (a syscall, an LSM hook
	 Suggestions: []
docs/runtime-security.md:95:27     - Unknown word (syscall)     -- kprobes/tracepoints (a syscall, an LSM hook, a file
	 Suggestions: [sysctl, stdcall, stdCall, myscale, Myscale]
docs/runtime-security.md:101:17    - Unknown word (Kubescape)   -- would duplicate Kubescape). It enforces the rules
	 Suggestions: [Unescape, Kubespec]
docs/runtime-security.md:109:74    - Unknown word (behaviour)   -- correlation, learned-behaviour anomaly detection, the
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
docs/runtime-security.md:109:119   - Unknown word (Kubescape)   -- detection, the single-pane Kubescape view — i.e. *"is this
	 Suggestions: [Unescape, Kubespec]
docs/runtime-security.md:150:12    - Unknown word (Kubescape's) -- because Kubescape's node-agent is detection
	 Suggestions: []
docs/rwx-storage.md:24:4      - Unknown word (siderolabs) -- - `siderolabs/iscsi-tools` — **required
	 Suggestions: [sideroses, siderosis, siderolite]
docs/rwx-storage.md:24:15     - Unknown word (iscsi)      -- - `siderolabs/iscsi-tools` — **required
	 Suggestions: [scsi, SCSI, isis, sics, Isis]
docs/rwx-storage.md:25:4      - Unknown word (siderolabs) -- - `siderolabs/util-linux-tools` —
	 Suggestions: [sideroses, siderosis, siderolite]
docs/rwx-storage.md:26:4      - Unknown word (siderolabs) -- - `siderolabs/qemu-guest-agent` —
	 Suggestions: [sideroses, siderosis, siderolite]
docs/rwx-storage.md:28:49     - Unknown word (ksail)      -- configured declaratively in `ksail.prod.yaml` as
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/rwx-storage.md:39:2      - Unknown word (ksail)      -- `ksail.prod.yaml` and re-run
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/rwx-storage.md:39:31     - Unknown word (ksail)      -- prod.yaml` and re-run `ksail cluster update`; KSail
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/rwx-storage.md:49:9      - Unknown word (talosctl)   -- IMAGE=$(talosctl --nodes <healthy-IP
	 Suggestions: [talos]
docs/rwx-storage.md:49:43     - Unknown word (machineconfig) -- nodes <healthy-IP> get machineconfig -o jsonpath='{.spec
	 Suggestions: []
docs/rwx-storage.md:51:1      - Unknown word (talosctl)      -- talosctl upgrade --nodes <IP
	 Suggestions: [talos]
docs/rwx-storage.md:76:53     - Unknown word (talosctl)      -- sdb`. Confirm with `talosctl disks --nodes <worker
	 Suggestions: [talos]
docs/rwx-storage.md:127:17    - Unknown word (resizer)       -- | `longhorn_csi_resizer_replicas` | `1`
	 Suggestions: [resize, resider, resized, resizes, seizer]
docs/rwx-storage.md:127:55    - Unknown word (resizer)       -- | `1`     | CSI resizer replica count
	 Suggestions: [resize, resider, resized, resizes, seizer]
docs/rwx-storage.md:136:1     - Unknown word (talosctl)      -- talosctl upgrade --nodes <IP
	 Suggestions: [talos]
docs/rwx-storage.md:153:47    - Unknown word (talosctl)      -- on the worker (or via talosctl debug container):
	 Suggestions: [talos]
docs/rwx-storage.md:154:1     - Unknown word (sgdisk)        -- sgdisk -e /dev/sdb
	 Suggestions: [sadism, sadist, sandisk, SanDisk, disk]
docs/rwx-storage.md:155:1     - Unknown word (growpart)      -- growpart /dev/sdb 1
	 Suggestions: [groupware, groat, groper, grower, gosport]
docs/rwx-storage.md:156:5     - Unknown word (growfs)        -- xfs_growfs /var/lib/longhorn #
	 Suggestions: [grows, growls, gros, grow, glows]
docs/secret-rotation.md:7:30      - Unknown word (fleetdm)    -- (2026-06-03):** the fleetdm app — the flagship for
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/secret-rotation.md:8:43      - Unknown word (kustomization) -- disabled** (`k8s/bases/apps/kustomization.yaml`); its OpenBao
	 Suggestions: [customization]
docs/secret-rotation.md:9:60      - Unknown word (fleetdm)       -- for re-enabling. The fleetdm phases below
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/secret-rotation.md:19:5      - Unknown word (fleetdm)       -- fleetdm DB/redis/license, headlamp
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/secret-rotation.md:31:30     - Unknown word (statemanager)  -- a `GeneratorState` (statemanager) and reuses the prior
	 Suggestions: []
docs/secret-rotation.md:40:14     - Unknown word (creds)         -- | **Database creds** | fleetdm MySQL
	 Suggestions: [cred, coeds, credo, crees, cress]
docs/secret-rotation.md:40:25     - Unknown word (fleetdm)       -- Database creds**  | fleetdm MySQL `fleet` user,
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/secret-rotation.md:49:20     - Unknown word (Valkey)        -- and Redis (via the Valkey-compatible plugin):
	 Suggestions: [Valley, Flakey, oakley, Oakley, Vale]
docs/secret-rotation.md:51:67     - Unknown word (fleetdm)       -- Secret` at startup (like fleetdm) —
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/secret-rotation.md:71:45     - Unknown word (creds)         -- on `database/static-creds/fleet` and returns the
	 Suggestions: [cred, coeds, credo, crees, cress]
docs/secret-rotation.md:84:42     - Unknown word (bitnami)       -- writes `mysql` Secret → bitnami MySQL bootstraps the
	 Suggestions: [bigname, binname, bigName, Bigname, binName]
docs/secret-rotation.md:119:16    - Unknown word (ksail)         -- both build; `ksail workload validate` and
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/secret-rotation.md:119:46    - Unknown word (ksail)         -- workload validate` and `ksail --config ksail.prod
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/secret-rotation.md:119:61    - Unknown word (ksail)         -- and `ksail --config ksail.prod.yaml workload validate
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/secret-rotation.md:121:8     - Unknown word (creds)         -- with creds sourced from `database
	 Suggestions: [cred, coeds, credo, crees, cress]
docs/secret-rotation.md:121:44    - Unknown word (creds)         -- from `database/static-creds/fleet`. (CI validates
	 Suggestions: [cred, coeds, credo, crees, cress]
docs/secret-rotation.md:126:30    - Unknown word (Valkey)        -- pattern using OpenBao's Valkey-compatible plugin (Redis
	 Suggestions: [Valley, Flakey, oakley, Oakley, Vale]
docs/TEMPLATING.md:4:25      - Unknown word (Kustomizations) -- providers, cluster Flux Kustomizations) can stay untouched
	 Suggestions: [Customization]
docs/TEMPLATING.md:5:14      - Unknown word (homelab)        -- for your own homelab. Everything a new instance
	 Suggestions: [homely, homeland, hola, home, homag]
docs/TEMPLATING.md:5:58      - Unknown word (customise)      -- new instance needs to customise is listed
	 Suggestions: [customize, custodies, customizes, customs, custom's]
docs/TEMPLATING.md:7:8       - Unknown word (upstreaming)    -- you're upstreaming a change.
	 Suggestions: [streaming, upstream, unseaming, uprearing, upstaging]
docs/TEMPLATING.md:16:8      - Unknown word (ksail)          -- ### 1. ksail configs — one per environment
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/TEMPLATING.md:18:9      - Unknown word (ksail)          -- Files: `ksail.yaml` (local), `ksail
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/TEMPLATING.md:18:31     - Unknown word (ksail)          -- ksail.yaml` (local), `ksail.prod.yaml`.
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/TEMPLATING.md:25:61     - Unknown word (kubeconfig)     -- | kubeconfig context
	 Suggestions: []
docs/TEMPLATING.md:25:97     - Unknown word (kubeconfig)     -- context | kubeconfig context
	 Suggestions: []
docs/TEMPLATING.md:32:18     - Unknown word (kustomization)  -- | `spec.workload.kustomizationFile`
	 Suggestions: [customization]
docs/TEMPLATING.md:42:63     - Unknown word (ksail)          -- , and `workers/` as ksail expects.
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/TEMPLATING.md:48:26     - Unknown word (kustomization)  -- `k8s/clusters/<env>/kustomization.yaml` carries two template
	 Suggestions: [customization]
docs/TEMPLATING.md:60:15     - Unknown word (ksail)          -- values, point ksail at it".
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/TEMPLATING.md:67:42     - Unknown word (hostnames)      -- non-secret values (hostnames, URLs,
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/TEMPLATING.md:74:40     - Unknown word (authorised)     -- the Age public keys authorised to decrypt secrets.
	 Suggestions: [authorized, authored, authorize, authorizer, authorizes]
docs/TEMPLATING.md:88:38     - Unknown word (Kustomizations) -- base/` — shared Flux Kustomizations with sentinel paths
	 Suggestions: [Customization]
docs/TEMPLATING.md:89:55     - Unknown word (Kyverno)        -- Cilium, cert-manager, Kyverno, alerting configs,
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/TEMPLATING.md:106:47    - Unknown word (generatable)    -- generators** create randomly-generatable secrets
	 Suggestions: [generable, generale, generate, generative, generalizable]
docs/TEMPLATING.md:110:43    - Unknown word (openbao)        -- secrets. Runs in the `openbao` namespace with
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TEMPLATING.md:121:12    - Unknown word (generatable)    -- | Randomly-generatable | ESO Password generator
	 Suggestions: [generable, generale, generate, generative, generalizable]
docs/TEMPLATING.md:132:58    - Unknown word (openbao)        -- credentials in the `openbao-unseal` K8s Secret
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TEMPLATING.md:135:5     - Unknown word (openbao)        -- `openbao-unseal` Secret (volume
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/tenant-abstraction.md:14:9      - Unknown word (Kustomization) -- + Flux `Kustomization`, a `ghcr-auth` `ExternalSecr
	 Suggestions: [Customization]
docs/tenant-abstraction.md:25:37     - Unknown word (ascoachingogvaner) -- providers/docker/apps/tenant-ascoachingogvaner.yaml` and `..web-app
	 Suggestions: []
docs/tenant-abstraction.md:33:19     - Unknown word (ascoachingogvaner) -- metadata: { name: ascoachingogvaner }
	 Suggestions: []
docs/tenant-abstraction.md:35:9      - Unknown word (ascoachingogvaner) -- name: ascoachingogvaner
	 Suggestions: []
docs/tenant-abstraction.md:44:6      - Unknown word (Kubescape)         -- trip Kubescape C-0002), the Kyverno
	 Suggestions: [Unescape, Kubespec]
docs/tenant-abstraction.md:44:29     - Unknown word (Kyverno)           -- Kubescape C-0002), the Kyverno-restricted namespaced
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/tenant-abstraction.md:46:50     - Unknown word (validatable)       -- (Enforce) and stay validatable by `ksail workload validate
	 Suggestions: [validate, vacatable]
docs/tenant-abstraction.md:46:66     - Unknown word (ksail)             -- stay validatable by `ksail workload validate`.
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/tenant-abstraction.md:56:17     - Unknown word (diffable)          -- the RGDs render-diffable and the controller in
	 Suggestions: [disable, dimmable, dippable, diablo, dibble]
docs/tenant-abstraction.md:57:46     - Unknown word (Kustomization)     -- the `infrastructure` Kustomization `dependsOn` the controllers
	 Suggestions: [Customization]
docs/tenant-abstraction.md:65:61     - Unknown word (Kustomization)     -- impersonating Flux `Kustomization` — would stay hand-written
	 Suggestions: [Customization]
docs/tenant-abstraction.md:66:96     - Unknown word (Kyverno)           -- overlapping the existing Kyverno
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/tenant-abstraction.md:91:71     - Unknown word (diffable)          -- the RGDs stay render-diffable and the controller sits
	 Suggestions: [disable, dimmable, dippable, diablo, dibble]
docs/tenant-abstraction.md:92:17     - Unknown word (chokepoint)        -- in the platform chokepoint with explicit ordering
	 Suggestions: []
docs/tenant-abstraction.md:93:1      - Unknown word (Kustomization)     -- Kustomization with the controller
	 Suggestions: [Customization]
docs/tenant-abstraction.md:96:90     - Unknown word (prioritised)       -- each to be filed and prioritised as
	 Suggestions: [prioritized, priorities, prioritize, prioritizes]
docs/tenant-abstraction.md:97:16     - Unknown word (behaviour)         -- its own issue, behaviour-preservingly, render
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
docs/tenant-abstraction.md:97:26     - Unknown word (preservingly)      -- own issue, behaviour-preservingly, render-diffed vs the
	 Suggestions: [preserving, pressingly]
docs/tenant-abstraction.md:106:86    - Unknown word (validatable)       -- one *is* safely static-validatable).
	 Suggestions: [validate, vacatable]
docs/tenant-abstraction.md:107:41    - Unknown word (behaviour)         -- false` on both ⇒ no behaviour change for existing
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
docs/tenant-abstraction.md:108:34    - Unknown word (CNPG)              -- `WebApp` shapes** — CNPG-backed apps (e.g. `wedding
	 Suggestions: [cnp, CNP, NCP, PNG, CAPE]
docs/TENANTS.md:6:61      - Unknown word (Kustomization) -- Flux `OCIRepository` + `Kustomization` and
	 Suggestions: [Customization]
docs/TENANTS.md:21:6      - Unknown word (gitops)        -- [`gitops-tenant-template`](https
	 Suggestions: [gitpod, Gitpod, gips, gits, gimps]
docs/TENANTS.md:34:18     - Unknown word (ascoachingogvaner) -- [`k8s/bases/apps/ascoachingogvaner/`](.k8s/bases/apps
	 Suggestions: []
docs/TENANTS.md:42:16     - Unknown word (devantler)         -- gh repo create devantler-tech/<tenant> \
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/TENANTS.md:43:14     - Unknown word (devantler)         -- --template devantler-tech/gitops-tenant-template
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/TENANTS.md:43:29     - Unknown word (gitops)            -- template devantler-tech/gitops-tenant-template --private
	 Suggestions: [gitpod, Gitpod, gips, gits, gimps]
docs/TENANTS.md:47:53     - Unknown word (zizmor)            -- yaml`, `CLAUDE.md`, `zizmor.yml`) plus scaffolding
	 Suggestions: [zimmer, Zimmer, gizmo, izmir, timor]
docs/TENANTS.md:48:10     - Unknown word (customise)         -- you then customise and own (`AGENTS.md
	 Suggestions: [customize, custodies, customizes, customs, custom's]
docs/TENANTS.md:49:28     - Unknown word (releaserc)         -- Dockerfile`, `deploy/`, `.releaserc`, `.gitignore`, `.github
	 Suggestions: [releaser, releasers, release, released, releases]
docs/TENANTS.md:56:47     - Unknown word (kustomization)     -- Kubernetes manifests (`kustomization.yaml`,
	 Suggestions: [customization]
docs/TENANTS.md:57:39     - Unknown word (httproute)         -- yaml`, `service.yaml`, `httproute.yaml`, an optional
	 Suggestions: []
docs/TENANTS.md:67:7      - Unknown word (templatesyncignore) -- - **`.templatesyncignore`** — list every file
	 Suggestions: []
docs/TENANTS.md:70:5      - Unknown word (releaserc)          -- `.releaserc`, `.gitignore`, `.github
	 Suggestions: [releaser, releasers, release, released, releases]
docs/TENANTS.md:71:13     - Unknown word (templatesyncignore) -- and the `.templatesyncignore` itself). Everything
	 Suggestions: []
docs/TENANTS.md:79:21     - Unknown word (creds)              -- ### App secrets (DB creds, API keys, … — only
	 Suggestions: [cred, coeds, credo, crees, cress]
docs/TENANTS.md:86:38     - Unknown word (ascoachingogvaner's) -- issued credentials — e.g. ascoachingogvaner's simply.com DNS
	 Suggestions: []
docs/TENANTS.md:87:22     - Unknown word (ascoachingogvaner)   -- credentials at `apps/ascoachingogvaner/simply`), or seed it
	 Suggestions: []
docs/TENANTS.md:96:11     - Unknown word (openbao)             -- { name: openbao, kind: SecretStore
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TENANTS.md:97:40     - Unknown word (Kyverno)             -- tenant-secret-stores` Kyverno policy blocks that)
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/TENANTS.md:109:36    - Unknown word (secretstore)         -- its own namespaced `secretstore.yaml` (`kind: SecretStore
	 Suggestions: [certstore, certStore, secretor, secretors, secretory]
docs/TENANTS.md:110:4     - Unknown word (openbao)             -- `openbao`) in its `deploy/`
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TENANTS.md:119:78    - Unknown word (openbao)             -- the cluster-scoped `openbao`
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TENANTS.md:120:28    - Unknown word (materialises)        -- ClusterSecretStore** and materialises the `ghcr-auth` dockerconfigj
	 Suggestions: [materialists, materializes, materialness, materialism, materialist]
docs/TENANTS.md:120:57    - Unknown word (dockerconfigjson)    -- materialises the `ghcr-auth` dockerconfigjson the
	 Suggestions: []
docs/TENANTS.md:123:24    - Unknown word (Kyverno)             -- resources may not (the Kyverno policy carves out flux
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/TENANTS.md:124:41    - Unknown word (dockerconfigjson)    -- SOPS-encrypted as `ghcr_dockerconfigjson` in the shared
	 Suggestions: []
docs/TENANTS.md:126:73    - Unknown word (openbao)             -- ghcr/auth` via the `openbao`
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TENANTS.md:153:17    - Unknown word (Kustomization)       -- > impersonating Kustomization, optional external-dns
	 Suggestions: [Customization]
docs/TENANTS.md:155:38    - Unknown word (ascoachingogvaner)   -- providers/docker/apps/tenant-ascoachingogvaner.yaml`); prod tenants
	 Suggestions: []
docs/TENANTS.md:160:5     - Unknown word (ascoachingogvaner)   -- or `ascoachingogvaner/` (a static tenant that
	 Suggestions: []
docs/TENANTS.md:166:4     - Unknown word (kustomization)       -- | `kustomization.yaml`
	 Suggestions: [customization]
docs/TENANTS.md:168:78    - Unknown word (automount)           -- | SA with `automountServiceAccountToken:
	 Suggestions: [autocount, autoCount, Autocount, AutoCount, autoout]
docs/TENANTS.md:170:110   - Unknown word (openbao)             -- ExternalSecret` (shared `openbao` ClusterSecretStore
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/TENANTS.md:171:318   - Unknown word (ascoachingogvaner)   -- namespaces) — mirror `ascoachingogvaner/` |
	 Suggestions: []
docs/TENANTS.md:172:33    - Unknown word (kustomization)       -- repository.yaml` + `flux-kustomization.yaml`
	 Suggestions: [customization]
docs/TENANTS.md:172:129   - Unknown word (Kustomization)       -- cosign `verify`) + Flux `Kustomization` (prune, `serviceAccountName
	 Suggestions: [Customization]
docs/TENANTS.md:177:38    - Unknown word (secretstore)         -- secrets — the namespaced `secretstore.yaml`. The tenant's
	 Suggestions: [certstore, certStore, secretor, secretors, secretory]
docs/TENANTS.md:183:34    - Unknown word (kustomization)       -- repository.yaml` / `flux-kustomization.yaml`, update the `name
	 Suggestions: [customization]
docs/TENANTS.md:184:17    - Unknown word (devantler)           -- (`oci://ghcr.io/devantler-tech/<tenant>/manifests
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/TENANTS.md:190:18    - Unknown word (kustomization)       -- [`k8s/bases/apps/kustomization.yaml`](.k8s/bases
	 Suggestions: [customization]
docs/TENANTS.md:202:10    - Unknown word (Kustomization)       -- layer a `Kustomization` `spec.patches` onto
	 Suggestions: [Customization]
docs/TENANTS.md:202:78    - Unknown word (Kustomization)       -- platform-side Flux `Kustomization`
	 Suggestions: [Customization]
docs/TENANTS.md:212:45    - Unknown word (CNPG)                -- repo: `wedding-app`'s CNPG `Cluster` `storage.storageCla
	 Suggestions: [cnp, CNP, NCP, PNG, CAPE]
docs/TENANTS.md:220:12    - Unknown word (hostnames)           -- here** — **hostnames**, **`gethomepage.dev
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/TENANTS.md:220:28    - Unknown word (gethomepage)         -- — **hostnames**, **`gethomepage.dev/*` dashboard annotations
	 Suggestions: []
docs/TENANTS.md:222:26    - Unknown word (hostnames)           -- List all of a tenant's hostnames (local + prod + any
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/TENANTS.md:223:11    - Unknown word (httproute)           -- `deploy/httproute.yaml`. The Gateway attaches
	 Suggestions: []
docs/TENANTS.md:223:58    - Unknown word (hostnames)           -- Gateway attaches only the hostnames that match a listener
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/TENANTS.md:225:44    - Unknown word (gethomepage)         -- homepage` app discovers `gethomepage.dev/*` annotations on
	 Suggestions: []
docs/TENANTS.md:229:34    - Unknown word (httproute)           -- existing tenant's `deploy/httproute.yaml` (e.g. `ascoachingogvane
	 Suggestions: []
docs/TENANTS.md:230:1     - Unknown word (hostnames)           -- hostnames *and* its dashboard
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/TENANTS.md:233:4     - Unknown word (ksail)               -- > `ksail workload validate`.
	 Suggestions: [kail, sail, csail, CSAIL, kali]
docs/TENANTS.md:237:17    - Unknown word (fleetdm)             -- > the disabled `fleetdm` patch was deleted,
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
docs/unifi-management.md:5:3       - Unknown word (devantler)  -- [`devantler-tech/unifi`](https:
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/unifi-management.md:5:18      - Unknown word (unifi)      -- [`devantler-tech/unifi`](https://github.com
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/unifi-management.md:5:71      - Unknown word (Crossplane) -- devantler-tech/unifi), as Crossplane
	 Suggestions: [Cropland, Crosspiece]
docs/unifi-management.md:7:12      - Unknown word (upjet)      -- [`provider-upjet-unifi`](https://github
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
docs/unifi-management.md:7:18      - Unknown word (unifi)      -- [`provider-upjet-unifi`](https://github.com
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/unifi-management.md:8:36      - Unknown word (Kustomization) -- the cluster, a Flux `Kustomization` pulls the repo and
	 Suggestions: [Customization]
docs/unifi-management.md:9:21      - Unknown word (unifi)         -- resources into the `unifi` namespace, and the
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/unifi-management.md:9:47      - Unknown word (Crossplane)    -- namespace, and the Crossplane provider continuously
	 Suggestions: [Cropland, Crosspiece]
docs/unifi-management.md:15:1      - Unknown word (devantler)     -- devantler-tech/unifi (Crossplane
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/unifi-management.md:15:16     - Unknown word (unifi)         -- devantler-tech/unifi (Crossplane Managed
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/unifi-management.md:15:23     - Unknown word (Crossplane)    -- devantler-tech/unifi (Crossplane Managed Resources)
	 Suggestions: [Cropland, Crosspiece]
docs/unifi-management.md:16:28     - Unknown word (Kustomization) -- Flux GitRepository + Kustomization (namespace: unifi, runs
	 Suggestions: [Customization]
docs/unifi-management.md:16:54     - Unknown word (unifi)         -- Kustomization (namespace: unifi, runs as the unifi SA
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/unifi-management.md:18:10     - Unknown word (upjet)         -- provider-upjet-unifi (Crossplane, crossplane
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
docs/unifi-management.md:18:23     - Unknown word (Crossplane)    -- provider-upjet-unifi (Crossplane, crossplane-system)
	 Suggestions: [Cropland, Crosspiece]
docs/unifi-management.md:18:35     - Unknown word (crossplane)    -- upjet-unifi (Crossplane, crossplane-system)
	 Suggestions: [cropland, crosspiece]
docs/unifi-management.md:24:26     - Unknown word (Crossplane)    -- is the steady-state Crossplane model; it replaced an
	 Suggestions: [Cropland, Crosspiece]
docs/unifi-management.md:25:51     - Unknown word (WLANs)         -- Broadening coverage (VLANs/WLANs/firewall) and a
	 Suggestions: [weans, alans, clans, elans, flans]
docs/unifi-management.md:27:11     - Unknown word (upjet)         -- `provider-upjet-unifi` issues.
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
docs/unifi-management.md:33:197    - Unknown word (crossplane)    -- hetzner/infrastructure/crossplane/` |
	 Suggestions: [cropland, crosspiece]
docs/unifi-management.md:34:24     - Unknown word (netpol)        -- Tenant (ns, SA/RBAC, netpol, namespaced `SecretStore
	 Suggestions: [netplot, netPlot, Netplot, NetPlot, nepal]
docs/unifi-management.md:34:141    - Unknown word (Kustomization) -- GitRepository`, Flux `Kustomization`) | `k8s/providers/hetzner
	 Suggestions: [Customization]
docs/unifi-management.md:36:187    - Unknown word (kustomization) -- providers/hetzner/apps/kustomization.yaml` |
	 Suggestions: [customization]
docs/unifi-management.md:39:6      - Unknown word (Kustomization) -- Flux Kustomization like wedding-app/github
	 Suggestions: [Customization]
docs/unifi-management.md:40:38     - Unknown word (crossplane)    -- nfrastructure** layer (a `pkg.crossplane.io` `Provider` needs
	 Suggestions: [cropland, crosspiece]
docs/unifi-management.md:41:64     - Unknown word (upjet)         -- ordering as `provider-upjet-github`). It
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
docs/unifi-management.md:47:13     - Unknown word (crossplane)    -- (`*.unifi.m.crossplane.io`) `Client`/`TrafficRoute
	 Suggestions: [cropland, crosspiece]
docs/unifi-management.md:49:20     - Unknown word (Kustomization) -- The tenant's Flux `Kustomization` runs **as the namespace
	 Suggestions: [Customization]
docs/unifi-management.md:58:17     - Unknown word (openbao)       -- **namespaced** `openbao` `SecretStore` (not
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/unifi-management.md:59:33     - Unknown word (Kyverno)       -- tenant-secret-stores` Kyverno policy blocks tenants
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
docs/unifi-management.md:59:73     - Unknown word (authorised)    -- blocks tenants from it), authorised
	 Suggestions: [authorized, authored, authorize, authorizer, authorizes]
docs/unifi-management.md:66:43     - Unknown word (openbao)       -- via the namespaced `openbao` SecretStore (unifi
	 Suggestions: [openai, openbsd, openbase, Openbase, OpenAI]
docs/unifi-management.md:72:10     - Unknown word (upjet)         -- provider-upjet-unifi (crossplane-system
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
docs/unifi-management.md:72:23     - Unknown word (crossplane)    -- provider-upjet-unifi (crossplane-system) ──▶  controller
	 Suggestions: [cropland, crosspiece]
docs/unifi-management.md:91:26     - Unknown word (Velero)        -- Raft snapshots → R2 via Velero), so the manually-set
	 Suggestions: [valero, Valero, velcro, Velcro, Veer]
docs/unifi-management.md:120:47    - Unknown word (providerconfigs) -- kubectl get providers,providerconfigs.unifi.m.crossplane.io
	 Suggestions: []
docs/unifi-management.md:123:13    - Unknown word (devantler)       -- (`ghcr.io/devantler-tech/provider-upjet
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
docs/wireguard-vpn-access.md:5:27      - Unknown word (unifi)      -- UniFi *client* side is [unifi#9](https://github.com
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/wireguard-vpn-access.md:12:19     - Unknown word (Coroot)     -- The admin UIs — **Coroot** (`observability`)
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/wireguard-vpn-access.md:33:36     - Unknown word (hostnames)  -- | admin hostnames → **Cloudflare** (`
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/wireguard-vpn-access.md:34:82     - Unknown word (vxlan)      -- routing-mode=tunnel/vxlan`, `enable-ipv4-masquerade
	 Suggestions: [vlan, VLAN, vilna, Vilna, vala]
docs/wireguard-vpn-access.md:36:53     - Unknown word (Coroot)     -- oauth2-proxy+Dex gates Coroot/Hubble/OpenCost/Longhorn
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
docs/wireguard-vpn-access.md:48:66     - Unknown word (SNAT'd)     -- sourced traffic is **SNAT'd to a node
	 Suggestions: [sat's, sated, snapd, snath, scat's]
docs/wireguard-vpn-access.md:56:69     - Unknown word (datapath)   -- externalIPs/nodePort datapath. So
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
docs/wireguard-vpn-access.md:58:11     - Unknown word (DNAT'd)     -- **not** DNAT'd by Cilium unless `wg
	 Suggestions: [dat's, DAT's, dated, dna's, DNA's]
docs/wireguard-vpn-access.md:58:72     - Unknown word (datapath)   -- to `devices` — a core-datapath
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
docs/wireguard-vpn-access.md:68:46     - Unknown word (unifi)      -- horizon, already in unifi#9) → **`10.0.1.7`**
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/wireguard-vpn-access.md:71:43     - Unknown word (nftables)   -- .ip_forward=1` + an nftables masquerade rule for
	 Suggestions: [notables, notable, notable's, fables, tables]
docs/wireguard-vpn-access.md:75:69     - Unknown word (hostnames)  -- shared LB serving those hostnames to
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/wireguard-vpn-access.md:91:3      - Unknown word (hostnames)  -- hostnames via the LB) and removing
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/wireguard-vpn-access.md:92:34     - Unknown word (datapath)   -- explicit, predictable datapath; **no core Cilium change
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
docs/wireguard-vpn-access.md:98:56     - Unknown word (datapath)   -- tunnel enters the BPF datapath), add a second
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
docs/wireguard-vpn-access.md:107:6     - Unknown word (Datapath)   -- 1. **Datapath reachability.** With
	 Suggestions: [dataauth, dataAuth, Dataauth, DataAuth, Datadata]
docs/wireguard-vpn-access.md:111:40    - Unknown word (hostnames)  -- gateway listener** and the hostnames from **Cloudflare**
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
docs/wireguard-vpn-access.md:120:63    - Unknown word (unroutable) -- .200.0.1`, which is unroutable off the
	 Suggestions: [unquotable, unrentable, unsortable, uncountable, unmountable]
docs/wireguard-vpn-access.md:126:62    - Unknown word (keypair)    -- generate the server keypair; apply the
	 Suggestions: [keypad, kvpair, keypads, kvPair, kepi]
docs/wireguard-vpn-access.md:128:21    - Unknown word (unifi)      -- (`infrastructure/unifi/wireguard`); `talosctl
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
docs/wireguard-vpn-access.md:128:41    - Unknown word (talosctl)   -- unifi/wireguard`); `talosctl --nodes 10.0.1.1,10
	 Suggestions: [talos]
docs/wireguard-vpn-access.md:130:19    - Unknown word (datapath)   -- 3. **Validate the datapath** (crux 1) from a tunnel
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
docs/wireguard-vpn-access.md:140:34    - Unknown word (hostnames)  -- DNS:** drop the admin hostnames from Cloudflare entirely
	 Suggestions: [hostname, hostages, houtname, houtName, hOutname]
hosts:1:11      - Unknown word (ascoachingogvaner) -- 127.0.0.1 ascoachingogvaner.platform.lan
	 Suggestions: []
hosts:4:11      - Unknown word (fleetdm)           -- 127.0.0.1 fleetdm.platform.lan
	 Suggestions: [fleet, fleets, fleet's, fleeted, fleeter]
ksail.prod.yaml:4:13      - Unknown word (ksail)      -- apiVersion: ksail.io/v1alpha1
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.prod.yaml:33:55     - Unknown word (coroot)     -- DaemonSets (cilium, coroot, kubescape,
	 Suggestions: [corot, Corot, chroot, coot, coopt]
ksail.prod.yaml:33:63     - Unknown word (kubescape)  -- DaemonSets (cilium, coroot, kubescape,
	 Suggestions: [unescape, kubespec]
ksail.prod.yaml:50:36     - Unknown word (KSAIL)      -- list, so the pinned KSAIL_VERSION (ci.yaml/cd
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
ksail.prod.yaml:74:32     - Unknown word (ksail)      -- KSail now ENFORCES this: ksail#6172 rejects a Talos
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.prod.yaml:81:54     - Unknown word (ksail)      -- validation is being changed (ksail#5017) to expect
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.prod.yaml:86:49     - Unknown word (ksail)      -- the user_data limit, ksail#5015.)
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.prod.yaml:95:11     - Unknown word (overprovisioning) -- # overprovisioning/) reserves warm scale
	 Suggestions: []
ksail.prod.yaml:99:17     - Unknown word (ksail)            -- # ships ksail#5277 (post-v7.58.3)
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.prod.yaml:99:54     - Unknown word (KSAIL)            -- v7.58.3); the CI/CD KSAIL_VERSION pin
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
ksail.prod.yaml:114:32    - Unknown word (ksail's)          -- Hetzner clusters between ksail's metrics-server install
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
ksail.prod.yaml:117:34    - Unknown word (devantler)        -- pending). File upstream in devantler-tech/ksail if this trips
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
ksail.prod.yaml:121:19    - Unknown word (Kyverno)          -- policyEngine: Kyverno
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
ksail.prod.yaml:125:10    - Unknown word (ksail's)          -- # to ksail's newer default when that
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
ksail.prod.yaml:132:49    - Unknown word (datapath)         -- including the kube-system datapath now under auto-vpa,
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
ksail.prod.yaml:139:41    - Unknown word (ksail's)          -- in-place upgrade to ksail's default version.
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
ksail.prod.yaml:149:13    - Unknown word (siderolabs)       -- # - siderolabs/iscsi-tools (Longhorn
	 Suggestions: [sideroses, siderosis, siderolite]
ksail.prod.yaml:150:13    - Unknown word (siderolabs)       -- # - siderolabs/util-linux-tools (Longhorn
	 Suggestions: [sideroses, siderosis, siderolite]
ksail.prod.yaml:151:13    - Unknown word (siderolabs)       -- # - siderolabs/qemu-guest-agent (Hetzner
	 Suggestions: [sideroses, siderosis, siderolite]
ksail.prod.yaml:153:11    - Unknown word (siderolabs)       -- - siderolabs/iscsi-tools
	 Suggestions: [sideroses, siderosis, siderolite]
ksail.prod.yaml:154:11    - Unknown word (siderolabs)       -- - siderolabs/util-linux-tools
	 Suggestions: [sideroses, siderosis, siderolite]
ksail.prod.yaml:157:18    - Unknown word (devantler)        -- registry: "devantler:${GHCR_TOKEN}@ghcr.io
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
ksail.prod.yaml:157:50    - Unknown word (devantler)        -- GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
ksail.prod.yaml:168:75    - Unknown word (Fulcio)           -- deploy) under GitHub's Fulcio
	 Suggestions: [Lucio, Folio, Fileio, Fulcra, Fulfil]
ksail.prod.yaml:172:20    - Unknown word (defence)          -- # not trusted (defence in depth on a high-blast
	 Suggestions: [defense, deface, defect, defend, define]
ksail.prod.yaml:196:64    - Unknown word (coroot)           -- tax (Cilium, Longhorn, coroot/kubescape
	 Suggestions: [corot, Corot, chroot, coot, coopt]
ksail.prod.yaml:196:71    - Unknown word (kubescape)        -- Cilium, Longhorn, coroot/kubescape
	 Suggestions: [unescape, kubespec]
ksail.prod.yaml:227:16    - Unknown word (Coroot)           -- # bases (Coroot + Flagger), so `ksail
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
ksail.prod.yaml:228:69    - Unknown word (kubeconform)      -- CRD-catalog schemas kubeconform
	 Suggestions: [kubeconfig]
ksail.prod.yaml:230:13    - Unknown word (Coroot)           -- # - Coroot (coroot.com/v1): the
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
ksail.prod.yaml:230:21    - Unknown word (coroot)           -- # - Coroot (coroot.com/v1): the datreeio
	 Suggestions: [corot, Corot, chroot, coot, coopt]
ksail.prod.yaml:230:41    - Unknown word (datreeio)         -- coroot.com/v1): the datreeio catalog schema is stale
	 Suggestions: []
ksail.prod.yaml:232:23    - Unknown word (clickhouse)       -- # nodeAgent/clickhouse), so kubeconform rejects
	 Suggestions: []
ksail.prod.yaml:232:39    - Unknown word (kubeconform)      -- nodeAgent/clickhouse), so kubeconform rejects valid manifests
	 Suggestions: [kubeconfig]
ksail.prod.yaml:235:73    - Unknown word (umami)            -- onboarded Canaries (umami,
	 Suggestions: [maim, tammi, Tammi, umask, uname]
ksail.prod.yaml:236:23    - Unknown word (opencost)         -- # homepage, opencost) can't be schema-checked
	 Suggestions: [opencast, openest, opens, opec's, open's]
ksail.prod.yaml:245:9     - Unknown word (Coroot)           -- # Coroot/Flagger CRD catalog
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
ksail.prod.yaml:247:11    - Unknown word (Coroot)           -- - Coroot
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
ksail.yaml:4:13      - Unknown word (ksail)      -- apiVersion: ksail.io/v1alpha1
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.yaml:21:19     - Unknown word (Kyverno)    -- policyEngine: Kyverno
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
ksail.yaml:27:34     - Unknown word (CNPG)       -- e.g. multi-instance CNPG, Velero node-spread
	 Suggestions: [cnp, CNP, PNG, CAPE, CAPH]
ksail.yaml:27:40     - Unknown word (Velero)     -- multi-instance CNPG, Velero node-spread).
	 Suggestions: [valero, Valero, velcro, Velcro, Veer]
ksail.yaml:39:44     - Unknown word (kubeconform) -- CRD-catalog schemas kubeconform cannot resolve during
	 Suggestions: [kubeconfig]
ksail.yaml:40:10     - Unknown word (ksail)       -- # `ksail workload validate`
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.yaml:40:45     - Unknown word (ksail)       -- validate` (requires ksail >= 7.26.0):
	 Suggestions: [kail, sail, csail, CSAIL, kali]
ksail.yaml:41:13     - Unknown word (Coroot)      -- # - Coroot (coroot.com/v1): the
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
ksail.yaml:41:21     - Unknown word (coroot)      -- # - Coroot (coroot.com/v1): the datreeio
	 Suggestions: [corot, Corot, chroot, coot, coopt]
ksail.yaml:41:41     - Unknown word (datreeio)    -- coroot.com/v1): the datreeio catalog schema is stale
	 Suggestions: []
ksail.yaml:43:23     - Unknown word (clickhouse)  -- # nodeAgent/clickhouse), so kubeconform rejects
	 Suggestions: []
ksail.yaml:43:39     - Unknown word (kubeconform) -- nodeAgent/clickhouse), so kubeconform rejects valid manifests
	 Suggestions: [kubeconfig]
ksail.yaml:46:73     - Unknown word (umami)       -- onboarded Canaries (umami,
	 Suggestions: [maim, tammi, Tammi, umask, uname]
ksail.yaml:47:23     - Unknown word (opencost)    -- # homepage, opencost) can't be schema-checked
	 Suggestions: [opencast, openest, opens, opec's, open's]
ksail.yaml:53:11     - Unknown word (Coroot)      -- - Coroot
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
README.md:1:3       - Unknown word (Devantler)  -- # Devantler Tech Platform ☸️⛴️
	 Suggestions: [Decanter, deventer, Deventer, Desalter, Defaulter]
README.md:26:196    - Unknown word (kubeconfig) -- boot, CCM, CSI, and kubeconfig.
	 Suggestions: []
README.md:55:1      - Unknown word (ksail)      -- ksail cluster create
	 Suggestions: [kail, sail, csail, CSAIL, kali]
README.md:56:1      - Unknown word (ksail)      -- ksail workload push
	 Suggestions: [kail, sail, csail, CSAIL, kali]
README.md:57:1      - Unknown word (ksail)      -- ksail workload reconcile
	 Suggestions: [kail, sail, csail, CSAIL, kali]
README.md:60:80     - Unknown word (ksail)      -- extraPortMappings` in [`ksail.yaml`](ksail.yaml))
	 Suggestions: [kail, sail, csail, CSAIL, kali]
README.md:72:25     - Unknown word (ksail)      -- template — then re-run `ksail workload push && ksail
	 Suggestions: [kail, sail, csail, CSAIL, kali]
README.md:74:52     - Unknown word (kustomization) -- infrastructure/controllers/kustomization.yaml`
	 Suggestions: [customization]
README.md:75:40     - Unknown word (kustomization) -- docker/infrastructure/kustomization.yaml`
	 Suggestions: [customization]
README.md:76:30     - Unknown word (kustomization) -- providers/docker/apps/kustomization.yaml`
	 Suggestions: [customization]
README.md:125:35    - Unknown word (Kyverno)       -- runtime security** — Kyverno, Kubescape, Tetragon
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
README.md:125:44    - Unknown word (Kubescape)     -- security** — Kyverno, Kubescape, Tetragon; see [`docs
	 Suggestions: [Unescape, Kubespec]
README.md:127:53    - Unknown word (Descheduler)   -- Autoscaler (nodes), SIG Descheduler (pod rebalancing + node
	 Suggestions: [Scheduler]
README.md:127:170   - Unknown word (umami)         -- autoscaling (homepage/umami); see [`docs/node-autoscaling
	 Suggestions: [maim, tammi, Tammi, umacr, umask]
README.md:128:118   - Unknown word (Coroot)        -- rollback, metrics from Coroot); see [`docs/progressive
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
README.md:129:23    - Unknown word (Coroot)        -- **Observability** — Coroot (self-hosted, eBPF:
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
README.md:130:21    - Unknown word (Velero)        -- - **Backup / DR** — Velero with CloudNativePG backups
	 Suggestions: [valero, Valero, velcro, Velcro, Veer]
README.md:131:28    - Unknown word (Virt)          -- Virtualization** — KubeVirt + CDI *(local/CI only
	 Suggestions: [vert, Vert, Vidt, Virtu, VIDT]
README.md:132:17    - Unknown word (Testkube)      -- - **Testing** — Testkube *(local/CI only; not
	 Suggestions: [Testee, Testate, Testbed, Testudo, Texture]
README.md:136:51    - Unknown word (Umami)         -- Headlamp (Kubernetes UI), Umami (analytics), Actual
	 Suggestions: [Maim, tammi, Tammi, umacr, Umacr]
README.md:137:129   - Unknown word (kustomization) -- see [`k8s/bases/apps/kustomization.yaml`](k8s/bases/apps
	 Suggestions: [customization]
README.md:138:61    - Unknown word (ascoachingogvaner) -- their own repositories (`ascoachingogvaner`, `wedding-app`); see
	 Suggestions: []
README.md:149:59    - Unknown word (Kustomizations)    -- Contains the shared Flux Kustomizations with sentinel paths
	 Suggestions: [Customization]
README.md:158:298   - Unknown word (Kustomization)     -- the `bootstrap` Flux Kustomization before everything that
	 Suggestions: [Customization]
README.md:226:97    - Unknown word (Velero)            -- OpenBao crypto custody, Velero + CloudNativePG, alerting
	 Suggestions: [valero, Valero, velcro, Velcro, Veer]
scripts/generate-kubescape-exceptions/main_test.go:26:22     - Unknown word (devantler)  -- apiVersion: security.devantler.tech/v1alpha1
	 Suggestions: [decanter, deventer, demangler, Deventer, Demangler]
scripts/generate-kubescape-exceptions/main_test.go:145:17    - Unknown word (Kubescape's) -- // preserved in Kubescape's native posture policy
	 Suggestions: []
scripts/generate-kubescape-exceptions/main_test.go:218:65    - Unknown word (unrecognised) -- closed contract: every unrecognised CR
	 Suggestions: [unrecognized, unreconciled]
scripts/generate-kubescape-exceptions/main_test.go:387:45    - Unknown word (behavioural)  -- eAgainstRealExceptions is the behavioural check: the committed
	 Suggestions: [behavioral, behavior, behaviors, behavior's, behaviorally]
scripts/generate-kubescape-exceptions/main_test.go:389:41    - Unknown word (ksail)        -- ExceptionPolicy objects that `ksail workload scan --exceptions
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/generate-kubescape-exceptions/main.go:1:21      - Unknown word (kubescape)  -- // Command generate-kubescape-exceptions renders a
	 Suggestions: [unescape, kubespec]
scripts/generate-kubescape-exceptions/main.go:1:52      - Unknown word (Kubescape)  -- exceptions renders a Kubescape exceptions file
	 Suggestions: [Unescape, Kubespec]
scripts/generate-kubescape-exceptions/main.go:7:19      - Unknown word (kubescape)  -- // The in-cluster kubescape-operator consumes the
	 Suggestions: [unescape, kubespec]
scripts/generate-kubescape-exceptions/main.go:8:14      - Unknown word (ksail)      -- // CI scan (`ksail workload scan --exceptions
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/generate-kubescape-exceptions/main.go:8:62      - Unknown word (Kubescape's) -- exceptions <file>`) takes Kubescape's native
	 Suggestions: []
scripts/generate-kubescape-exceptions/main.go:13:64     - Unknown word (recognise)   -- this converter does not recognise (an
	 Suggestions: [recognize, recognizes, recognized, recognizee, recognizer]
scripts/generate-kubescape-exceptions/main.go:21:30     - Unknown word (kubescape)   -- run scripts/generate-kubescape-exceptions -o /tmp/exceptions
	 Suggestions: [unescape, kubespec]
scripts/generate-kubescape-exceptions/main.go:52:33     - Unknown word (Kubescape)   -- posturePolicy identifies one Kubescape control excluded by
	 Suggestions: [Unescape, Kubespec]
scripts/generate-kubescape-exceptions/main.go:58:14     - Unknown word (Kubescape's) -- // policy is Kubescape's native PostureExceptionPolicy
	 Suggestions: []
scripts/generate-kubescape-exceptions/main.go:76:7      - Unknown word (velero)      -- // (`^velero-server$`) but plain
	 Suggestions: [valero, velcro, Valero, Velcro, veer]
scripts/generate-kubescape-exceptions/main.go:76:57     - Unknown word (Kubescape)   -- kind/controlID values; Kubescape treats every
	 Suggestions: [Unescape, Kubespec]
scripts/generate-kubescape-exceptions/main.go:300:21    - Unknown word (nolint)      -- return nil, nil //nolint:nilnil // a non-CSE
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/generate-kubescape-exceptions/main.go:300:28    - Unknown word (nilnil)      -- return nil, nil //nolint:nilnil // a non-CSE document
	 Suggestions: [nill, nihil, inline, innit, inlaid]
scripts/generate-kubescape-exceptions/main.go:312:76    - Unknown word (Kubescape)   -- cannot be preserved in Kubescape exceptions")
	 Suggestions: [Unescape, Kubespec]
scripts/generate-kubescape-exceptions/main.go:484:36    - Unknown word (Kubescape's) -- marshals the policies as Kubescape's native exceptions JSON
	 Suggestions: []
scripts/generate-kubescape-exceptions/main.go:494:64    - Unknown word (Kubescape)   -- directory and writes Kubescape JSON.
	 Suggestions: [Unescape, Kubespec]
scripts/generate-kubescape-exceptions/main.go:523:67    - Unknown word (nolint)      -- 44); err != nil { //nolint:gosec // a CI scan input
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/generate-kubescape-exceptions/main.go:523:74    - Unknown word (gosec)       -- err != nil { //nolint:gosec // a CI scan input,
	 Suggestions: [cosec, goes, goer, gogc, gone]
scripts/ghcr-auth-lib.sh:14:23     - Unknown word (Farah)      -- echo "::error::Mike Farah yq v4 is required by
	 Suggestions: [Farad, fatah, Fatah, sarah, Sarah]
scripts/ghcr-auth-lib.sh:24:2      - Unknown word (ksail)      -- ksail workload cipher decrypt
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/ghcr-auth-lib.sh:26:35     - Unknown word (dockerconfigjson) -- "stringData"]["ghcr_dockerconfigjson"]' \
	 Suggestions: []
scripts/ghcr-auth-lib.sh:75:22     - Unknown word (ciphertext)       -- Print a non-secret ciphertext revision for redaction
	 Suggestions: [ciphered]
scripts/ghcr-auth-lib.sh:79:28     - Unknown word (ciphertext)       -- Hash the committed SOPS ciphertext, not the decrypted credential
	 Suggestions: [ciphered]
scripts/ghcr-auth-lib.sh:83:22     - Unknown word (dockerconfigjson) -- .stringData.ghcr_dockerconfigjson
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:18:18     - Unknown word (fanout)     -- allow_incomplete_fanout=false
	 Suggestions: [faut, fagot, fanon, flout, famous]
scripts/refresh-flux-ghcr-auth.sh:20:51     - Unknown word (fanout)     -- only|--allow-incomplete-fanout]" >&2
	 Suggestions: [faut, fagot, fanon, flout, famous]
scripts/refresh-flux-ghcr-auth.sh:26:21     - Unknown word (fanout)     -- --allow-incomplete-fanout) allow_incomplete_fanout
	 Suggestions: [faut, fagot, fanon, flout, famous]
scripts/refresh-flux-ghcr-auth.sh:26:46     - Unknown word (fanout)     -- fanout) allow_incomplete_fanout=true ;;
	 Suggestions: [faut, fagot, fanon, flout, famous]
scripts/refresh-flux-ghcr-auth.sh:28:52     - Unknown word (fanout)     -- only|--allow-incomplete-fanout]" >&2
	 Suggestions: [faut, fagot, fanon, flout, famous]
scripts/refresh-flux-ghcr-auth.sh:38:1      - Unknown word (KSAIL)      -- KSAIL_OPERATOR_VERSION="$
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
scripts/refresh-flux-ghcr-auth.sh:39:39     - Unknown word (ksail)      -- infrastructure/controllers/ksail-operator/helm-release
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/refresh-flux-ghcr-auth.sh:40:10     - Unknown word (KSAIL)      -- readonly KSAIL_OPERATOR_VERSION
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
scripts/refresh-flux-ghcr-auth.sh:41:10     - Unknown word (KSAIL)      -- readonly KSAIL_OPERATOR_IMAGE="ghcr
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
scripts/refresh-flux-ghcr-auth.sh:41:40     - Unknown word (devantler)  -- OPERATOR_IMAGE="ghcr.io/devantler-tech/ksail:v${KSAIL
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
scripts/refresh-flux-ghcr-auth.sh:41:55     - Unknown word (ksail)      -- ghcr.io/devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/refresh-flux-ghcr-auth.sh:41:64     - Unknown word (KSAIL)      -- devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
scripts/refresh-flux-ghcr-auth.sh:46:3      - Unknown word (devantler)  -- "devantler-tech/platform/manifests
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
scripts/refresh-flux-ghcr-auth.sh:47:3      - Unknown word (devantler)  -- "devantler-tech/wedding-app/manifests
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
scripts/refresh-flux-ghcr-auth.sh:48:3      - Unknown word (devantler)  -- "devantler-tech/ascoachingogvaner
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
scripts/refresh-flux-ghcr-auth.sh:48:18     - Unknown word (ascoachingogvaner) -- "devantler-tech/ascoachingogvaner/manifests:latest"
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:49:3      - Unknown word (devantler)         -- "devantler-tech/wedding-app:latest
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
scripts/refresh-flux-ghcr-auth.sh:50:18     - Unknown word (ascoachingogvaner) -- "devantler-tech/ascoachingogvaner:latest"
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:51:18     - Unknown word (ksail)             -- "devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/refresh-flux-ghcr-auth.sh:51:27     - Unknown word (KSAIL)             -- devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
scripts/refresh-flux-ghcr-auth.sh:52:27     - Unknown word (upjet)             -- devantler-tech/provider-upjet-unifi:v0.1.0"
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
scripts/refresh-flux-ghcr-auth.sh:52:33     - Unknown word (unifi)             -- tech/provider-upjet-unifi:v0.1.0"
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
scripts/refresh-flux-ghcr-auth.sh:54:13     - Unknown word (FANOUT)            -- readonly -a FANOUT_NAMESPACES=(
	 Suggestions: [FAUT, FAGOT, FANON, FLOUT, FAMOUS]
scripts/refresh-flux-ghcr-auth.sh:56:3      - Unknown word (ascoachingogvaner) -- "ascoachingogvaner"
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:57:3      - Unknown word (kyverno)           -- "kyverno"
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/refresh-flux-ghcr-auth.sh:158:23    - Unknown word (dockerconfigjson)  -- if ! jq -er '.data[".dockerconfigjson"] | @base64d' \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:163:64    - Unknown word (materialise)       -- namespace}/ghcr-auth did not materialise the Git/SOPS GHCR credential
	 Suggestions: [materialism, materialist, materialize, materializes, materials]
scripts/refresh-flux-ghcr-auth.sh:198:4     - Unknown word (talosctl)          -- # talosctl connects to the public
	 Suggestions: [talos]
scripts/refresh-flux-ghcr-auth.sh:198:63    - Unknown word (talosconfig)       -- control-plane endpoints in talosconfig and
	 Suggestions: [tsconfig]
scripts/refresh-flux-ghcr-auth.sh:251:12    - Unknown word (ciphertext)        -- # current ciphertext revision has been proved
	 Suggestions: [ciphered]
scripts/refresh-flux-ghcr-auth.sh:259:8     - Unknown word (talosctl)          -- if ! talosctl \
	 Suggestions: [talos]
scripts/refresh-flux-ghcr-auth.sh:261:10    - Unknown word (machineconfig)     -- patch machineconfig \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:271:8     - Unknown word (talosctl)          -- if ! talosctl \
	 Suggestions: [talos]
scripts/refresh-flux-ghcr-auth.sh:282:8     - Unknown word (talosctl)          -- if ! talosctl \
	 Suggestions: [talos]
scripts/refresh-flux-ghcr-auth.sh:290:8     - Unknown word (talosctl)          -- if ! talosctl \
	 Suggestions: [talos]
scripts/refresh-flux-ghcr-auth.sh:292:10    - Unknown word (machineconfig)     -- patch machineconfig \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:323:14    - Unknown word (curlrc)            -- # ambient ~/.curlrc cannot enable tracing
	 Suggestions: [curloc, curLoc, curl, curr, ctrlc]
scripts/refresh-flux-ghcr-auth.sh:334:10    - Unknown word (urlencode)         -- --data-urlencode 'service=ghcr.io' \
	 Suggestions: [urlencoded, unencoded]
scripts/refresh-flux-ghcr-auth.sh:335:10    - Unknown word (urlencode)         -- --data-urlencode "scope=repository:$
	 Suggestions: [urlencoded, unencoded]
scripts/refresh-flux-ghcr-auth.sh:410:20    - Unknown word (dockerconfigjson)  -- jq -Rs '{data: {".dockerconfigjson": .}}' \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:418:16    - Unknown word (ksail)             -- patch secret ksail-registry-credentials
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/refresh-flux-ghcr-auth.sh:458:18    - Unknown word (dockerconfigjson)  -- jq '{data: {ghcr_dockerconfigjson: .data[".dockerconfigjson
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:458:44    - Unknown word (dockerconfigjson)  -- dockerconfigjson: .data[".dockerconfigjson"]}}' \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:478:16    - Unknown word (pushsecrets)       -- if ! grep -qx 'pushsecrets.external-secrets.io
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:479:14    - Unknown word (externalsecrets)   -- ! grep -qx 'externalsecrets.external-secrets.io
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:482:7     - Unknown word (pushsecret)        -- if ! pushsecret_name="$(kubectl \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:485:7     - Unknown word (pushsecret)        -- get pushsecret seed-ghcr \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:491:14    - Unknown word (pushsecret)        -- if [[ -z "${pushsecret_name}" ]]; then
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:495:22    - Unknown word (FANOUT)            -- for namespace in "${FANOUT_NAMESPACES[@]}"; do
	 Suggestions: [FAUT, FAGOT, FANON, FLOUT, FAMOUS]
scripts/refresh-flux-ghcr-auth.sh:496:8     - Unknown word (externalsecret)    -- if ! externalsecret_name="$(kubectl \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:499:8     - Unknown word (externalsecret)    -- get externalsecret ghcr-auth \
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:505:15    - Unknown word (externalsecret)    -- if [[ -z "${externalsecret_name}" ]]; then
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:526:21    - Unknown word (pushsecret)        -- force_sync_resource pushsecret flux-system seed-ghcr
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:527:21    - Unknown word (FANOUT)            -- for namespace in "${FANOUT_NAMESPACES[@]}"; do
	 Suggestions: [FAUT, FAGOT, FANON, FLOUT, FAMOUS]
scripts/refresh-flux-ghcr-auth.sh:528:22    - Unknown word (externalsecret)    -- force_sync_resource externalsecret "${namespace}" ghcr
	 Suggestions: []
scripts/refresh-flux-ghcr-auth.sh:534:9     - Unknown word (Synchronised)      -- echo "✅ Synchronised every existing consumer
	 Suggestions: [Synchronized, Synchronies, Synchronism, Synchronize, Synchronisms]
scripts/run-ksail-prod-with-pull-auth.sh:29:73     - Unknown word (devantler)  -- GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
scripts/run-ksail-prod-with-pull-auth.sh:37:3      - Unknown word (ciphertext) -- # ciphertext revision defeats KSail
	 Suggestions: [ciphered]
scripts/run-ksail-prod-with-pull-auth.sh:47:3      - Unknown word (ksail)      -- ksail --config ksail.prod
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/run-ksail-prod-with-pull-auth.sh:47:18     - Unknown word (ksail)      -- ksail --config ksail.prod.yaml "$1" "$2"
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/run-ksail-prod-with-pull-auth.sh:60:2      - Unknown word (KSAIL)      -- KSAIL_SPEC_CLUSTER_LOCALREGISTRY
	 Suggestions: [KAIL, SAIL, csail, CSAIL, KALI]
scripts/run-ksail-prod-with-pull-auth.sh:60:21     - Unknown word (LOCALREGISTRY) -- KSAIL_SPEC_CLUSTER_LOCALREGISTRY_REGISTRY="${PULL_REGISTRY
	 Suggestions: []
scripts/run-ksail-prod-with-pull-auth.sh:64:3      - Unknown word (ksail)         -- ksail --config ksail.prod
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/run-ksail-prod-with-pull-auth.sh:64:18     - Unknown word (ksail)         -- ksail --config ksail.prod.yaml "$1" "$2"
	 Suggestions: [kail, sail, csail, CSAIL, kali]
scripts/tests/test-cilium-bandwidth-manager-component.sh:110:22    - Unknown word (kustomization) -- readonly controllers_kustomization="${controllers_dir}
	 Suggestions: [customization]
scripts/tests/test-cilium-bandwidth-manager-component.sh:110:56    - Unknown word (kustomization) -- "${controllers_dir}/kustomization.yaml"
	 Suggestions: [customization]
scripts/tests/test-cilium-bandwidth-manager-component.sh:111:75    - Unknown word (kustomization) -- bbr/' "${controllers_kustomization}" ||
	 Suggestions: [customization]
scripts/tests/test-cilium-bandwidth-manager-component.sh:113:76    - Unknown word (kustomization) -- bbr/' "${controllers_kustomization}"; then
	 Suggestions: [customization]
scripts/tests/test-cilium-bandwidth-manager-component.sh:125:47    - Unknown word (restrictor)    -- in_fixture}" --load-restrictor LoadRestrictionsNone
	 Suggestions: [restrict, restricts, retractor, restricted, restriction]
scripts/validate-eks-ci-role-policy/main_test.go:23:65     - Unknown word (nolint)     -- Join(repoRoot, path)) //nolint:gosec // Explicit repository
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/validate-eks-ci-role-policy/main_test.go:23:72     - Unknown word (gosec)      -- repoRoot, path)) //nolint:gosec // Explicit repository
	 Suggestions: [cosec, goes, goer, gogc, gone]
scripts/validate-eks-ci-role-policy/main_test.go:140:27    - Unknown word (kyverno)    -- manifest: `apiVersion: kyverno.io/v1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main_test.go:175:11    - Unknown word (Kyverno)    -- name: "Kyverno generator",
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main_test.go:176:27    - Unknown word (kyverno)    -- manifest: `apiVersion: kyverno.io/v1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main_test.go:207:17    - Unknown word (rolebindings) -- resources: [rolebindings, clusterrolebindings
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main_test.go:207:31    - Unknown word (clusterrolebindings) -- resources: [rolebindings, clusterrolebindings]
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main_test.go:212:11    - Unknown word (Kyverno)             -- name: "Kyverno binding mutation",
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main_test.go:213:27    - Unknown word (kyverno)             -- manifest: `apiVersion: kyverno.io/v1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main_test.go:233:11    - Unknown word (Kyverno)             -- name: "Kyverno RBAC group wildcard
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main_test.go:234:27    - Unknown word (kyverno)             -- manifest: `apiVersion: kyverno.io/v1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main_test.go:253:11    - Unknown word (Kyverno)             -- name: "Kyverno role mutation",
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main_test.go:254:27    - Unknown word (kyverno)             -- manifest: `apiVersion: kyverno.io/v1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main_test.go:277:7     - Unknown word (Kustomization)       -- kind: Kustomization
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main_test.go:285:11    - Unknown word (unreviewed)          -- name: unreviewed
	 Suggestions: [unrenewed, unrevised, unrelieved, unreceived, unreeled]
scripts/validate-eks-ci-role-policy/main_test.go:297:30    - Unknown word (unreviewed)          -- oci://example.invalid/unreviewed
	 Suggestions: [unrenewed, unrevised, unrelieved, unreceived, unreeled]
scripts/validate-eks-ci-role-policy/main_test.go:320:11    - Unknown word (Crossplane)          -- name: "Crossplane package RBAC emitter
	 Suggestions: [Cropland, Crosspiece]
scripts/validate-eks-ci-role-policy/main_test.go:321:31    - Unknown word (crossplane)          -- manifest: `apiVersion: pkg.crossplane.io/v1
	 Suggestions: [cropland, crosspiece]
scripts/validate-eks-ci-role-policy/main_test.go:338:17    - Unknown word (serviceaccounts)     -- resources: [serviceaccounts/token]
	 Suggestions: [servicecount, Servicecount]
scripts/validate-eks-ci-role-policy/main_test.go:350:17    - Unknown word (serviceaccounts)     -- resources: [serviceaccounts]
	 Suggestions: [servicecount, Servicecount]
scripts/validate-eks-ci-role-policy/main_test.go:368:19    - Unknown word (Kyverno)             -- name: "current Kyverno mutating policy",
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main_test.go:431:23    - Unknown word (Kustomization)       -- kinds: [Kustomization]
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main_test.go:473:24    - Unknown word (clusterroles)        -- resources: [roles, clusterroles]
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main_test.go:485:17    - Unknown word (clusterroles)        -- resources: [clusterroles]
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main_test.go:528:27    - Unknown word (upbound)             -- apiGroups: [iam.aws.m.upbound.io]
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main_test.go:550:31    - Unknown word (upbound)             -- apiVersion: iam.aws.m.upbound.io/v1beta1
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main_test.go:604:7     - Unknown word (Kustomization)       -- kind: Kustomization
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main_test.go:657:31    - Unknown word (upbound)             -- apiVersion: iam.aws.m.upbound.io/v1beta1
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main_test.go:704:95    - Unknown word (nolint)              -- workflows", "ci.yaml")) //nolint:gosec // Explicit repository
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/validate-eks-ci-role-policy/main_test.go:704:102   - Unknown word (gosec)               -- ci.yaml")) //nolint:gosec // Explicit repository
	 Suggestions: [cosec, goes, goer, gogc, gone]
scripts/validate-eks-ci-role-policy/main_test.go:774:42    - Unknown word (upbound)             -- boundary is an iam.aws.m.upbound.io/v1beta1 Policy, and
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main_test.go:781:14    - Unknown word (upbound)             -- "iam.aws.m.upbound.io/v1beta1/Policy",
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main_test.go:889:11    - Unknown word (unreviewed)          -- name: "unreviewed role management surface
	 Suggestions: [unrenewed, unrevised, unrelieved, unreceived, unreeled]
scripts/validate-eks-ci-role-policy/main_test.go:938:7     - Unknown word (Kustomization)       -- kind: Kustomization
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main_test.go:998:2     - Unknown word (srole)               -- %sroleRef:
	 Suggestions: [sole, stole, role, prole, sorel]
scripts/validate-eks-ci-role-policy/main_test.go:1030:18   - Unknown word (serviceaccount)      -- name: system:serviceaccount:aws:aws
	 Suggestions: [servicecount, Servicecount, serviceCount, serviceupcount, serviceUpcount]
scripts/validate-eks-ci-role-policy/main_test.go:1038:18   - Unknown word (serviceaccounts)     -- name: system:serviceaccounts:aws
	 Suggestions: [servicecount, Servicecount]
scripts/validate-eks-ci-role-policy/main_test.go:1046:18   - Unknown word (serviceaccounts)     -- name: system:serviceaccounts
	 Suggestions: [servicecount, Servicecount]
scripts/validate-eks-ci-role-policy/main.go:8:52      - Unknown word (unreviewed) -- fingerprints, so an unreviewed privilege grant
	 Suggestions: [unrenewed, unrevised, unrelieved, unreceived, unreeled]
scripts/validate-eks-ci-role-policy/main.go:47:18     - Unknown word (JSONSHA)    -- expectedBoundaryJSONSHA = "e617004bce71a
	 Suggestions: [JONAH, Jonah, JOSH, json, JSON]
scripts/validate-eks-ci-role-policy/main.go:84:26     - Unknown word (upbound)    -- apiVersion: "iam.aws.m.upbound.io/v1beta1", kind:
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main.go:85:26     - Unknown word (upbound)    -- apiVersion: "iam.aws.m.upbound.io/v1beta1", kind:
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main.go:88:80     - Unknown word (crossview)  -- RoleBinding", namespace: "crossview", name: "crossview-portforwar
	 Suggestions: [crosstie, crossties, crosier, crosse, crossed]
scripts/validate-eks-ci-role-policy/main.go:88:99     - Unknown word (crossview)  -- "crossview", name: "crossview-portforward"}:
	 Suggestions: [crosstie, crossties, crosier, crosse, crossed]
scripts/validate-eks-ci-role-policy/main.go:88:109    - Unknown word (portforward) -- crossview", name: "crossview-portforward"}: "7899
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:93:56     - Unknown word (Kustomization) -- fluxcd.io/v1", kind: "Kustomization", namespace: "ascoachingogvan
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main.go:93:84     - Unknown word (ascoachingogvaner) -- Kustomization", namespace: "ascoachingogvaner", name: "ascoachingogvaner
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:93:111    - Unknown word (ascoachingogvaner) -- ascoachingogvaner", name: "ascoachingogvaner"}: "89ea0484e37b
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:94:56     - Unknown word (Kustomization)     -- fluxcd.io/v1", kind: "Kustomization", namespace: "aws",
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main.go:95:56     - Unknown word (Kustomization)     -- fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main.go:96:56     - Unknown word (Kustomization)     -- fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main.go:97:56     - Unknown word (Kustomization)     -- fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system
	 Suggestions: [Customization]
scripts/validate-eks-ci-role-policy/main.go:100:84    - Unknown word (unifi)             -- Kustomization", namespace: "unifi", name: "unifi"}:
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
scripts/validate-eks-ci-role-policy/main.go:100:99    - Unknown word (unifi)             -- namespace: "unifi", name: "unifi"}:
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
scripts/validate-eks-ci-role-policy/main.go:174:29    - Unknown word (Crossplane's)      -- parseJSONPolicy requires Crossplane's embedded IAM policy
	 Suggestions: [Cropland's, Crosspiece's]
scripts/validate-eks-ci-role-policy/main.go:273:61    - Unknown word (JSONSHA)           -- policy, expectedBoundaryJSONSHA, "permissions boundary
	 Suggestions: [JONAH, Jonah, JOSH, json, JSON]
scripts/validate-eks-ci-role-policy/main.go:340:5     - Unknown word (deletecollection)  -- "deletecollection",
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:347:27    - Unknown word (upbound)           -- apiGroup: "iam.aws.m.upbound.io", resources: []string
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main.go:348:25    - Unknown word (upbound)           -- {apiGroup: "iam.aws.upbound.io", resources: []string
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main.go:349:68    - Unknown word (kustomizations)    -- resources: []string{"kustomizations", "*"}},
	 Suggestions: [customization]
scripts/validate-eks-ci-role-policy/main.go:351:63    - Unknown word (helmreleases)      -- resources: []string{"helmreleases", "*"}},
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:352:21    - Unknown word (crossplane)        -- {apiGroup: "pkg.crossplane.io", resources: []string
	 Suggestions: [cropland, crosspiece]
scripts/validate-eks-ci-role-policy/main.go:352:102   - Unknown word (deploymentruntimeconfigs) -- "configurations", "deploymentruntimeconfigs", "*"}},
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:353:17    - Unknown word (kyverno)                  -- {apiGroup: "kyverno.io", resources: []string
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main.go:353:63    - Unknown word (clusterpolicies)          -- string{"policies", "clusterpolicies", "*"}},
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:354:26    - Unknown word (kyverno)                  -- apiGroup: "policies.kyverno.io", resources: []string
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main.go:354:60    - Unknown word (mutatingpolicies)         -- resources: []string{"mutatingpolicies", "generatingpolicies
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:354:80    - Unknown word (generatingpolicies)       -- mutatingpolicies", "generatingpolicies", "*"}},
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:367:6     - Unknown word (clusterroles)             -- "clusterroles",
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:368:6     - Unknown word (rolebindings)             -- "rolebindings",
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:369:6     - Unknown word (clusterrolebindings)      -- "clusterrolebindings",
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:378:6     - Unknown word (deletecollection)         -- "deletecollection",
	 Suggestions: []
scripts/validate-eks-ci-role-policy/main.go:386:43    - Unknown word (serviceaccounts)          -- rule["resources"], "serviceaccounts/token", "*") &&
	 Suggestions: [servicecount, Servicecount]
scripts/validate-eks-ci-role-policy/main.go:391:43    - Unknown word (serviceaccounts)          -- rule["resources"], "serviceaccounts", "*") &&
	 Suggestions: [servicecount, Servicecount]
scripts/validate-eks-ci-role-policy/main.go:399:39    - Unknown word (Kyverno's)                -- CAuthorizationKind recognizes Kyverno's short and group-qualified
	 Suggestions: [Keno's, kern's, Kern's, keven's, Keven's]
scripts/validate-eks-ci-role-policy/main.go:409:6     - Unknown word (AWSIAM)                   -- // isAWSIAMAuthorizationKind recognizes
	 Suggestions: [asam, ASAM, ASIA, ASIAN, ASSAM]
scripts/validate-eks-ci-role-policy/main.go:409:45    - Unknown word (Crossplane)               -- horizationKind recognizes the Crossplane IAM kinds that CARRY
	 Suggestions: [Cropland, Crosspiece]
scripts/validate-eks-ci-role-policy/main.go:415:45    - Unknown word (Kyverno)                  -- protected surface — so a Kyverno selector reaching them
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main.go:417:20    - Unknown word (upbound)                  -- // match iam.aws.m.upbound.io/v1beta1/Policy and
	 Suggestions: [ubound, unbound, unsound, unwound, upcount]
scripts/validate-eks-ci-role-policy/main.go:425:8     - Unknown word (AWSIAM)                   -- func isAWSIAMAuthorizationKind(kind
	 Suggestions: [asam, ASAM, ASIA, ASIAN, ASSAM]
scripts/validate-eks-ci-role-policy/main.go:456:53    - Unknown word (crossplane)               -- identity.apiVersion, "pkg.crossplane.io/")
	 Suggestions: [cropland, crosspiece]
scripts/validate-eks-ci-role-policy/main.go:459:13    - Unknown word (Kyverno)                  -- // isCurrentKyvernoMutationPolicy recognizes
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main.go:459:61    - Unknown word (Kyverno)                  -- recognizes the non-legacy Kyverno resources
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main.go:461:15    - Unknown word (Kyverno)                  -- func isCurrentKyvernoMutationPolicy(identity
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main.go:462:58    - Unknown word (kyverno)                  -- apiVersion, "policies.kyverno.io/") &&
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main.go:466:12    - Unknown word (Kyverno)                  -- // isLegacyKyvernoPolicy recognizes rule
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
scripts/validate-eks-ci-role-policy/main.go:468:49    - Unknown word (kyverno)                  -- identity.apiVersion, "kyverno.io/") &&
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
scripts/validate-eks-ci-role-policy/main.go:475:40    - Unknown word (AWSIAM)                   -- CAuthorizationKind(kind) || isAWSIAMAuthorizationKind(kind
	 Suggestions: [asam, ASAM, ASIA, ASIAN, ASSAM]
scripts/validate-eks-ci-role-policy/main.go:498:32    - Unknown word (crossplane)               -- HasPrefix(kind, "pkg.crossplane.io/") && strings.HasSuffix
	 Suggestions: [cropland, crosspiece]
scripts/validate-eks-ci-role-policy/main.go:637:16    - Unknown word (Ciphertext)               -- // containsSOPSCiphertext finds encrypted scalar
	 Suggestions: [Ciphered]
scripts/validate-eks-ci-role-policy/main.go:639:18    - Unknown word (Ciphertext)               -- func containsSOPSCiphertext(value any) bool {
	 Suggestions: [Ciphered]
scripts/validate-eks-ci-role-policy/main.go:645:19    - Unknown word (Ciphertext)               -- if containsSOPSCiphertext(item) {
	 Suggestions: [Ciphered]
scripts/validate-eks-ci-role-policy/main.go:651:19    - Unknown word (Ciphertext)               -- if containsSOPSCiphertext(item) {
	 Suggestions: [Ciphered]
scripts/validate-eks-ci-role-policy/main.go:662:36    - Unknown word (Ciphertext)               -- hasMetadata || containsSOPSCiphertext(document)
	 Suggestions: [Ciphered]
scripts/validate-eks-ci-role-policy/main.go:1087:55   - Unknown word (nolint)                   -- ctx, name, args...) //nolint:gosec // Fixed binary
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/validate-eks-ci-role-policy/main.go:1087:62   - Unknown word (gosec)                    -- name, args...) //nolint:gosec // Fixed binary and
	 Suggestions: [cosec, goes, goer, gogc, gone]
scripts/validate-eks-ci-role-policy/main.go:1135:72   - Unknown word (nolint)                   -- roleManifestPath)) //nolint:gosec // Explicit repository
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/validate-eks-ci-role-policy/main.go:1135:79   - Unknown word (gosec)                    -- roleManifestPath)) //nolint:gosec // Explicit repository
	 Suggestions: [cosec, goes, goer, gogc, gone]
scripts/validate-eks-ci-role-policy/main.go:1140:80   - Unknown word (nolint)                   -- boundaryManifestPath)) //nolint:gosec // Explicit repository
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/validate-eks-ci-role-policy/main.go:1140:87   - Unknown word (gosec)                    -- undaryManifestPath)) //nolint:gosec // Explicit repository
	 Suggestions: [cosec, goes, goer, gogc, gone]
scripts/validate-merge-group-heal/main.go:7:60      - Unknown word (preemptible) -- that lock must not be preemptible, or the
	 Suggestions: [preemptive, preemptively, preempting, preemption, preemptions]
scripts/validate-merge-group-heal/main.go:125:47    - Unknown word (nolint)      -- ReadFile(workflowPath) //nolint:gosec // The explicit
	 Suggestions: [online, nlist, nolan, nolet, nosing]
scripts/validate-merge-group-heal/main.go:125:54    - Unknown word (gosec)       -- workflowPath) //nolint:gosec // The explicit CLI
	 Suggestions: [cosec, goes, goer, gogc, gone]
SECURITY.md:60:35     - Unknown word (ksail)      -- configuration files (`ksail.yaml`, `ksail.prod.yaml
	 Suggestions: [kail, sail, csail, CSAIL, kali]
SECURITY.md:60:49     - Unknown word (ksail)      -- files (`ksail.yaml`, `ksail.prod.yaml`). KSail's
	 Suggestions: [kail, sail, csail, CSAIL, kali]
SECURITY.md:71:3      - Unknown word (ciphertext) -- ciphertext is intentionally checked
	 Suggestions: [ciphered]
SECURITY.md:71:57     - Unknown word (ciphertext) -- checked into git; finding ciphertext is not a
	 Suggestions: [ciphered]
SECURITY.md:80:39     - Unknown word (ciphertext) -- recipients). Reporting that ciphertext is present in git is
	 Suggestions: [ciphered]
SECURITY.md:83:31     - Unknown word (Fulcio)     -- cosign keyless signing (Fulcio + Rekor) against the
	 Suggestions: [Lucio, Folio, Fileio, Fulcra, Fulfil]
SECURITY.md:83:40     - Unknown word (Rekor)      -- keyless signing (Fulcio + Rekor) against the GitHub
	 Suggestions: [regor, Regor, Error, Repro, Retro]
talos-local/cluster/enable-dex-oidc.yaml:1:35      - Unknown word (apiserver)  -- configuration for the kube-apiserver.
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, apiserverid]
talos/cluster/authenticate-ghcr-pulls.yaml:4:37      - Unknown word (devantler)  -- signature of every ghcr.io/devantler-tech/* image at the
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/authenticate-ghcr-pulls.yaml:14:58     - Unknown word (ascoachingogvaner) -- evicts its cache (how ascoachingogvaner
	 Suggestions: []
talos/cluster/authenticate-ghcr-pulls.yaml:18:26     - Unknown word (ksail's)           -- endpoint redirect. (ksail's --mirror-registry flag
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
talos/cluster/authenticate-ghcr-pulls.yaml:24:32     - Unknown word (ksail)             -- TOKEN} is expanded by ksail when it loads this patch
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/authenticate-ghcr-pulls.yaml:26:7      - Unknown word (ksail)             -- # run-ksail-prod-with-pull-auth
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/authenticate-ghcr-pulls.yaml:28:19     - Unknown word (ciphertext)        -- # non-secret SOPS ciphertext revision because KSail
	 Suggestions: [ciphered]
talos/cluster/authenticate-ghcr-pulls.yaml:36:43     - Unknown word (ksail)             -- pull the exact live ksail-operator image successfully
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/block-ingress-by-default.yaml:5:29      - Unknown word (siderolink) -- Allows traffic on lo, siderolink, kubespan interfaces
	 Suggestions: [siderolite, sideline, sideling, siderosis]
talos/cluster/block-ingress-by-default.yaml:5:41      - Unknown word (kubespan)   -- traffic on lo, siderolink, kubespan interfaces
	 Suggestions: [kubespec, kuban, keresan, kneepan]
talos/cluster/enable-apparmor.yaml:8:53      - Unknown word (Cmdline)    -- and install.grubUseUKICmdline=true:
	 Suggestions: [cline, Cline, Carline, Cauline, Choline]
talos/cluster/enable-apparmor.yaml:9:56      - Unknown word (Cmdline)    -- and install.grubUseUKICmdline can't be used together
	 Suggestions: [cline, Cline, Carline, Cauline, Choline]
talos/cluster/enable-apparmor.yaml:10:21     - Unknown word (Cmdline)    -- # and grubUseUKICmdline defaults to TRUE here
	 Suggestions: [cline, Cline, Carline, Cauline, Choline]
talos/cluster/enable-apparmor.yaml:12:21     - Unknown word (Cmdline)    -- # for grubUseUKICmdline is true (gated at >
	 Suggestions: [cline, Cline, Carline, Cauline, Choline]
talos/cluster/enable-apparmor.yaml:14:27     - Unknown word (Cmdline)    -- defaulted grubUseUKICmdline:true, so the v1.13.
	 Suggestions: [cline, Cline, Carline, Cauline, Choline]
talos/cluster/enable-apparmor.yaml:15:24     - Unknown word (ksail)      -- # mid-upgrade and `ksail cluster update` aborts
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/enable-apparmor.yaml:18:29     - Unknown word (cmdline)    -- UKI's baked-in kernel cmdline instead of building
	 Suggestions: [cline, carline, cauline, choline, codeine]
talos/cluster/enable-apparmor.yaml:19:65     - Unknown word (cmdline)    -- SecureBoot/UKI), so the cmdline MUST be
	 Suggestions: [cline, carline, cauline, choline, codeine]
talos/cluster/enable-audit-logging.yaml:24:18     - Unknown word (healthz)    -- - /healthz*
	 Suggestions: [health, healths, healthy, Health, heath]
talos/cluster/enable-audit-logging.yaml:25:18     - Unknown word (readyz)     -- - /readyz*
	 Suggestions: [ready, read, readd, reade, reads]
talos/cluster/enable-audit-logging.yaml:26:18     - Unknown word (livez)      -- - /livez*
	 Suggestions: [live, lived, liven, liver, lives]
talos/cluster/enable-audit-logging.yaml:57:21     - Unknown word (serviceaccounts) -- - serviceaccounts
	 Suggestions: [serviceaccount, serviceAccount, servicecount, Servicecount]
talos/cluster/enable-audit-logging.yaml:81:17     - Unknown word (maxage)          -- audit-log-maxage: "30"
	 Suggestions: [manage, mage, madag, madge, mange]
talos/cluster/enable-audit-logging.yaml:82:17     - Unknown word (maxbackup)       -- audit-log-maxbackup: "3"
	 Suggestions: []
talos/cluster/enable-audit-logging.yaml:83:17     - Unknown word (maxsize)         -- audit-log-maxsize: "100"
	 Suggestions: [maize, maisie, maxine, maxixe, massive]
talos/cluster/enable-dex-oidc.yaml:1:35      - Unknown word (apiserver)  -- configuration for the kube-apiserver on Hetzner clusters
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, apiserverid]
talos/cluster/enable-dex-oidc.yaml:5:20      - Unknown word (apiserver)  -- # On Hetzner, kube-apiserver reaches Dex through
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, apiserverid]
talos/cluster/enable-dex-oidc.yaml:10:57     - Unknown word (ksail)      -- talos/ directory used by ksail.prod.yaml.
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/enable-dex-oidc.yaml:12:23     - Unknown word (ksail)      -- in the future (e.g. ksail.dev.yaml), create a
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/encrypt-ephemeral-volume.yaml:1:62      - Unknown word (LUKS)       -- nodeID-derived key (LUKS2).
	 Suggestions: [LEKS, LU'S, LUES, LUGS, LUIS]
talos/cluster/encrypt-ephemeral-volume.yaml:19:13     - Unknown word (luks)       -- provider: luks2
	 Suggestions: [leks, lu's, lues, lugs, luis]
talos/cluster/encrypt-state-volume.yaml:1:58      - Unknown word (LUKS)       -- nodeID-derived key (LUKS2).
	 Suggestions: [LEKS, LU'S, LUES, LUGS, LUIS]
talos/cluster/encrypt-state-volume.yaml:18:13     - Unknown word (luks)       -- provider: luks2
	 Suggestions: [leks, lu's, lues, lugs, luis]
talos/cluster/gc-terminated-pods-sooner.yaml:7:4       - Unknown word (ksail)      -- # `ksail cluster update` upgrades
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/gc-terminated-pods-sooner.yaml:18:68     - Unknown word (inspectable) -- recent failures stay inspectable.
	 Suggestions: [injectable, inseparable, instable, insectile, indictable]
talos/cluster/harden-kernel-sysctls.yaml:4:14      - Unknown word (rmem)       -- net.core.rmem_max: "7500000"
	 Suggestions: [rem, REM, mem, rems, rime]
talos/cluster/harden-kernel-sysctls.yaml:5:14      - Unknown word (wmem)       -- net.core.wmem_max: "7500000"
	 Suggestions: [mem, mwei, wame, gemm, mme]
talos/cluster/harden-kernel-sysctls.yaml:10:12     - Unknown word (kptr)       -- kernel.kptr_restrict: "2"
	 Suggestions: [tptr, tPtr, kart, kurt, Kurt]
talos/cluster/harden-kernel-sysctls.yaml:15:48     - Unknown word (neighbour)  -- attaching a debugger to a neighbour in the same
	 Suggestions: [neighbor, neighbors, neighbor's, neighbored, neighborly]
talos/cluster/mark-ghcr-pull-revision.yaml:5:3       - Unknown word (ciphertext) -- # ciphertext gives token rotations
	 Suggestions: [ciphered]
talos/cluster/mark-ghcr-pull-revision.yaml:14:14     - Unknown word (devantler)  -- platform.devantler.tech/ghcr-pull-desired
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/use-platform-hostname.yaml:11:9      - Unknown word (cloudprovider) -- # `node.cloudprovider.kubernetes.io/uninitialized
	 Suggestions: []
talos/cluster/use-platform-hostname.yaml:18:46     - Unknown word (behaviour)     -- only restores the prior behaviour for upgraded nodes.
	 Suggestions: [behavior, behaviors, behaver, behaving, belabour]
talos/cluster/verify-first-party-images.yaml:7:16      - Unknown word (Kyverno)    -- # Flux and the Kyverno admission layer never
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
talos/cluster/verify-first-party-images.yaml:10:38     - Unknown word (devantler)  -- PARTY images (ghcr.io/devantler-tech/*). All are keyless
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/verify-first-party-images.yaml:12:16     - Unknown word (devantler)  -- # 1. ghcr.io/devantler-tech/ksail — the ksail
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/verify-first-party-images.yaml:12:31     - Unknown word (ksail)      -- ghcr.io/devantler-tech/ksail — the ksail-operator
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/verify-first-party-images.yaml:12:43     - Unknown word (ksail)      -- devantler-tech/ksail — the ksail-operator image — is
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/verify-first-party-images.yaml:13:8      - Unknown word (ksail's)    -- # ksail's OWN release pipeline
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
talos/cluster/verify-first-party-images.yaml:13:38     - Unknown word (devantler)  -- OWN release pipeline (devantler-tech/ksail .github/workflows
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/verify-first-party-images.yaml:13:53     - Unknown word (ksail)      -- pipeline (devantler-tech/ksail .github/workflows/
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/verify-first-party-images.yaml:15:37     - Unknown word (ascoachingogvaner) -- images (wedding-app, ascoachingogvaner, …) are signed by the
	 Suggestions: []
talos/cluster/verify-first-party-images.yaml:16:50     - Unknown word (devantler)         -- yaml workflow — now in devantler-tech/actions
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/verify-first-party-images.yaml:21:69     - Unknown word (ksail)             -- wins, so the specific ksail rule
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/verify-first-party-images.yaml:22:28     - Unknown word (devantler)         -- precede the ghcr.io/devantler-tech/* catch-all.
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/cluster/verify-first-party-images.yaml:25:72     - Unknown word (Coroot)            -- images (Cilium, Longhorn, Coroot,
	 Suggestions: [corot, Corot, Chroot, Coot, Coopt]
talos/cluster/verify-first-party-images.yaml:32:33     - Unknown word (talosctl)          -- cluster given the manual-talosctl recovery path — so it
	 Suggestions: [talos]
talos/cluster/verify-first-party-images.yaml:35:25     - Unknown word (kubescape)         -- This does NOT satisfy kubescape C-0237 (see
	 Suggestions: [unescape, kubespec]
talos/cluster/verify-first-party-images.yaml:45:5      - Unknown word (ksail)             -- # ksail-operator image — signed
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/cluster/verify-first-party-images.yaml:45:38     - Unknown word (ksail's)           -- operator image — signed by ksail's OWN release pipeline
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
talos/cluster/verify-first-party-images.yaml:54:5      - Unknown word (Crossplane)        -- # Crossplane PROVIDER package images
	 Suggestions: [Cropland, Crosspiece]
talos/cluster/verify-first-party-images.yaml:54:73     - Unknown word (upjet)             -- devantler-tech/provider-upjet-*)
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
talos/cluster/verify-first-party-images.yaml:57:5      - Unknown word (crossplane)        -- # crossplane-contrib publish reusable
	 Suggestions: [cropland, crosspiece]
talos/cluster/verify-first-party-images.yaml:58:66     - Unknown word (upjet)             -- mageValidatingPolicy provider-upjet-*
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
talos/cluster/verify-first-party-images.yaml:61:44     - Unknown word (upjet)             -- devantler-tech/provider-upjet-*
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
talos/control-planes/allow-internal-node-ingress.yaml:4:5       - Unknown word (trustd)     -- # trustd 50001, NodePort range
	 Suggestions: [trust, trusts, trusty, trusted, Trust]
talos/control-planes/allow-internal-node-ingress.yaml:6:33      - Unknown word (Clusterwide) -- require-mutual-auth CiliumClusterwideNetworkPolicy marks
	 Suggestions: [Clusterid, Clustered, Clusterip, Clustering, Clusterips]
talos/control-planes/allow-internal-node-ingress.yaml:12:41     - Unknown word (ENOBUFS)     -- add new rules (see the ENOBUFS note in
	 Suggestions: [NETBUF, netBuf, ENOS, EMBUS, ENUMS]
talos/control-planes/allow-internal-nodepod-ingress.yaml:9:41      - Unknown word (ENOBUFS)    -- add new rules (see the ENOBUFS note in
	 Suggestions: [NETBUF, netBuf, ENOS, EMBUS, ENUMS]
talos/control-planes/allow-internal-nodepod-ingress.yaml:13:30     - Unknown word (nodepod)    -- control-plane-internal-nodepod-ingress
	 Suggestions: [nodep, nodeid, nodeptr, notepad, nodeport]
talos/control-planes/allow-internal-udp-ingress.yaml:3:12      - Unknown word (VXLAN)      -- # Cilium VXLAN 8472, Cilium WireGuard
	 Suggestions: [vlan, VLAN, VILNA, Vilna, VALA]
talos/control-planes/allow-internal-udp-ingress.yaml:5:3       - Unknown word (VXLAN)      -- # VXLAN-encapsulated pod traffic
	 Suggestions: [vlan, VLAN, VILNA, Vilna, VALA]
talos/control-planes/allow-internal-udp-ingress.yaml:9:41      - Unknown word (ENOBUFS)    -- add new rules (see the ENOBUFS note in
	 Suggestions: [NETBUF, netBuf, ENOS, EMBUS, ENUMS]
talos/control-planes/allow-public-ingress.yaml:2:27      - Unknown word (apid)       -- Kubernetes API (6443) + apid (50000). Pairs with
	 Suggestions: [paid, acid, amid, aped, api3]
talos/control-planes/allow-public-ingress.yaml:7:43      - Unknown word (netlink)    -- through a bare, un-tuned netlink conn (google/nftables
	 Suggestions: [netlify, netlike, netting, newline, nestling]
talos/control-planes/allow-public-ingress.yaml:7:64      - Unknown word (nftables)   -- netlink conn (google/nftables
	 Suggestions: [notables, notable, notable's, fables, tables]
talos/control-planes/allow-public-ingress.yaml:8:64      - Unknown word (ENOBUFS)    -- conn.Flush() fail with ENOBUFS; the
	 Suggestions: [NETBUF, netBuf, ENOS, EMBUS, ENUMS]
talos/control-planes/allow-public-ingress.yaml:10:15     - Unknown word (apid)       -- # "Booting" — apid/etcd/kubelet never start
	 Suggestions: [paid, acid, amid, aped, api3]
talos/control-planes/allow-public-ingress.yaml:12:50     - Unknown word (nftables)   -- upgrade. See google/nftables#103 and #235.
	 Suggestions: [notables, notable, notable's, fables, tables]
talos/control-planes/allow-public-ingress.yaml:14:43     - Unknown word (ksail)      -- CIDR: 10.0.0.0/16 (from ksail.prod.yaml networkCidr
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/control-planes/wireguard.yaml:5:19      - Unknown word (devantler)  -- # Pairs with the `devantler-tech/unifi` tenant's
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
talos/control-planes/wireguard.yaml:5:34      - Unknown word (unifi)      -- the `devantler-tech/unifi` tenant's `unifi_network
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
talos/control-planes/wireguard.yaml:5:51      - Unknown word (unifi)      -- tech/unifi` tenant's `unifi_network.cluster_wireguard
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
talos/control-planes/wireguard.yaml:7:78      - Unknown word (DDNS)       -- IP never matters (no DDNS).
	 Suggestions: [DNS, DD'S, DDOS, DDoS, DDTS]
talos/control-planes/wireguard.yaml:9:33      - Unknown word (ksail)      -- MATERIAL (env-expanded by ksail at load time — never
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/control-planes/wireguard.yaml:12:32     - Unknown word (ksail)      -- `ksail cluster create/update
	 Suggestions: [kail, sail, csail, CSAIL, kali]
talos/control-planes/wireguard.yaml:14:36     - Unknown word (genkey)     -- (`wg genkey | tee priv | wg pubkey
	 Suggestions: [geeky, gene, genes, genet, genie]
talos/control-planes/wireguard.yaml:20:56     - Unknown word (upjet)      -- exist until provider-upjet-unifi applies the
	 Suggestions: [upset, upnet, upNet, Upnet, UpNet]
talos/control-planes/wireguard.yaml:20:62     - Unknown word (unifi)      -- until provider-upjet-unifi applies the
	 Suggestions: [unfi, unify, unific, UNFI, unfit]
talos/control-planes/wireguard.yaml:30:72     - Unknown word (datapath)   -- gateway VIP is follow-on datapath
	 Suggestions: [dataauth, dataAuth, datadata, dataData, datapage]
talos/control-planes/wireguard.yaml:34:40     - Unknown word (ksail's)    -- applying to EXISTING nodes: ksail's `cluster update` diff
	 Suggestions: [sail's, kali's, Kali's, basil's, Basil's]
talos/control-planes/wireguard.yaml:40:5      - Unknown word (talosctl)   -- # talosctl --nodes <control-plane
	 Suggestions: [talos]
talos/control-planes/wireguard.yaml:45:9      - Unknown word (repoint)    -- # dies, repoint the gateway's peer endpoint
	 Suggestions: [repaint, reprint, repin, recoin, rejoin]
talos/workers/allow-apid-ingress.yaml:1:3       - Unknown word (apid)       -- # apid (50000) open to all
	 Suggestions: [paid, acid, amid, aped, api3]
talos/workers/allow-apid-ingress.yaml:4:31      - Unknown word (ENOBUFS)    -- set matches (see the ENOBUFS note in
	 Suggestions: [NETBUF, netBuf, ENOS, EMBUS, ENUMS]
talos/workers/allow-apid-ingress.yaml:8:7       - Unknown word (apid)       -- name: apid-ingress
	 Suggestions: [paid, acid, amid, aped, api3]
talos/workers/allow-cilium-mutual-auth-ingress.yaml:4:41      - Unknown word (Clusterwide) -- require-mutual-auth CiliumClusterwideNetworkPolicy marks
	 Suggestions: [Clusterid, Clustered, Clusterip, Clustering, Clusterips]
talos/workers/allow-cilium-mutual-auth-ingress.yaml:10:47     - Unknown word (coroot)      -- That silently broke the coroot-heartbeat dead-man's
	 Suggestions: [corot, Corot, chroot, coot, coopt]
talos/workers/allow-cilium-mutual-auth-ingress.yaml:11:15     - Unknown word (coroot)      -- # switch: its coroot-prometheus health-gate
	 Suggestions: [corot, Corot, chroot, coot, coopt]
talos/workers/allow-cilium-mutual-auth-ingress.yaml:12:58     - Unknown word (healthchecks) -- node, so the external healthchecks.io
	 Suggestions: []
talos/workers/allow-cilium-wireguard-ingress.yaml:2:11      - Unknown word (VXLAN)      -- # Tunnels VXLAN-encapsulated pod traffic
	 Suggestions: [vlan, VLAN, VILNA, Vilna, VALA]
talos/workers/allow-cni-vxlan-ingress.yaml:1:10      - Unknown word (VXLAN)      -- # Cilium VXLAN (8472) cluster-internal
	 Suggestions: [vlan, VLAN, VILNA, Vilna, VALA]
talos/workers/allow-cni-vxlan-ingress.yaml:5:11      - Unknown word (vxlan)      -- name: cni-vxlan-ingress
	 Suggestions: [vlan, VLAN, vilna, Vilna, vala]
talos/workers/load-kvm-modules.yaml:1:7       - Unknown word (Virt)       -- # KubeVirt requires KVM kernel
	 Suggestions: [Vert, Vidt, Virtu, VIDT, Airt]
talos/workers/mount-longhorn-data.yaml:22:13     - Unknown word (rshared)    -- - rshared
	 Suggestions: [shared, rared, reared, roared, rehired]
tests/validate-host-restrictions/kyverno-test.yaml:2:17      - Unknown word (kyverno)    -- apiVersion: cli.kyverno.io/v1alpha1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
tests/validate-host-restrictions/kyverno-test.yaml:19:9      - Unknown word (userns)     -- - userns-enabled/explicit-host
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/kyverno-test.yaml:26:9      - Unknown word (userns)     -- - userns-enabled/explicit-unshared
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/kyverno-test.yaml:27:9      - Unknown word (userns)     -- - userns-enabled/host-users-absent
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/kyverno-test.yaml:30:5      - Unknown word (CNPG)       -- # CNPG pods are excluded, and
	 Suggestions: [cnp, CNP, PNG, CAPE, CAPH]
tests/validate-host-restrictions/kyverno-test.yaml:34:9      - Unknown word (userns)     -- - userns-enabled/cnpg-instance
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/kyverno-test.yaml:34:24     - Unknown word (cnpg)       -- - userns-enabled/cnpg-instance
	 Suggestions: [cnp, CNP, png, cape, caph]
tests/validate-host-restrictions/kyverno-test.yaml:35:9      - Unknown word (userns)     -- - userns-absent/unscoped-host
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/resources.yaml:6:9       - Unknown word (userns)     -- name: userns-enabled
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/resources.yaml:8:18      - Unknown word (devantler)  -- pod-security.devantler.tech/user-namespaces
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
tests/validate-host-restrictions/resources.yaml:10:46     - Unknown word (userns)     -- graduated through a userns pilot — the rule must
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/resources.yaml:14:9      - Unknown word (userns)     -- name: userns-absent
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/resources.yaml:22:14     - Unknown word (userns)     -- namespace: userns-enabled
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/resources.yaml:34:14     - Unknown word (userns)     -- namespace: userns-enabled
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/resources.yaml:52:3      - Unknown word (CNPG)       -- # CNPG-managed pod inside an
	 Suggestions: [cnp, CNP, PNG, CAPE, CAPH]
tests/validate-host-restrictions/resources.yaml:53:50     - Unknown word (CNPG)       -- namespace (stateless app + CNPG database) stays safe
	 Suggestions: [cnp, CNP, PNG, CAPE, CAPH]
tests/validate-host-restrictions/resources.yaml:57:9      - Unknown word (cnpg)       -- name: cnpg-instance
	 Suggestions: [cnp, CNP, png, cape, caph]
tests/validate-host-restrictions/resources.yaml:60:5      - Unknown word (cnpg)       -- cnpg.io/cluster: demo-db
	 Suggestions: [cnp, CNP, png, cape, caph]
tests/validate-host-restrictions/values.yaml:2:7       - Unknown word (Kyverno)    -- # The Kyverno CLI does not read namespace
	 Suggestions: [Wyvern, Wyverns, Hyperon, Keno, Kern]
tests/validate-host-restrictions/values.yaml:6:17      - Unknown word (kyverno)    -- apiVersion: cli.kyverno.io/v1alpha1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
tests/validate-host-restrictions/values.yaml:11:11     - Unknown word (userns)     -- - name: userns-enabled
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-host-restrictions/values.yaml:13:20     - Unknown word (devantler)  -- pod-security.devantler.tech/user-namespaces
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
tests/validate-host-restrictions/values.yaml:14:11     - Unknown word (userns)     -- - name: userns-absent
	 Suggestions: [users, user's, suers, urns, user]
tests/validate-replica-floor/kyverno-test.yaml:2:17      - Unknown word (kyverno)    -- apiVersion: cli.kyverno.io/v1alpha1
	 Suggestions: [wyvern, wyverns, hyperon, keno, kern]
tests/validate-replica-floor/kyverno-test.yaml:30:31     - Unknown word (apiserver)  -- keda-operator-metrics-apiserver
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, apiserverid]
tests/validate-replica-floor/resources.yaml:46:14     - Unknown word (devantler)  -- platform.devantler.tech/replica-floor:
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
tests/validate-replica-floor/resources.yaml:75:18     - Unknown word (devantler)  -- platform.devantler.tech/replica-floor:
	 Suggestions: [decanter, deventer, Deventer, desalter, defaulter]
tests/validate-replica-floor/resources.yaml:85:14     - Unknown word (velero)     -- namespace: velero
	 Suggestions: [valero, velcro, Valero, Velcro, veer]
tests/validate-replica-floor/resources.yaml:103:31    - Unknown word (apiserver)  -- keda-operator-metrics-apiserver
	 Suggestions: [zipserver, Zipserver, zipServer, ZipServer, apiserverid]
CSpell: Files checked: 109, Issues found: 1624 in 87 files.


You can skip this misspellings by defining the following .cspell.json file at the root of your repository
Of course, please correct real typos before :)

{
    "version": "0.2",
    "language": "en",
    "ignorePaths": [
        "**/node_modules/**",
        "**/vscode-extension/**",
        "**/.git/**",
        "**/.pnpm-lock.json",
        ".vscode",
        "package-lock.json",
        "megalinter-reports"
    ],
    "words": [
        "AWSIAM",
        "Alertmanager",
        "Alertmanager's",
        "BKSAIL",
        "Bitnami",
        "Bitwarden",
        "CAROOT",
        "CISA",
        "CNPG",
        "CNPG's",
        "Ciphertext",
        "Clusterwide",
        "Cmdline",
        "Coroot",
        "Coroot's",
        "Crossplane",
        "Crossplane's",
        "Crossview",
        "DDNS",
        "DNAT'd",
        "Datapath",
        "Descheduler",
        "Devantler",
        "ENOBUFS",
        "FANOUT",
        "Farah",
        "Fulcio",
        "Fulcio's",
        "HSTS",
        "Initialise",
        "JSONSHA",
        "KSAIL",
        "KUBECONFIG",
        "Karpenter",
        "Kopia",
        "Kubescape",
        "Kubescape's",
        "Kustomization",
        "Kustomizations",
        "Kyverno",
        "Kyverno's",
        "LOCALREGISTRY",
        "LUKS",
        "Opsgenie",
        "PITR",
        "Prereqs",
        "Productionisation",
        "Productionising",
        "Rekor",
        "SLSA",
        "SNAT'd",
        "SPIFFE",
        "SPOF",
        "SVID",
        "SVIDs",
        "SYFT",
        "Shamir",
        "Sigstore",
        "Sigstore's",
        "Synchronised",
        "Syscall",
        "TOCTOU",
        "Testkube",
        "Umami",
        "VXLAN",
        "Valkey",
        "Velero",
        "Velero's",
        "Virt",
        "WLANs",
        "Yubi",
        "ZIZMOR",
        "alertmanager",
        "alertname",
        "analysed",
        "anchore",
        "antiaffinity",
        "apid",
        "apiserver",
        "apiserver's",
        "artipacked",
        "ascoachingogvaner",
        "ascoachingogvaner's",
        "authorised",
        "automerged",
        "automount",
        "autoscalers",
        "backupstoragelocations",
        "baselining",
        "bcdfghjklmnpqrstvwxz",
        "behaviour",
        "behavioural",
        "betterleaks",
        "bitnami",
        "blocklist",
        "browsable",
        "buildx",
        "burstable",
        "callsite",
        "chokepoint",
        "ciphertext",
        "clickhouse",
        "cloudnative",
        "cloudprovider",
        "clusterpolicies",
        "clusterrolebindings",
        "clusterroles",
        "cmdline",
        "cnpg",
        "configmap",
        "controlplane",
        "controlplaneio",
        "cooldown",
        "coroot",
        "crashloop",
        "crashloops",
        "creds",
        "crossplane",
        "crossview",
        "curlrc",
        "customise",
        "cutover",
        "cyclonedx",
        "datapath",
        "datreeio",
        "dbname",
        "defence",
        "deletecollection",
        "deploymentruntimeconfigs",
        "descheduler",
        "descheduling",
        "devantler",
        "diffable",
        "dockerconfigjson",
        "dorny",
        "dpkg",
        "endgroup",
        "envsubst",
        "esac",
        "etcdctl",
        "externalsecret",
        "externalsecrets",
        "fanout",
        "featureflagsource",
        "fleetdm",
        "gatewayapi",
        "generatable",
        "generatingpolicies",
        "genkey",
        "gethomepage",
        "gitops",
        "golangci",
        "gosec",
        "grjtvs",
        "growfs",
        "growpart",
        "healthchecks",
        "healthz",
        "helmrelease",
        "helmreleases",
        "helmv",
        "homelab",
        "hostnames",
        "httproute",
        "imagetools",
        "imranismail",
        "inspectable",
        "iscsi",
        "keypair",
        "kprobes",
        "kptr",
        "krew",
        "ksail",
        "ksail's",
        "kubeconfig",
        "kubeconform",
        "kubeconform's",
        "kubelet",
        "kubelets",
        "kubelogin",
        "kubescape",
        "kubespan",
        "kubevirt",
        "kubevuln",
        "kustomization",
        "kustomizations",
        "kyverno",
        "libc",
        "libgnutls",
        "lintable",
        "livez",
        "loadtester",
        "luks",
        "lycheeignore",
        "machineconfig",
        "materialise",
        "materialised",
        "materialises",
        "maxage",
        "maxbackup",
        "maxsize",
        "misconfigs",
        "mktemp",
        "mlock",
        "mutatingpolicies",
        "myapp",
        "najsk",
        "neighbour",
        "netlink",
        "netpol",
        "netpols",
        "nftables",
        "nilnil",
        "nodepod",
        "nodeport",
        "nolint",
        "openbao",
        "opencost",
        "openfeature",
        "oras",
        "overprovisioning",
        "parallelised",
        "pasteable",
        "permissioning",
        "pipefail",
        "policyignore",
        "policyreports",
        "portforward",
        "preemptible",
        "preservingly",
        "prioritised",
        "privesc",
        "providerconfigs",
        "pushsecret",
        "pushsecrets",
        "rdqwpktr",
        "readyz",
        "recognise",
        "reconverges",
        "regenerable",
        "releaserc",
        "rematerialise",
        "rematerialised",
        "repoint",
        "repointed",
        "repoints",
        "repositoryrulesets",
        "resizer",
        "restrictor",
        "rmem",
        "rolebindings",
        "rollouts",
        "rshared",
        "seccomp",
        "secretbox",
        "secretstore",
        "seedable",
        "serviceaccount",
        "serviceaccounts",
        "sgdisk",
        "shellcheck",
        "siderolabs",
        "siderolink",
        "sigstore",
        "skmde",
        "spiffe",
        "srole",
        "statefulset",
        "statemanager",
        "stdlib",
        "storageclass",
        "syft",
        "syft's",
        "syscall",
        "sysctls",
        "talosconfig",
        "talosctl",
        "tanzu",
        "templatesyncignore",
        "thresholded",
        "tracepoints",
        "trixie",
        "trustd",
        "umami",
        "umami's",
        "uncertifiable",
        "unifi",
        "unrecognised",
        "unreviewed",
        "unroutable",
        "unshippable",
        "untrackable",
        "upbound",
        "updatekeys",
        "upjet",
        "upstreaming",
        "urlencode",
        "userns",
        "ushfn",
        "validatable",
        "vcunav",
        "velero",
        "volumesnapshot",
        "vxlan",
        "wffc",
        "wgpolicyk",
        "wmem",
        "worktrees",
        "yannh",
        "yubikey",
        "yzwvjjmcyfnl",
        "zizmor"
    ]
}


You can also copy-paste megalinter-reports/.cspell.json at the root of your repository

(Truncated to last 133333 characters out of 269687)
```

</details>

<details>
<summary>⚠️ COPYPASTE / jscpd - 17 errors</summary>

```
Using config from /action/lib/.automation/.jscpd.json
Clone found (python)
 - scripts/tests/test_refresh_flux_ghcr_auth.py [633:56 - 645:69] (13 lines, 81 tokens)
   scripts/tests/test_refresh_flux_ghcr_auth.py [691:66 - 703:69]
Clone found (python)
 - scripts/tests/test_refresh_flux_ghcr_auth.py [732:9 - 738:72] (7 lines, 66 tokens)
   scripts/tests/test_refresh_flux_ghcr_auth.py [1153:9 - 1158:72]
Clone found (python)
 - scripts/tests/test_refresh_flux_ghcr_auth.py [1274:58 - 1280:59] (7 lines, 52 tokens)
   scripts/tests/test_refresh_flux_ghcr_auth.py [1327:43 - 1333:51]
Clone found (python)
 - scripts/tests/test_validate_homepage_bookmarks.py [46:57 - 54:54] (9 lines, 58 tokens)
   scripts/tests/test_validate_homepage_bookmarks.py [100:53 - 109:54]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [244:50 - 249:24] (6 lines, 103 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [756:31 - 761:24]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [276:14 - 282:4] (7 lines, 115 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [603:43 - 609:4]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [388:5 - 393:2] (6 lines, 82 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [574:8 - 579:9]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [389:1 - 399:23] (11 lines, 220 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [666:60 - 677:3]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [393:15 - 404:2] (12 lines, 185 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [494:17 - 505:2]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [440:15 - 451:12] (12 lines, 264 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [649:1 - 662:3]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [503:2 - 509:19] (7 lines, 89 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [688:7 - 694:19]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [503:2 - 509:33] (7 lines, 103 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [943:58 - 949:33]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [503:2 - 509:4] (7 lines, 74 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [1056:5 - 1062:4]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [596:30 - 601:8] (6 lines, 50 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [623:44 - 628:8]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [881:33 - 889:11] (9 lines, 118 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [893:130 - 901:11]
Clone found (go)
 - scripts/validate-eks-ci-role-policy/main_test.go [1088:47 - 1093:2] (6 lines, 166 tokens)
   scripts/validate-eks-ci-role-policy/main_test.go [1142:46 - 1147:2]
Clone found (python)
 - scripts/validate-naming.py [126:52 - 132:25] (7 lines, 53 tokens)
   scripts/validate-naming.py [171:82 - 177:29]
┌────────┬────────────────┬─────────────┬──────────────┬──────────────┬──────────────────┬───────────────────┐
│ Format │ Files analyzed │ Total lines │ Total tokens │ Clones found │ Duplicated lines │ Duplicated tokens │
├────────┼────────────────┼─────────────┼──────────────┼──────────────┼──────────────────┼───────────────────┤
│ bash   │ 7              │ 1522        │ 5580         │ 0            │ 0 (0.00%)        │ 0 (0.00%)         │
├────────┼────────────────┼─────────────┼──────────────┼──────────────┼──────────────────┼───────────────────┤
│ go     │ 6              │ 3694        │ 40869        │ 12           │ 84 (2.27%)       │ 1569 (3.84%)      │
├────────┼────────────────┼─────────────┼──────────────┼──────────────┼──────────────────┼───────────────────┤
│ python │ 5              │ 2551        │ 14243        │ 5            │ 38 (1.49%)       │ 310 (2.18%)       │
├────────┼────────────────┼─────────────┼──────────────┼──────────────┼──────────────────┼───────────────────┤
│ Total: │ 18             │ 7767        │ 60692        │ 17           │ 122 (1.57%)      │ 1879 (3.10%)      │
└────────┴────────────────┴─────────────┴──────────────┴──────────────┴──────────────────┴───────────────────┘
Found 17 clones.
HTML report saved to megalinter-reports/copy-paste/jscpd-report.html
ERROR: jscpd found too many duplicates (1.6%) over threshold (0.0%)
time: 433.664ms
```

</details>

<details>
<summary>⚠️ MARKDOWN / markdownlint - 62 errors</summary>

```
.claude/skills/maintain/SKILL.md:6 error MD041/first-line-heading/first-line-h1 First line in a file should be a top-level heading [Context: "Perform maintenance per the **..."]
AGENTS.md:15:401 error MD013/line-length Line length [Expected: 400; Actual: 483]
AGENTS.md:24 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
AGENTS.md:101:401 error MD013/line-length Line length [Expected: 400; Actual: 1769]
AGENTS.md:105:401 error MD013/line-length Line length [Expected: 400; Actual: 1126]
AGENTS.md:106:401 error MD013/line-length Line length [Expected: 400; Actual: 1447]
AGENTS.md:121:401 error MD013/line-length Line length [Expected: 400; Actual: 649]
AGENTS.md:123:401 error MD013/line-length Line length [Expected: 400; Actual: 971]
AGENTS.md:150:401 error MD013/line-length Line length [Expected: 400; Actual: 970]
AGENTS.md:241:401 error MD013/line-length Line length [Expected: 400; Actual: 491]
AGENTS.md:242:401 error MD013/line-length Line length [Expected: 400; Actual: 468]
AGENTS.md:248:401 error MD013/line-length Line length [Expected: 400; Actual: 532]
AGENTS.md:250:401 error MD013/line-length Line length [Expected: 400; Actual: 523]
AGENTS.md:253:401 error MD013/line-length Line length [Expected: 400; Actual: 613]
AGENTS.md:254:401 error MD013/line-length Line length [Expected: 400; Actual: 714]
AGENTS.md:258:401 error MD013/line-length Line length [Expected: 400; Actual: 502]
AGENTS.md:262:401 error MD013/line-length Line length [Expected: 400; Actual: 441]
AGENTS.md:267:401 error MD013/line-length Line length [Expected: 400; Actual: 427]
AGENTS.md:370:401 error MD013/line-length Line length [Expected: 400; Actual: 1012]
AGENTS.md:372:401 error MD013/line-length Line length [Expected: 400; Actual: 1240]
AGENTS.md:377:401 error MD013/line-length Line length [Expected: 400; Actual: 430]
AGENTS.md:388:401 error MD013/line-length Line length [Expected: 400; Actual: 1230]
AGENTS.md:399:401 error MD013/line-length Line length [Expected: 400; Actual: 790]
AGENTS.md:404:401 error MD013/line-length Line length [Expected: 400; Actual: 515]
CLAUDE.md:1 error MD041/first-line-heading/first-line-h1 First line in a file should be a top-level heading [Context: "@AGENTS.md"]
docs/chaos-engineering.md:136 error MD025/single-title/single-h1 Multiple top-level headings in the same document [Context: "1820, #2024) are best rehearse..."]
docs/dr/alerting.md:213:28 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/crypto-custody.md:22:389 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/crypto-custody.md:23:264 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/crypto-custody.md:27:35 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/crypto-custody.md:27:161 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/crypto-custody.md:27:239 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/crypto-custody.md:114 error MD024/no-duplicate-heading Multiple headings with the same content [Context: "Custody recommendations"]
docs/dr/crypto-custody.md:245 error MD024/no-duplicate-heading Multiple headings with the same content [Context: "Custody recommendations"]
docs/dr/crypto-custody.md:251 error MD024/no-duplicate-heading Multiple headings with the same content [Context: "What to do if it leaks"]
docs/dr/crypto-custody.md:258 error MD024/no-duplicate-heading Multiple headings with the same content [Context: "What to do if it is *lost* (no..."]
docs/dr/restore-drill.md:42 error MD028/no-blanks-blockquote Blank line inside blockquote
docs/dr/runbook.md:23:102 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/runbook.md:23:487 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/runbook.md:34 error MD028/no-blanks-blockquote Blank line inside blockquote
docs/dr/runbook.md:41 error MD028/no-blanks-blockquote Blank line inside blockquote
docs/dr/runbook.md:50 error MD028/no-blanks-blockquote Blank line inside blockquote
docs/dr/runbook.md:474:92 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/spire-server-ha.md:93 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/dr/velero-cnpg.md:11 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/dr/velero-cnpg.md:56:78 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/velero-cnpg.md:56:166 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/velero-cnpg.md:57:78 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/velero-cnpg.md:57:227 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/velero-cnpg.md:58:78 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/dr/velero-cnpg.md:58:166 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
docs/github-management.md:38:401 error MD013/line-length Line length [Expected: 400; Actual: 419]
docs/github-management.md:40:401 error MD013/line-length Line length [Expected: 400; Actual: 522]
docs/node-autoscaling.md:14 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/oidc-kubectl.md:95 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/runtime-security.md:114 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/rwx-storage.md:9 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/unifi-management.md:14 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
docs/unifi-management.md:62 error MD040/fenced-code-language Fenced code blocks should have a language specified [Context: "```"]
README.md:116:401 error MD013/line-length Line length [Expected: 400; Actual: 540]
README.md:237:32 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
README.md:237:36 error MD060/table-column-style Table column style [Table pipe does not align with header for style "aligned"]
```

</details>

<details>
<summary>⚠️ REPOSITORY / trivy - 1 error</summary>

```
───────────────────────────────────────
  10 ┌ spec:
  11 │   sources:
  12 │     - useDefaultCAs: true
  13 │     - inLine: |
  14 │         -----BEGIN CERTIFICATE-----
  15 │         MIICiTCCAi6gAwIBAgIUXZP3MWb8MKwBE1Qbawsp1sfA/Y4wCgYIKoZIzj0EAwIw
  16 │         gY8xCzAJBgNVBAYTAlVTMRMwEQYDVQQIEwpDYWxpZm9ybmlhMRYwFAYDVQQHEw1T
  17 │         YW4gRnJhbmNpc2NvMRkwFwYDVQQKExBDbG91ZEZsYXJlLCBJbmMuMTgwNgYDVQQL
  18 └         Ey9DbG91ZEZsYXJlIE9yaWdpbiBTU0wgRUNDIENlcnRpZmljYXRlIEF1dGhvcml0
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/cluster-issuers/cloudflare-origin-issuer.yaml (kubernetes)
===============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/cluster-issuers/cloudflare-origin-issuer.yaml:5-10
────────────────────────────────────────
   5 ┌ spec:
   6 │   requestType: OriginECC
   7 │   auth:
   8 │     tokenRef:
   9 │       name: cloudflare-api-token
  10 └       key: api-token
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/cluster-issuers/letsencrypt-prod-issuer.yaml (kubernetes)
==============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/cluster-issuers/letsencrypt-prod-issuer.yaml:5-23
────────────────────────────────────────
   5 ┌ spec:
   6 │   acme:
   7 │     server: https://acme-v02.api.letsencrypt.org/directory
   8 │     email: ${admin_email}
   9 │     privateKeySecretRef:
  10 │       name: letsencrypt-prod-account-key
  11 │     solvers:
  12 │       # Cloudflare hosts ${domain}. This shared ClusterIssuer only carries the
  13 └       # Cloudflare solver; tenant-owned zones DNS-hosted at simply.com (e.g.
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/cluster-policies/drain-autoscale-node-storage.yaml (kubernetes)
====================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/cluster-policies/drain-autoscale-node-storage.yaml:37-71
────────────────────────────────────────
  37 ┌ spec:
  38 │   mutateExistingOnPolicyUpdate: true
  39 │   rules:
  40 │     # The trigger match must NOT carry an `operations:` filter: Kyverno's
  41 │     # policy controller enumerates EXISTING trigger resources through the same
  42 │     # match when it generates the mutateExistingOnPolicyUpdate retrofit URs
  43 │     # (and again on its hourly resync), and an `operations: [CREATE]` filter
  44 │     # excludes every already-existing resource from that enumeration — the
  45 └     # retrofit then never fires at all (observed live 2026-07-02: 28h after
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/cluster-policies/prefer-baseline-nodes.yaml (kubernetes)
=============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/cluster-policies/prefer-baseline-nodes.yaml:53-97
────────────────────────────────────────
  53 ┌ spec:
  54 │   rules:
  55 │     - name: prefer-baseline-nodes
  56 │       match:
  57 │         any:
  58 │           - resources:
  59 │               kinds:
  60 │                 - Pod
  61 └               # CREATE only. A node affinity is consulted solely by the scheduler
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/cluster-policies/restrict-storage-to-baseline-workers.yaml (kubernetes)
============================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/cluster-policies/restrict-storage-to-baseline-workers.yaml:68-84
────────────────────────────────────────
  68 ┌ spec:
  69 │   rules:
  70 │     # Admission path: every future autoscale Node CR is patched as it is
  71 │     # created (or on any later spec update that slips through).
  72 │     - name: disable-replica-scheduling-on-autoscale-nodes
  73 │       match:
  74 │         any:
  75 │           - resources:
  76 └               kinds:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/alertmanager/cilium-network-policy.yaml (kubernetes)
=====================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/alertmanager/cilium-network-policy.yaml:14-57
────────────────────────────────────────
  14 ┌ spec:
  15 │   endpointSelector:
  16 │     matchLabels:
  17 │       app.kubernetes.io/name: alertmanager
  18 │   ingress:
  19 │     # The Headlamp Kubescape plugin reads GET /api/v2/alerts through the
  20 │     # Kubernetes API server's Service proxy (/api/v1/namespaces/kubescape/
  21 │     # services/alertmanager:9093/proxy/...), so the connection to :9093
  22 └     # originates from the API server. Mirror the entity set the base
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/alertmanager/helm-release.yaml (kubernetes)
============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/alertmanager/helm-release.yaml:8-118
────────────────────────────────────────
   8 ┌ spec:
   9 │   # WHY THIS EXISTS (prod-only). The platform's general alerting was migrated off
  10 │   # the kube-prometheus-stack onto Coroot (docs/dr/alerting.md), so there is no
  11 │   # Alertmanager in the cluster. But the Kubescape node-agent's runtime-detection
  12 │   # alerts have exactly one first-class dashboard — the Headlamp Kubescape
  13 │   # plugin's "Runtime Detection > Alerts" tab — and that tab reads ONLY from a
  14 │   # Prometheus Alertmanager `GET /api/v2/alerts` (filtered on
  15 │   # alertname="KubescapeRuleViolated"); it cannot read Kubescape storage CRs and
  16 └   # cannot query Coroot's Prometheus (a metrics store, not an Alertmanager). So a
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/alertmanager/helm-repository.yaml (kubernetes)
===============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/alertmanager/helm-repository.yaml:7-9
────────────────────────────────────────
   7 ┌ spec:
   8 │   interval: 1h
   9 └   url: https://prometheus-community.github.io/helm-charts
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/cilium/cilium-clusterwide-network-policy.yaml (kubernetes)
===========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/cilium/cilium-clusterwide-network-policy.yaml:42-80
────────────────────────────────────────
  42 ┌ spec:
  43 │   # Every pod is a potential destination EXCEPT CoreDNS. DNS must never
  44 │   # sit behind the mutual-auth handshake. CoreDNS is a regular (non
  45 │   # host-network) pod, so without this carve-out the blanket ingress rule
  46 │   # below makes every pod -> CoreDNS query require mutual auth. A TCP flow
  47 │   # survives that (the first packet is dropped, the auth handshake fires,
  48 │   # and the kernel's retransmit rides the now-established auth), but DNS
  49 │   # over UDP has no transport-layer retransmit: Cilium drops every datagram
  50 └   # until auth is established, and the resolver just times out. Applying
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/cilium/patches/enforce-wireguard-strict-mode.yaml (kubernetes)
===============================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRelease 'cilium' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/cilium/patches/enforce-wireguard-strict-mode.yaml:59-70
────────────────────────────────────────
  59 ┌ spec:
  60 │   values:
  61 │     encryption:
  62 │       strictMode:
  63 │         egress:
  64 │           enabled: true
  65 │           cidr: 10.244.0.0/16
  66 │           # Required in tunnel routing mode (see header): exempts node-identity
  67 └           # traffic from the strict drop so the SPIRE mutual-auth handshake and
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/cilium/patches/enforce-wireguard-strict-mode.yaml:59-70
────────────────────────────────────────
  59 ┌ spec:
  60 │   values:
  61 │     encryption:
  62 │       strictMode:
  63 │         egress:
  64 │           enabled: true
  65 │           cidr: 10.244.0.0/16
  66 │           # Required in tunnel routing mode (see header): exempts node-identity
  67 └           # traffic from the strict drop so the SPIRE mutual-auth handshake and
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/cilium/patches/store-spire-data-on-hcloud.yaml (kubernetes)
============================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRelease 'cilium' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/cilium/patches/store-spire-data-on-hcloud.yaml:53-62
────────────────────────────────────────
  53 ┌ spec:
  54 │   values:
  55 │     authentication:
  56 │       mutual:
  57 │         spire:
  58 │           install:
  59 │             server:
  60 │               dataStorage:
  61 │                 storageClass: hcloud
  62 └                 size: 10Gi
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/cilium/patches/store-spire-data-on-hcloud.yaml:53-62
────────────────────────────────────────
  53 ┌ spec:
  54 │   values:
  55 │     authentication:
  56 │       mutual:
  57 │         spire:
  58 │           install:
  59 │             server:
  60 │               dataStorage:
  61 │                 storageClass: hcloud
  62 └                 size: 10Gi
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/coredns/pod-disruption-budget.yaml (kubernetes)
================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): PodDisruptionBudget 'coredns' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/coredns/pod-disruption-budget.yaml:12-19
────────────────────────────────────────
  12 ┌ spec:
  13 │   # maxUnavailable: 1 is the platform-wide drain-safe PDB pattern (issue #1880):
  14 │   # never deadlocks a drain regardless of replica count, and bounds disruption to
  15 │   # one pod at a time (behaviourally identical to minAvailable: 1 at 2 replicas).
  16 │   maxUnavailable: 1
  17 │   selector:
  18 │     matchLabels:
  19 └       k8s-app: kube-dns
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/coredns/pod-disruption-budget.yaml:12-19
────────────────────────────────────────
  12 ┌ spec:
  13 │   # maxUnavailable: 1 is the platform-wide drain-safe PDB pattern (issue #1880):
  14 │   # never deadlocks a drain regardless of replica count, and bounds disruption to
  15 │   # one pod at a time (behaviourally identical to minAvailable: 1 at 2 replicas).
  16 │   maxUnavailable: 1
  17 │   selector:
  18 │     matchLabels:
  19 └       k8s-app: kube-dns
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/crossplane/cilium-network-policy.yaml (kubernetes)
===================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/crossplane/cilium-network-policy.yaml:6-72
────────────────────────────────────────
   6 ┌ spec:
   7 │   # Namespace-wide: covers the Crossplane core pod (package manager pulls
   8 │   # provider OCI packages itself, in-pod — unlike runtime images, which the
   9 │   # kubelet pulls at node level) AND the provider pods Crossplane creates here
  10 │   # (provider-upjet-github talks to the GitHub API).
  11 │   endpointSelector: {}
  12 │   egress:
  13 │     # Kube API for watching/updating managed resources and leader election.
  14 └     - toEntities:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/crossplane/helm-release.yaml (kubernetes)
==========================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/crossplane/helm-release.yaml:14-50
────────────────────────────────────────
  14 ┌ spec:
  15 │   interval: 10m
  16 │   timeout: 10m
  17 │   install:
  18 │     crds: CreateReplace
  19 │   upgrade:
  20 │     crds: CreateReplace
  21 │   chart:
  22 └     spec:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/crossplane/helm-repository.yaml (kubernetes)
=============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/crossplane/helm-repository.yaml:6-7
────────────────────────────────────────
   6 ┌ spec:
   7 └   url: https://charts.crossplane.io/stable
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-release.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRelease 'descheduler' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-release.yaml:9-92
────────────────────────────────────────
   9 ┌ spec:
  10 │   interval: 10m
  11 │   timeout: 10m
  12 │   chart:
  13 │     spec:
  14 │       chart: descheduler
  15 │       # appVersion 0.36.0. Renovate tracks the chart version.
  16 │       version: 0.36.0
  17 └       sourceRef:
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-release.yaml:9-92
────────────────────────────────────────
   9 ┌ spec:
  10 │   interval: 10m
  11 │   timeout: 10m
  12 │   chart:
  13 │     spec:
  14 │       chart: descheduler
  15 │       # appVersion 0.36.0. Renovate tracks the chart version.
  16 │       version: 0.36.0
  17 └       sourceRef:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-repository.yaml (kubernetes)
==============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRepository 'descheduler' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-repository.yaml:7-9
────────────────────────────────────────
   7 ┌ spec:
   8 │   interval: 24h
   9 └   url: https://kubernetes-sigs.github.io/descheduler/
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-repository.yaml:7-9
────────────────────────────────────────
   7 ┌ spec:
   8 │   interval: 24h
   9 └   url: https://kubernetes-sigs.github.io/descheduler/
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml (kubernetes)
==============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml:13-142
────────────────────────────────────────
  13 ┌   namespace: flux-system
  14 │ spec:
  15 │   distribution:
  16 │     version: "2.8.x"
  17 │     registry: "ghcr.io/fluxcd"
  18 │   components:
  19 │     - source-controller
  20 │     - kustomize-controller
  21 └     - helm-controller
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/helm-release.yaml (kubernetes)
==========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRelease 'hcloud-cloud-controller-manager' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/helm-release.yaml:9-39
────────────────────────────────────────
   9 ┌ spec:
  10 │   chart:
  11 │     spec:
  12 │       chart: hcloud-cloud-controller-manager
  13 │       version: 1.33.0
  14 │       sourceRef:
  15 │         kind: HelmRepository
  16 │         name: hcloud
  17 └   interval: 10m0s
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/helm-release.yaml:9-39
────────────────────────────────────────
   9 ┌ spec:
  10 │   chart:
  11 │     spec:
  12 │       chart: hcloud-cloud-controller-manager
  13 │       version: 1.33.0
  14 │       sourceRef:
  15 │         kind: HelmRepository
  16 │         name: hcloud
  17 └   interval: 10m0s
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/pod-disruption-budget.yaml (kubernetes)
===================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): PodDisruptionBudget 'hcloud-cloud-controller-manager' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/pod-disruption-budget.yaml:6-16
────────────────────────────────────────
   6 ┌ spec:
   7 │   # maxUnavailable: 1 (not minAvailable: 1) keeps the node drainable at the prod
   8 │   # single-replica floor (hcloud_ccm_replicas: "1"). minAvailable: 1 over a
   9 │   # single ready replica permits zero voluntary evictions and makes the hosting
  10 │   # node undrainable; at 2+ replicas maxUnavailable: 1 still bounds disruption to
  11 │   # one pod at a time.
  12 │   maxUnavailable: 1
  13 │   selector:
  14 └     matchLabels:
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/pod-disruption-budget.yaml:6-16
────────────────────────────────────────
   6 ┌ spec:
   7 │   # maxUnavailable: 1 (not minAvailable: 1) keeps the node drainable at the prod
   8 │   # single-replica floor (hcloud_ccm_replicas: "1"). minAvailable: 1 over a
   9 │   # single ready replica permits zero voluntary evictions and makes the hosting
  10 │   # node undrainable; at 2+ replicas maxUnavailable: 1 still bounds disruption to
  11 │   # one pod at a time.
  12 │   maxUnavailable: 1
  13 │   selector:
  14 └     matchLabels:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-release.yaml (kubernetes)
==========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRelease 'hcloud-csi' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-release.yaml:10-81
────────────────────────────────────────
  10 ┌ spec:
  11 │   chart:
  12 │     spec:
  13 │       chart: hcloud-csi
  14 │       version: 2.22.0
  15 │       sourceRef:
  16 │         kind: HelmRepository
  17 │         name: hcloud
  18 └   interval: 10m0s
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-release.yaml:10-81
────────────────────────────────────────
  10 ┌ spec:
  11 │   chart:
  12 │     spec:
  13 │       chart: hcloud-csi
  14 │       version: 2.22.0
  15 │       sourceRef:
  16 │         kind: HelmRepository
  17 │         name: hcloud
  18 └   interval: 10m0s
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-repository.yaml (kubernetes)
=============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRepository 'hcloud' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-repository.yaml:7-8
────────────────────────────────────────
   7 ┌ spec:
   8 └   url: https://charts.hetzner.cloud
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-repository.yaml:7-8
────────────────────────────────────────
   7 ┌ spec:
   8 └   url: https://charts.hetzner.cloud
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/ksail-operator/patches/enable-oidc.yaml (kubernetes)
=====================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/ksail-operator/patches/enable-oidc.yaml:18-27
────────────────────────────────────────
  18 ┌ spec:
  19 │   values:
  20 │     auth:
  21 │       oidc:
  22 │         enabled: true
  23 │         issuerURL: https://dex.${domain}
  24 │         clientID: public-client
  25 │         clientSecret: ${dex_client_secret}
  26 │         redirectURL: https://ksail.${domain}/api/v1/auth/callback
  27 └         scopes: "openid email profile groups"
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/kubelet-serving-cert-approver/cilium-network-policy.yaml (kubernetes)
======================================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/kubelet-serving-cert-approver/cilium-network-policy.yaml:6-22
────────────────────────────────────────
   6 ┌ spec:
   7 │   endpointSelector: {}
   8 │   egress:
   9 │     # Kube API for approving CSRs
  10 │     - toEntities:
  11 │         - kube-apiserver
  12 │     # DNS resolution
  13 │     - toEndpoints:
  14 └         - matchLabels:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/kubelet-serving-cert-approver/pod-disruption-budget.yaml (kubernetes)
======================================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/kubelet-serving-cert-approver/pod-disruption-budget.yaml:6-18
────────────────────────────────────────
   6 ┌ spec:
   7 │   # Drain-safe PDB pattern (#1880/#1882): maxUnavailable: 1 keeps the node
   8 │   # drainable at any replica count, unlike minAvailable: 1 which deadlocks a
   9 │   # drain when only 1 replica is ready. The upstream ha-install.yaml ships no
  10 │   # PDB, so it is declared here. Pairs with the HA base (replicas: 2 +
  11 │   # --enable-leader-election in kustomization.yaml): the approver is then
  12 │   # leader-elected, so the second replica is a warm standby that keeps kubelet
  13 │   # serving-cert CSR approval available while a node drains.
  14 └   maxUnavailable: 1
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/kubescape/patches/route-runtime-detection-alerts.yaml (kubernetes)
===================================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/kubescape/patches/route-runtime-detection-alerts.yaml:6-33
────────────────────────────────────────
   6 ┌ spec:
   7 │   # Matches the base interval so this patch is a no-op on that field once merged.
   8 │   interval: 10m
   9 │   values:
  10 │     nodeAgent:
  11 │       config:
  12 │         # Fan the runtime-detection alerts out to their three destinations
  13 │         # (prod-only — runtimeDetection is disabled on the docker/local overlay).
  14 └         #
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/longhorn/cilium-network-policy.yaml (kubernetes)
=================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/cilium-network-policy.yaml:6-74
────────────────────────────────────────
   6 ┌ spec:
   7 │   endpointSelector: {}
   8 │   ingress:
   9 │     # Webhook from kube-apiserver (hostNetwork on control plane nodes)
  10 │     - fromEntities:
  11 │         - kube-apiserver
  12 │         - remote-node
  13 │         - host
  14 └       toPorts:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/longhorn/cron-job-stale-node-cleanup.yaml (kubernetes)
=======================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/cron-job-stale-node-cleanup.yaml:35-188
────────────────────────────────────────
  35 ┌ spec:
  36 │   # Daily, off the top of the hour. Stale CRs are pure cosmetic/bookkeeping
  37 │   # debt, never urgent, so one reconcile a day is ample.
  38 │   schedule: "17 3 * * *"
  39 │   # Never overlap; a slow run skips the next tick and resumes after.
  40 │   concurrencyPolicy: Forbid
  41 │   startingDeadlineSeconds: 200
  42 │   successfulJobsHistoryLimit: 1
  43 └   failedJobsHistoryLimit: 3
  ..   
────────────────────────────────────────


KSV-0125 (MEDIUM): Container cleanup in cronjob longhorn-stale-node-cleanup (namespace: longhorn-system) uses an image from an untrusted registry.
════════════════════════════════════════
Ensure that all containers use images only from trusted registry domains.

See https://avd.aquasec.com/misconfig/ksv-0125
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/cron-job-stale-node-cleanup.yaml:71-110
────────────────────────────────────────
  71 ┌             - name: cleanup
  72 │               # NOT registry.k8s.io/kubectl: that image is distroless (kubectl
  73 │               # binary only, no /bin/sh), so the shell script below could never
  74 │               # start — every run since the CronJob shipped failed with
  75 │               # StartError exit 128 "stat /bin/sh: no such file or directory"
  76 │               # (observed live 2026-07-02). alpine/k8s ships kubectl + a POSIX
  77 │               # shell; the tag tracks the kubectl minor, matching the cluster.
  78 │               image: docker.io/alpine/k8s:1.36.2@sha256:44ef4942e171939b9c665a4a84beb80e2dcdb9a24330d4651cfdfd2e9deecc47
  79 └               securityContext:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml (kubernetes)
========================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml:10-131
────────────────────────────────────────
  10 ┌ spec:
  11 │   # The Longhorn CSI snapshotter sidecar (enabled via
  12 │   # longhorn_csi_snapshotter_replicas) needs the snapshot.storage.k8s.io CRDs to
  13 │   # exist or it crash-loops. Gate Longhorn on the snapshot-controller (which
  14 │   # installs those CRDs) so the sidecar never starts before they are present.
  15 │   dependsOn:
  16 │     - name: snapshot-controller
  17 │       namespace: kube-system
  18 └   chart:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-repository.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-repository.yaml:7-8
────────────────────────────────────────
   7 ┌ spec:
   8 └   url: https://charts.longhorn.io
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/longhorn/http-route.yaml (kubernetes)
======================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/http-route.yaml:21-43
────────────────────────────────────────
  21 ┌     gethomepage.dev/icon: longhorn.png
  22 │ spec:
  23 │   parentRefs:
  24 │     - name: platform
  25 │       namespace: kube-system
  26 │       sectionName: https
  27 │   hostnames:
  28 │     - longhorn.${domain}
  29 └   rules:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/longhorn/limit-range.yaml (kubernetes)
=======================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/longhorn/limit-range.yaml:32-39
────────────────────────────────────────
  32 ┌ spec:
  33 │   limits:
  34 │     - type: Container
  35 │       default:
  36 │         cpu: "2"
  37 │       defaultRequest:
  38 │         cpu: 15m
  39 └         memory: 128Mi
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/openbao/patches/store-data-on-hcloud.yaml (kubernetes)
=======================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/openbao/patches/store-data-on-hcloud.yaml:6-14
────────────────────────────────────────
   6 ┌ spec:
   7 │   values:
   8 │     server:
   9 │       # Hetzner rootfs (~36 GiB) is too small for Longhorn to host 10 GiB
  10 │       # OpenBao file storage. Use hcloud-csi block storage.
  11 │       dataStorage:
  12 │         storageClass: hcloud
  13 │       auditStorage:
  14 └         storageClass: hcloud
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/helm-release.yaml (kubernetes)
================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/helm-release.yaml:6-29
────────────────────────────────────────
   6 ┌ spec:
   7 │   dependsOn:
   8 │     - name: cert-manager
   9 │   interval: 10m
  10 │   timeout: 10m
  11 │   chart:
  12 │     spec:
  13 │       chart: origin-ca-issuer
  14 └       version: 0.6.4
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/helm-repository.yaml (kubernetes)
===================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/helm-repository.yaml:6-8
────────────────────────────────────────
   6 ┌ spec:
   7 │   url: oci://ghcr.io/cloudflare/origin-ca-issuer-charts
   8 └   type: oci
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/pod-disruption-budget.yaml (kubernetes)
=========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/pod-disruption-budget.yaml:6-16
────────────────────────────────────────
   6 ┌ spec:
   7 │   # maxUnavailable: 1 (not minAvailable: 1) keeps the node drainable when the
   8 │   # workload runs 0/1 replicas — origin_ca_issuer_replicas is "0" in prod (the
   9 │   # service is disabled). minAvailable: 1 over <2 ready replicas permits zero
  10 │   # voluntary evictions and makes every hosting node undrainable; at 2+ replicas
  11 │   # maxUnavailable: 1 still bounds disruption to one pod at a time.
  12 │   maxUnavailable: 1
  13 │   selector:
  14 └     matchLabels:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/cilium-network-policy.yaml (kubernetes)
===========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/cilium-network-policy.yaml:6-40
────────────────────────────────────────
   6 ┌ spec:
   7 │   # The solver is an aggregated API server: cert-manager reaches it *through*
   8 │   # the kube-apiserver, so ingress arrives from the apiserver/host entities on
   9 │   # the solver's HTTPS port (443) — the namespace-wide allow-cert-manager
  10 │   # policy only admits 10250/6443 for cert-manager's own webhook.
  11 │   endpointSelector:
  12 │     matchLabels:
  13 │       app: simply-dns-webhook
  14 └       release: simply-dns-webhook
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/helm-release.yaml (kubernetes)
==================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/helm-release.yaml:8-49
────────────────────────────────────────
   8 ┌ spec:
   9 │   interval: 2m
  10 │   timeout: 10m
  11 │   chart:
  12 │     spec:
  13 │       chart: simply-dns-webhook
  14 │       # 1.9.0 is the newest *published* chart: v1.10.0 is tagged upstream but
  15 │       # its Pages index was never redeployed, so the 1.10.0 tgz 404s. Bump
  16 └       # once it actually resolves.
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/helm-repository.yaml (kubernetes)
=====================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/helm-repository.yaml:6-7
────────────────────────────────────────
   6 ┌ spec:
   7 └   url: https://runnerm.github.io/simply-dns-webhook/
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/pod-disruption-budget.yaml (kubernetes)
===========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/simply-dns-webhook/pod-disruption-budget.yaml:6-20
────────────────────────────────────────
   6 ┌ spec:
   7 │   # Drain-safe PDB pattern (#1880/#1882): maxUnavailable: 1 keeps the node
   8 │   # drainable at any replica count, unlike minAvailable: 1 which deadlocks a
   9 │   # drain when only 1 replica is ready. The chart ships no PDB knob, so this is
  10 │   # a standalone manifest (same approach as origin-ca-issuer). Pairs with
  11 │   # replicaCount: 2 in helm-release.yaml: the second replica is a warm standby
  12 │   # that keeps DNS01 challenge validation available while a node drains.
  13 │   maxUnavailable: 1
  14 └   selector:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-release.yaml (kubernetes)
===================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRelease 'snapshot-controller' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-release.yaml:9-69
────────────────────────────────────────
   9 ┌ spec:
  10 │   interval: 10m
  11 │   timeout: 10m
  12 │   chart:
  13 │     spec:
  14 │       chart: snapshot-controller
  15 │       # appVersion v8.5.0 — the external-snapshotter version Longhorn 1.11
  16 │       # targets. Installs the snapshot.storage.k8s.io CRDs + the cluster
  17 └       # snapshot-controller. Renovate tracks the chart version.
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-release.yaml:9-69
────────────────────────────────────────
   9 ┌ spec:
  10 │   interval: 10m
  11 │   timeout: 10m
  12 │   chart:
  13 │     spec:
  14 │       chart: snapshot-controller
  15 │       # appVersion v8.5.0 — the external-snapshotter version Longhorn 1.11
  16 │       # targets. Installs the snapshot.storage.k8s.io CRDs + the cluster
  17 └       # snapshot-controller. Renovate tracks the chart version.
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-repository.yaml (kubernetes)
======================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): HelmRepository 'piraeus' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-repository.yaml:7-9
────────────────────────────────────────
   7 ┌ spec:
   8 │   interval: 24h
   9 └   url: https://piraeus.io/helm-charts/
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-repository.yaml:7-9
────────────────────────────────────────
   7 ┌ spec:
   8 │   interval: 24h
   9 └   url: https://piraeus.io/helm-charts/
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/pod-disruption-budget.yaml (kubernetes)
============================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): PodDisruptionBudget 'snapshot-controller' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/pod-disruption-budget.yaml:6-16
────────────────────────────────────────
   6 ┌ spec:
   7 │   # maxUnavailable: 1 is the platform-wide drain-safe PDB pattern (issue #1880).
   8 │   # The piraeus chart exposes no PDB value, so the PDB is declared here. The
   9 │   # snapshot-controller is leader-elected (snapshot_controller_replicas: 3 in
  10 │   # prod); keeping a leader available means Velero's nightly CSI snapshot
  11 │   # backups don't hang if a drain lands mid-backup.
  12 │   maxUnavailable: 1
  13 │   selector:
  14 └     matchLabels:
  ..   
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/pod-disruption-budget.yaml:6-16
────────────────────────────────────────
   6 ┌ spec:
   7 │   # maxUnavailable: 1 is the platform-wide drain-safe PDB pattern (issue #1880).
   8 │   # The piraeus chart exposes no PDB value, so the PDB is declared here. The
   9 │   # snapshot-controller is leader-elected (snapshot_controller_replicas: 3 in
  10 │   # prod); keeping a leader available means Velero's nightly CSI snapshot
  11 │   # backups don't hang if a drain lands mid-backup.
  12 │   maxUnavailable: 1
  13 │   selector:
  14 └     matchLabels:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/tofu-controller/helm-release.yaml (kubernetes)
===============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/tofu-controller/helm-release.yaml:8-41
────────────────────────────────────────
   8 ┌ spec:
   9 │   interval: 10m
  10 │   timeout: 10m
  11 │   releaseName: tofu-controller
  12 │   chart:
  13 │     spec:
  14 │       chart: tofu-controller
  15 │       version: 0.16.4
  16 └       sourceRef:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/tofu-controller/helm-repository.yaml (kubernetes)
==================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/tofu-controller/helm-repository.yaml:6-8
────────────────────────────────────────
   6 ┌ spec:
   7 │   interval: 10m
   8 └   url: https://flux-iac.github.io/tofu-controller
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/controllers/velero/patches/enable-csi-snapshots.yaml (kubernetes)
======================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/controllers/velero/patches/enable-csi-snapshots.yaml:6-29
────────────────────────────────────────
   6 ┌ spec:
   7 │   values:
   8 │     configuration:
   9 │       # Enable the CSI snapshot path. Velero 1.18 ships the CSI plugin in core,
  10 │       # so this feature flag is all that's needed (the velero-plugin-for-aws
  11 │       # initContainer for the R2 BackupStorageLocation stays).
  12 │       #
  13 │       # defaultVolumesToFsBackup is intentionally LEFT at the base default
  14 └       # (true). It is the fail-safe fallback for every volume the volume policy
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/cilium-network-policy.yaml (kubernetes)
===================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/cilium-network-policy.yaml:22-65
────────────────────────────────────────
  22 ┌ spec:
  23 │   endpointSelector:
  24 │     matchLabels:
  25 │       cnpg.io/cluster: coroot-db
  26 │   ingress:
  27 │     # CNPG operator reaches the instance status (8000) and PostgreSQL (5432).
  28 │     - fromEndpoints:
  29 │         - matchLabels:
  30 └             k8s:io.kubernetes.pod.namespace: cnpg-system
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/cluster.yaml (kubernetes)
=====================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/cluster.yaml:44-160
────────────────────────────────────────
  44 ┌ spec:
  45 │   # Pin the PostgreSQL operand image to clear fixable base-OS + PostgreSQL CVEs in
  46 │   # the CNPG operator default (18.3-system-trixie) — same rationale and Renovate
  47 │   # wiring as umami-db. CNPG rolls the image in place across the HA instances.
  48 │   # renovate: datasource=docker depName=ghcr.io/cloudnative-pg/postgresql
  49 │   imageName: ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie
  50 │   # Coroot Postgres self-integration: stamp the coroot.com/postgres-scrape
  51 │   # annotations onto the DB instance Pods (CNPG inheritedMetadata propagates to
  52 └   # all managed objects, incl. Pods) so Coroot's own cluster-agent discovers and
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/cron-job-alert-autosuppressor.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/cron-job-alert-autosuppressor.yaml:65-116
────────────────────────────────────────
  65 ┌ spec:
  66 │   # Every 15m: an exempted alert still fires its first notification when it opens,
  67 │   # then this suppresses it within one interval so it stops re-notifying. The
  68 │   # schedule IS the retry loop (backoffLimit 0 + restartPolicy Never).
  69 │   schedule: "*/15 * * * *"
  70 │   concurrencyPolicy: Forbid
  71 │   startingDeadlineSeconds: 60
  72 │   successfulJobsHistoryLimit: 1
  73 └   failedJobsHistoryLimit: 3
  ..   
────────────────────────────────────────


KSV-0125 (MEDIUM): Container autosuppressor in cronjob coroot-alert-autosuppressor (namespace: observability) uses an image from an untrusted registry.
════════════════════════════════════════
Ensure that all containers use images only from trusted registry domains.

See https://avd.aquasec.com/misconfig/ksv-0125
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/cron-job-alert-autosuppressor.yaml:93-116
────────────────────────────────────────
  93 ┌             - name: autosuppressor
  94 │               # curl + jq, digest-pinned (same image as custom-cloud-pricing).
  95 │               # observability is exempt from disallow-latest-tag.
  96 │               image: docker.io/badouralix/curl-jq:latest@sha256:1e7c0284e24572ace7170df9fc91f15fd3b79ebf056d4dde17244d5d74bbfabc
  97 │               securityContext:
  98 │                 allowPrivilegeEscalation: false
  99 │                 readOnlyRootFilesystem: true
 100 │                 runAsNonRoot: true
 101 └                 runAsUser: 65532
 ...   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/cron-job-custom-cloud-pricing.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/cron-job-custom-cloud-pricing.yaml:49-114
────────────────────────────────────────
  49 ┌ spec:
  50 │   # Static config — re-asserted on a slow cadence purely to self-heal after a DR
  51 │   # restore; a no-op tick is cheap. The schedule IS the retry loop (backoffLimit
  52 │   # 0 + restartPolicy Never), mirroring umami-provision-tenants.
  53 │   schedule: "*/30 * * * *"
  54 │   concurrencyPolicy: Forbid
  55 │   startingDeadlineSeconds: 200
  56 │   successfulJobsHistoryLimit: 1
  57 └   failedJobsHistoryLimit: 3
  ..   
────────────────────────────────────────


KSV-0125 (MEDIUM): Container set-pricing in cronjob coroot-custom-cloud-pricing (namespace: observability) uses an image from an untrusted registry.
════════════════════════════════════════
Ensure that all containers use images only from trusted registry domains.

See https://avd.aquasec.com/misconfig/ksv-0125
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/cron-job-custom-cloud-pricing.yaml:77-114
────────────────────────────────────────
  77 ┌             - name: set-pricing
  78 │               # curl + jq, pinned by digest. jq replaces the former grep/sed/awk
  79 │               # JSON parsing + awk float compare with a robust, structure-tolerant
  80 │               # parse (the heartbeat CronJob still uses curlimages/curl — it has no
  81 │               # JSON to parse). No official curl+jq image exists, so this is the
  82 │               # de-facto community one, digest-pinned; observability is exempt from
  83 │               # disallow-latest-tag. Swappable for any curl+jq image.
  84 │               image: docker.io/badouralix/curl-jq:latest@sha256:1e7c0284e24572ace7170df9fc91f15fd3b79ebf056d4dde17244d5d74bbfabc
  85 └               securityContext:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/external-secret.yaml (kubernetes)
=============================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/external-secret.yaml:12-37
────────────────────────────────────────
  12 ┌ spec:
  13 │   refreshInterval: 1h
  14 │   secretStoreRef:
  15 │     name: openbao
  16 │     kind: ClusterSecretStore
  17 │   target:
  18 │     name: coroot-db-backup-r2
  19 │     creationPolicy: Owner
  20 └     template:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/object-store.yaml (kubernetes)
==========================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/object-store.yaml:15-33
────────────────────────────────────────
  15 ┌ spec:
  16 │   retentionPolicy: 30d
  17 │   configuration:
  18 │     destinationPath: s3://${r2_bucket}/cnpg/coroot-db
  19 │     endpointURL: ${r2_endpoint}
  20 │     s3Credentials:
  21 │       accessKeyId:
  22 │         name: coroot-db-backup-r2
  23 └         key: ACCESS_KEY_ID
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/patches/enable-ha.yaml (kubernetes)
===============================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/patches/enable-ha.yaml:7-215
────────────────────────────────────────
   7 ┌ spec:
   8 │   # --- High availability (prod-only) -----------------------------------------
   9 │   # Run TWO Coroot server pods so the UI / ingestion API rides out a node drain
  10 │   # (Talos roll, autoscaler scale-down) or a single node loss — coroot-coroot was
  11 │   # the last single-replica SPOF in the stack after ClickHouse (2) + Keeper (3)
  12 │   # went HA. The operator (controller/coroot.go) only honours replicas > 1 when
  13 │   # spec.postgres is set — it silently falls back to 1 replica otherwise, because
  14 │   # the default embedded-SQLite store on each pod's PVC can't be shared — so HA
  15 └   # REQUIRES the external Postgres below. The drain-safe maxUnavailable:1 PDB and
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/pod-disruption-budget.yaml (kubernetes)
===================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/pod-disruption-budget.yaml:25-31
────────────────────────────────────────
  25 ┌ spec:
  26 │   maxUnavailable: 1
  27 │   selector:
  28 │     matchLabels:
  29 │       app.kubernetes.io/managed-by: coroot-operator
  30 │       app.kubernetes.io/part-of: coroot
  31 └       app.kubernetes.io/component: coroot
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/coroot/scheduled-backup.yaml (kubernetes)
==============================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/coroot/scheduled-backup.yaml:12-22
────────────────────────────────────────
  12 ┌ spec:
  13 │   # CNPG cron is 6-field (seconds first): 03:30 UTC daily — offset 30m from
  14 │   # umami-db-daily (03:00) so the two base backups don't contend for R2 egress.
  15 │   schedule: "0 30 3 * * *"
  16 │   immediate: true
  17 │   backupOwnerReference: self
  18 │   method: plugin
  19 │   pluginConfiguration:
  20 └     name: barman-cloud.cloudnative-pg.io
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config-aws-iam.yaml (kubernetes)
===================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config-aws-iam.yaml:6-54
────────────────────────────────────────
   6 ┌ spec:
   7 │   deploymentTemplate:
   8 │     spec:
   9 │       selector: {}
  10 │       template:
  11 │         spec:
  12 │           containers:
  13 │             - name: package-runtime
  14 └               # IAM is global, but the provider validates credentials through
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config-family-aws.yaml (kubernetes)
======================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config-family-aws.yaml:8-47
────────────────────────────────────────
   8 ┌ spec:
   9 │   deploymentTemplate:
  10 │     spec:
  11 │       selector: {}
  12 │       template:
  13 │         spec:
  14 │           containers:
  15 │             - name: package-runtime
  16 └               # Kubescape C-0017 (immutable container filesystem) + the hardening
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config-upjet-unifi.yaml (kubernetes)
=======================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config-upjet-unifi.yaml:9-50
────────────────────────────────────────
   9 ┌ spec:
  10 │   deploymentTemplate:
  11 │     spec:
  12 │       selector: {}
  13 │       template:
  14 │         spec:
  15 │           containers:
  16 │             - name: package-runtime
  17 └               # Kubescape C-0017 (immutable container filesystem) + the hardening
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/deployment-runtime-config.yaml:9-43
────────────────────────────────────────
   9 ┌ spec:
  10 │   deploymentTemplate:
  11 │     spec:
  12 │       selector: {}
  13 │       template:
  14 │         spec:
  15 │           containers:
  16 │             - name: package-runtime
  17 └               # Kubescape C-0017 (immutable container filesystem) + the hardening
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy-aws.yaml (kubernetes)
========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy-aws.yaml:13-17
────────────────────────────────────────
  13 ┌   name: aws
  14 │ spec:
  15 │   activate:
  16 │     - openidconnectproviders.iam.aws.m.upbound.io
  17 └     - policies.iam.aws.m.upbound.io
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy-unifi.yaml (kubernetes)
==========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy-unifi.yaml:16-20
────────────────────────────────────────
  16 ┌ spec:
  17 │   activate:
  18 │     - clients.vpn.unifi.m.crossplane.io
  19 │     - trafficroutes.route.unifi.m.crossplane.io
  20 └     - records.dns.unifi.m.crossplane.io
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy.yaml (kubernetes)
====================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy.yaml:17-46
────────────────────────────────────────
  17 ┌ spec:
  18 │   activate:
  19 │     - repositories.repo.github.m.upbound.io
  20 │     - defaultbranches.repo.github.m.upbound.io
  21 │     - branchprotections.repo.github.m.upbound.io
  22 │     - repositoryrulesets.repo.github.m.upbound.io
  23 │     - issuelabels.repo.github.m.upbound.io
  24 │     # Org-level rulesets. Of the org's 20 org rules, the 10 provider-upjet-github
  25 └     # can express are adopted Observe-first in devantler-tech/.github
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/provider-aws-iam.yaml (kubernetes)
==================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/provider-aws-iam.yaml:10-13
────────────────────────────────────────
  10 ┌   name: provider-aws-iam
  11 │ spec:
  12 │   package: xpkg.upbound.io/upbound/provider-aws-iam:v2.6.1
  13 └   runtimeConfigRef:
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/provider-family-aws.yaml (kubernetes)
=====================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/provider-family-aws.yaml:24-27
────────────────────────────────────────
  24 ┌ spec:
  25 │   package: xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
  26 │   runtimeConfigRef:
  27 └     name: provider-family-aws
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/provider-upjet-unifi.yaml (kubernetes)
======================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/provider-upjet-unifi.yaml:15-18
────────────────────────────────────────
  15 ┌ spec:
  16 │   package: ghcr.io/devantler-tech/provider-upjet-unifi:v0.1.0
  17 │   runtimeConfigRef:
  18 └     name: provider-upjet-unifi
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/crossplane/provider.yaml (kubernetes)
==========================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/crossplane/provider.yaml:15-18
────────────────────────────────────────
  15 ┌ spec:
  16 │   package: ghcr.io/crossplane-contrib/provider-upjet-github:v0.19.1
  17 │   runtimeConfigRef:
  18 └     name: provider-upjet-github
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/external-dns/cilium-network-policy.yaml (kubernetes)
=========================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/external-dns/cilium-network-policy.yaml:6-47
────────────────────────────────────────
   6 ┌ spec:
   7 │   endpointSelector: {}
   8 │   egress:
   9 │     # Kube API for watching services/HTTPRoutes
  10 │     - toEntities:
  11 │         - kube-apiserver
  12 │     # External-dns talks to Cloudflare for DNS record management. Pinned
  13 │     # by FQDN rather than world:443 so a compromised external-dns pod
  14 └     # cannot reach arbitrary external services. matchName is exact —
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/external-dns/external-secret.yaml (kubernetes)
===================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/external-dns/external-secret.yaml:15-27
────────────────────────────────────────
  15 ┌ spec:
  16 │   refreshInterval: 1h
  17 │   secretStoreRef:
  18 │     name: openbao
  19 │     kind: ClusterSecretStore
  20 │   target:
  21 │     name: external-dns-cloudflare
  22 │     creationPolicy: Owner
  23 └   data:
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/external-dns/helm-release.yaml (kubernetes)
================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/external-dns/helm-release.yaml:8-79
────────────────────────────────────────
   8 ┌ spec:
   9 │   interval: 10m
  10 │   timeout: 10m
  11 │   chart:
  12 │     spec:
  13 │       chart: external-dns
  14 │       version: 1.21.1
  15 │       sourceRef:
  16 └         kind: HelmRepository
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/external-dns/helm-repository.yaml (kubernetes)
===================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/external-dns/helm-repository.yaml:6-7
────────────────────────────────────────
   6 ┌ spec:
   7 └   url: https://kubernetes-sigs.github.io/external-dns/
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/external-dns/pod-disruption-budget.yaml (kubernetes)
=========================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/external-dns/pod-disruption-budget.yaml:6-20
────────────────────────────────────────
   6 ┌ spec:
   7 │   # maxUnavailable: 1 is the platform-wide drain-safe PDB pattern (issue #1880):
   8 │   # it never deadlocks a node drain regardless of replica count. external-dns is
   9 │   # a DELIBERATELY single-replica writer (no leader election at the pinned app
  10 │   # version v0.21.0 — see helm-release.yaml; a 2nd ACTIVE replica would issue
  11 │   # duplicate Cloudflare writes under one txtOwnerId), so this PDB does not add
  12 │   # standby redundancy. Its job is to let a node drain evict and reschedule the
  13 │   # single pod cleanly (the high priorityClassName makes that reschedule fast)
  14 └   # rather than the drain stalling on an un-budgeted pod. Convert to a true HA
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/flux-notifications/alert.yaml (kubernetes)
===============================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/flux-notifications/alert.yaml:22-29
────────────────────────────────────────
  22 ┌ spec:
  23 │   providerRef:
  24 │     name: slack
  25 │   eventSeverity: error
  26 │   eventSources:
  27 │     - kind: Kustomization
  28 │       name: "*"
  29 └   summary: "Flux reconciliation error — prod platform"
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/flux-notifications/provider.yaml (kubernetes)
==================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/flux-notifications/provider.yaml:9-12
────────────────────────────────────────
   9 ┌ spec:
  10 │   type: slack
  11 │   secretRef:
  12 └     name: slack-webhook
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/overprovisioning/capacity-buffer.yaml (kubernetes)
=======================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/overprovisioning/capacity-buffer.yaml:39-45
────────────────────────────────────────
  39 ┌ spec:
  40 │   # One warm spare node. Raise to widen the buffer (N chunks ~= N warm nodes),
  41 │   # bounded by ksail.prod.yaml's maxNodesTotal: 10.
  42 │   replicas: 1
  43 │   # Shape of one chunk — see pod-template.yaml (same namespace).
  44 │   podTemplateRef:
  45 └     name: overprovisioning
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/overprovisioning/network-policy.yaml (kubernetes)
======================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/overprovisioning/network-policy.yaml:11-15
────────────────────────────────────────
  11 ┌ spec:
  12 │   podSelector: {}
  13 │   policyTypes:
  14 │     - Ingress
  15 └     - Egress
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/patches/attach-hcloud-load-balancer.yaml (kubernetes)
==========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): Gateway 'platform' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/patches/attach-hcloud-load-balancer.yaml:11-17
────────────────────────────────────────
  11 ┌ spec:
  12 │   infrastructure:
  13 │     annotations:
  14 │       load-balancer.hetzner.cloud/location: ${hetzner_lb_location}
  15 │       load-balancer.hetzner.cloud/type: ${hetzner_lb_type}
  16 │       load-balancer.hetzner.cloud/use-private-ip: "true"
  17 └       load-balancer.hetzner.cloud/name: platform-gateway
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/patches/attach-hcloud-load-balancer.yaml:11-17
────────────────────────────────────────
  11 ┌ spec:
  12 │   infrastructure:
  13 │     annotations:
  14 │       load-balancer.hetzner.cloud/location: ${hetzner_lb_location}
  15 │       load-balancer.hetzner.cloud/type: ${hetzner_lb_type}
  16 │       load-balancer.hetzner.cloud/use-private-ip: "true"
  17 └       load-balancer.hetzner.cloud/name: platform-gateway
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/patches/store-vault-snapshots-on-hcloud.yaml (kubernetes)
==============================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/patches/store-vault-snapshots-on-hcloud.yaml:29-37
────────────────────────────────────────
  29 ┌ spec:
  30 │   # Hetzner Cloud Volumes attach reliably via the API to any node in the
  31 │   # location, unlike a Longhorn RWO volume mounted by node-mobile Jobs.
  32 │   storageClassName: hcloud
  33 │   resources:
  34 │     requests:
  35 │       # Hetzner Cloud Volumes have a 10Gi minimum (openbao's own data/audit
  36 │       # PVCs round up to it too); raft snapshots are a few MB each.
  37 └       storage: 10Gi
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vault-seed/push-secret.yaml (kubernetes)
=============================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vault-seed/push-secret.yaml:10-26
────────────────────────────────────────
  10 ┌   namespace: flux-system
  11 │ spec:
  12 │   # 1h, not "0" (push-once): re-pushing hourly from the durable SOPS source
  13 │   # is what re-seeds the key automatically after OpenBao data loss — see
  14 │   # k8s/bases/infrastructure/vault-seed/ (the push-secret-seed-* files).
  15 │   refreshInterval: 1h
  16 │   secretStoreRefs:
  17 │     - name: openbao
  18 └       kind: ClusterSecretStore
  ..   
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium-envoy.yaml (kubernetes)
============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'cilium-envoy' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium-envoy.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: cilium-envoy
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium-envoy.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: cilium-envoy
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium-operator.yaml (kubernetes)
===============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'cilium-operator' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium-operator.yaml:14-18
────────────────────────────────────────
  14 ┌   namespace: kube-system
  15 │ spec:
  16 │   targetRef:
  17 │     apiVersion: apps/v1
  18 └     kind: Deployment
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium-operator.yaml:14-18
────────────────────────────────────────
  14 ┌   namespace: kube-system
  15 │ spec:
  16 │   targetRef:
  17 │     apiVersion: apps/v1
  18 └     kind: Deployment
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium.yaml (kubernetes)
======================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'cilium' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: cilium
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cilium.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: cilium
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cluster-autoscaler-hetzner-cluster-autoscaler.yaml (kubernetes)
=============================================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'cluster-autoscaler-hetzner-cluster-autoscaler' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cluster-autoscaler-hetzner-cluster-autoscaler.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: cluster-autoscaler-hetzner-cluster-autoscaler
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/cluster-autoscaler-hetzner-cluster-autoscaler.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: cluster-autoscaler-hetzner-cluster-autoscaler
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/coredns.yaml (kubernetes)
=======================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'coredns' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/coredns.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: coredns
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/coredns.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: coredns
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/descheduler.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'descheduler' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/descheduler.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: descheduler
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/descheduler.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: descheduler
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-cloud-controller-manager.yaml (kubernetes)
===============================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'hcloud-cloud-controller-manager' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-cloud-controller-manager.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hcloud-cloud-controller-manager
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-cloud-controller-manager.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hcloud-cloud-controller-manager
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-csi-controller.yaml (kubernetes)
=====================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'hcloud-csi-controller' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-csi-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hcloud-csi-controller
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-csi-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hcloud-csi-controller
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-csi-node.yaml (kubernetes)
===============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'hcloud-csi-node' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-csi-node.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: hcloud-csi-node
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hcloud-csi-node.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: hcloud-csi-node
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hubble-relay.yaml (kubernetes)
============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'hubble-relay' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hubble-relay.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hubble-relay
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hubble-relay.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hubble-relay
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hubble-ui.yaml (kubernetes)
=========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'hubble-ui' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hubble-ui.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hubble-ui
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/hubble-ui.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: hubble-ui
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-admission-controller.yaml (kubernetes)
============================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-admission-controller.yaml:13-17
────────────────────────────────────────
  13 ┌   namespace: kyverno
  14 │ spec:
  15 │   targetRef:
  16 │     apiVersion: apps/v1
  17 └     kind: Deployment
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-background-controller.yaml (kubernetes)
=============================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-background-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: kyverno-background-controller
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-cleanup-controller.yaml (kubernetes)
==========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-cleanup-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: kyverno-cleanup-controller
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-reports-controller.yaml (kubernetes)
==========================================================================================================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/kyverno-reports-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: kyverno-reports-controller
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/metrics-server.yaml (kubernetes)
==============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'metrics-server' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/metrics-server.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: metrics-server
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/metrics-server.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: metrics-server
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/snapshot-controller.yaml (kubernetes)
===================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'snapshot-controller' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/snapshot-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: snapshot-controller
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/snapshot-controller.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: snapshot-controller
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/spire-agent.yaml (kubernetes)
===========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'spire-agent' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/spire-agent.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: spire-agent
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/spire-agent.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: spire-agent
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/spire-server.yaml (kubernetes)
============================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'spire-server' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/spire-server.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: StatefulSet
  11 └     name: spire-server
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/spire-server.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: StatefulSet
  11 └     name: spire-server
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/tetragon-operator.yaml (kubernetes)
=================================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'tetragon-operator' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/tetragon-operator.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: tetragon-operator
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/tetragon-operator.yaml:7-11
────────────────────────────────────────
   7 ┌ spec:
   8 │   targetRef:
   9 │     apiVersion: apps/v1
  10 │     kind: Deployment
  11 └     name: tetragon-operator
────────────────────────────────────────



k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/tetragon.yaml (kubernetes)
========================================================================================
Tests: 118 (SUCCESSES: 116, FAILURES: 2)
Failures: 2 (UNKNOWN: 0, LOW: 1, MEDIUM: 1, HIGH: 0, CRITICAL: 0)

KSV-0037 (MEDIUM): VerticalPodAutoscaler 'tetragon' should not be set with 'kube-system' namespace
════════════════════════════════════════
ensure that user resources are not placed in kube-system namespace

See https://avd.aquasec.com/misconfig/ksv-0037
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/tetragon.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: tetragon
────────────────────────────────────────


KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/tetragon.yaml:9-13
────────────────────────────────────────
   9 ┌ spec:
  10 │   targetRef:
  11 │     apiVersion: apps/v1
  12 │     kind: DaemonSet
  13 └     name: tetragon
────────────────────────────────────────



ksail.prod.yaml (kubernetes)
============================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 ksail.prod.yaml:7-249
────────────────────────────────────────
   7 ┌   name: prod
   8 │ spec:
   9 │   cluster:
  10 │     distributionConfig: talos
  11 │     connection:
  12 │       context: admin@prod
  13 │       timeout: 30m
  14 │     distribution: Talos
  15 └     provider: Hetzner
  ..   
────────────────────────────────────────



ksail.yaml (kubernetes)
=======================
Tests: 118 (SUCCESSES: 117, FAILURES: 1)
Failures: 1 (UNKNOWN: 0, LOW: 1, MEDIUM: 0, HIGH: 0, CRITICAL: 0)

KSV-0039 (LOW): A LimitRange policy with a default requests and limits for each container should be configured
════════════════════════════════════════
Ensure that a LimitRange policy is configured to limit resource usage for namespaces or nodes

See https://avd.aquasec.com/misconfig/ksv-0039
────────────────────────────────────────
 ksail.yaml:7-55
────────────────────────────────────────
   7 ┌   name: local
   8 │ spec:
   9 │   cluster:
  10 │     distributionConfig: talos-local
  11 │     connection:
  12 │       context: admin@local
  13 │       timeout: 40m
  14 │     distribution: Talos
  15 └     provider: Docker
  ..   
────────────────────────────────────────



📣 Notices:
  - Version 0.72.0 of Trivy is now available, current version is 0.71.2

To suppress version checks, run Trivy scans with the --skip-version-check flag

(Truncated to last 133333 characters out of 792047)
```

</details>

### ✅ Linters with no issues

[actionlint](https://megalinter.io/9.6.0/descriptors/action_actionlint), [betterleaks](https://megalinter.io/9.6.0/descriptors/repository_betterleaks), [git_diff](https://megalinter.io/9.6.0/descriptors/repository_git_diff), [golangci-lint](https://megalinter.io/9.6.0/descriptors/go_golangci_lint), [grype](https://megalinter.io/9.6.0/descriptors/repository_grype), [jsonlint](https://megalinter.io/9.6.0/descriptors/json_jsonlint), [lychee](https://megalinter.io/9.6.0/descriptors/spell_lychee), [markdown-table-formatter](https://megalinter.io/9.6.0/descriptors/markdown_markdown_table_formatter) (63 fixes), [osv-scanner](https://megalinter.io/9.6.0/descriptors/repository_osv_scanner), [prettier](https://megalinter.io/9.6.0/descriptors/json_prettier) (60 fixes), [prettier](https://megalinter.io/9.6.0/descriptors/yaml_prettier) (22 fixes), [revive](https://megalinter.io/9.6.0/descriptors/go_revive), [secretlint](https://megalinter.io/9.6.0/descriptors/repository_secretlint), [shellcheck](https://megalinter.io/9.6.0/descriptors/bash_shellcheck), [shfmt](https://megalinter.io/9.6.0/descriptors/bash_shfmt) (7 fixes), [syft](https://megalinter.io/9.6.0/descriptors/repository_syft), [trivy-sbom](https://megalinter.io/9.6.0/descriptors/repository_trivy_sbom), [trufflehog](https://megalinter.io/9.6.0/descriptors/repository_trufflehog), [v8r](https://megalinter.io/9.6.0/descriptors/json_v8r), [v8r](https://megalinter.io/9.6.0/descriptors/yaml_v8r), [yamllint](https://megalinter.io/9.6.0/descriptors/yaml_yamllint), [zizmor](https://megalinter.io/9.6.0/descriptors/action_zizmor)


### Notices

📣 **MegaLinter 9.5.0 is out!** Discover the new features and security recommendations in the [release announcement](https://github.com/oxsecurity/megalinter/issues/7835). (Skip this info by defining `SECURITY_SUGGESTIONS: false`)

See detailed reports in MegaLinter artifacts

[![MegaLinter is graciously provided by OX Security](https://raw.githubusercontent.com/oxsecurity/megalinter/main/docs/assets/images/ox-banner.png)](https://www.ox.security/?ref=megalinter)
Show us your support by [**starring ⭐ the repository**](https://github.com/oxsecurity/megalinter)