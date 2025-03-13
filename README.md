# Welcome to the DevantlerTech Platform ⛴️

> [!WARNING]
> I am currently working towards making my other OS project [KSail](https://github.com/devantler/ksail) work really well for my homelab. As such this repo will not see much development the coming weeks. Rest assured, that it will recieve some much needed love, where I share my latest and greatest learnings in the Platform Engineering space.

<img width="1840" alt="Screenshot 2024-09-03 at 00 51 44" src="https://github.com/user-attachments/assets/eb6729f7-edff-4346-9be9-0c77d9740633">

This repo contains the deployment artifacts for the DevantlerTech Platform. The platform is a Kubernetes cluster that is highly automated with the use of Flux GitOps, CI/CD with Automated Testing, and much more. Feel free to look around. You might find some inspiration 🙌🏻

<details>
  <summary>Show/hide folder structure</summary>

<!-- readme-tree start -->
```
.
├── .github
│   └── workflows
├── .vscode
├── docs
│   └── images
├── hetzner
├── k8s
│   ├── base
│   │   ├── infrastructure
│   │   │   ├── cert-manager
│   │   │   ├── cilium
│   │   │   ├── cloudflared
│   │   │   ├── cluster-policies
│   │   │   │   └── samples
│   │   │   │       ├── argo
│   │   │   │       │   ├── application-field-validation
│   │   │   │       │   ├── application-prevent-default-project
│   │   │   │       │   ├── application-prevent-updates-project
│   │   │   │       │   ├── applicationset-name-matches-project
│   │   │   │       │   ├── appproject-clusterresourceblacklist
│   │   │   │       │   └── argo-cluster-generation-from-rancher-capi
│   │   │   │       ├── argo-cel
│   │   │   │       │   ├── application-field-validation
│   │   │   │       │   ├── application-prevent-default-project
│   │   │   │       │   ├── application-prevent-updates-project
│   │   │   │       │   ├── applicationset-name-matches-project
│   │   │   │       │   └── appproject-clusterresourceblacklist
│   │   │   │       ├── aws
│   │   │   │       │   ├── require-aws-node-irsa
│   │   │   │       │   └── require-encryption-aws-loadbalancers
│   │   │   │       ├── aws-cel
│   │   │   │       │   └── require-encryption-aws-loadbalancers
│   │   │   │       ├── best-practices
│   │   │   │       │   ├── add-network-policy
│   │   │   │       │   ├── add-networkpolicy-dns
│   │   │   │       │   ├── add-ns-quota
│   │   │   │       │   ├── add-rolebinding
│   │   │   │       │   ├── add-safe-to-evict
│   │   │   │       │   ├── check-deprecated-apis
│   │   │   │       │   ├── disallow-cri-sock-mount
│   │   │   │       │   ├── disallow-default-namespace
│   │   │   │       │   ├── disallow-empty-ingress-host
│   │   │   │       │   ├── disallow-helm-tiller
│   │   │   │       │   ├── disallow-latest-tag
│   │   │   │       │   ├── require-drop-all
│   │   │   │       │   ├── require-drop-cap-net-raw
│   │   │   │       │   ├── require-labels
│   │   │   │       │   ├── require-pod-requests-limits
│   │   │   │       │   ├── require-probes
│   │   │   │       │   ├── require-ro-rootfs
│   │   │   │       │   ├── restrict-image-registries
│   │   │   │       │   ├── restrict-node-port
│   │   │   │       │   └── restrict-service-external-ips
│   │   │   │       ├── best-practices-cel
│   │   │   │       │   ├── check-deprecated-apis
│   │   │   │       │   ├── disallow-cri-sock-mount
│   │   │   │       │   ├── disallow-default-namespace
│   │   │   │       │   ├── disallow-empty-ingress-host
│   │   │   │       │   ├── disallow-helm-tiller
│   │   │   │       │   ├── disallow-latest-tag
│   │   │   │       │   ├── require-drop-all
│   │   │   │       │   ├── require-drop-cap-net-raw
│   │   │   │       │   ├── require-labels
│   │   │   │       │   ├── require-pod-requests-limits
│   │   │   │       │   ├── require-probes
│   │   │   │       │   ├── require-ro-rootfs
│   │   │   │       │   ├── restrict-image-registries
│   │   │   │       │   ├── restrict-node-port
│   │   │   │       │   └── restrict-service-external-ips
│   │   │   │       ├── castai
│   │   │   │       │   └── add-castai-removal-disabled
│   │   │   │       ├── cert-manager
│   │   │   │       │   ├── limit-dnsnames
│   │   │   │       │   ├── limit-duration
│   │   │   │       │   └── restrict-issuer
│   │   │   │       ├── cleanup
│   │   │   │       │   ├── cleanup-bare-pods
│   │   │   │       │   └── cleanup-empty-replicasets
│   │   │   │       ├── consul
│   │   │   │       │   └── enforce-min-tls-version
│   │   │   │       ├── consul-cel
│   │   │   │       │   └── enforce-min-tls-version
│   │   │   │       ├── external-secret-operator
│   │   │   │       │   └── add-external-secret-prefix
│   │   │   │       ├── flux
│   │   │   │       │   ├── generate-flux-multi-tenant-resources
│   │   │   │       │   ├── verify-flux-images
│   │   │   │       │   ├── verify-flux-sources
│   │   │   │       │   └── verify-git-repositories
│   │   │   │       ├── flux-cel
│   │   │   │       │   ├── verify-flux-sources
│   │   │   │       │   └── verify-git-repositories
│   │   │   │       ├── istio
│   │   │   │       │   ├── add-ambient-mode-namespace
│   │   │   │       │   ├── add-sidecar-injection-namespace
│   │   │   │       │   ├── create-authorizationpolicy
│   │   │   │       │   ├── enforce-ambient-mode-namespace
│   │   │   │       │   ├── enforce-sidecar-injection-namespace
│   │   │   │       │   ├── enforce-strict-mtls
│   │   │   │       │   ├── enforce-tls-hosts-host-subnets
│   │   │   │       │   ├── prevent-disabling-injection-pods
│   │   │   │       │   ├── require-authorizationpolicy
│   │   │   │       │   ├── restrict-virtual-service-wildcard
│   │   │   │       │   ├── service-mesh-disallow-capabilities
│   │   │   │       │   └── service-mesh-require-run-as-nonroot
│   │   │   │       ├── istio-cel
│   │   │   │       │   ├── enforce-sidecar-injection-namespace
│   │   │   │       │   ├── enforce-strict-mtls
│   │   │   │       │   └── prevent-disabling-injection-pods
│   │   │   │       ├── karpenter
│   │   │   │       │   ├── add-karpenter-daemonset-priority-class
│   │   │   │       │   ├── add-karpenter-donot-evict
│   │   │   │       │   ├── add-karpenter-nodeselector
│   │   │   │       │   └── set-karpenter-non-cpu-limits
│   │   │   │       ├── kasten
│   │   │   │       │   ├── kasten-3-2-1-backup
│   │   │   │       │   ├── kasten-data-protection-by-label
│   │   │   │       │   ├── kasten-generate-example-backup-policy
│   │   │   │       │   ├── kasten-generate-policy-by-preset-label
│   │   │   │       │   ├── kasten-hourly-rpo
│   │   │   │       │   ├── kasten-immutable-location-profile
│   │   │   │       │   ├── kasten-minimum-retention
│   │   │   │       │   └── kasten-validate-ns-by-preset-label
│   │   │   │       ├── kasten-cel
│   │   │   │       │   ├── k10-data-protection-by-label
│   │   │   │       │   ├── k10-hourly-rpo
│   │   │   │       │   └── k10-validate-ns-by-preset-label
│   │   │   │       ├── kubecost
│   │   │   │       │   ├── enable-kubecost-continuous-rightsizing
│   │   │   │       │   ├── kubecost-proactive-cost-control
│   │   │   │       │   └── require-kubecost-labels
│   │   │   │       ├── kubecost-cel
│   │   │   │       │   └── require-kubecost-labels
│   │   │   │       ├── kubeops
│   │   │   │       │   └── config-syncer-secret-generation-from-rancher-capi
│   │   │   │       ├── kubevirt
│   │   │   │       │   ├── add-services
│   │   │   │       │   └── enforce-instancetype
│   │   │   │       ├── linkerd
│   │   │   │       │   ├── add-linkerd-mesh-injection
│   │   │   │       │   ├── add-linkerd-policy-annotation
│   │   │   │       │   ├── check-linkerd-authorizationpolicy
│   │   │   │       │   ├── prevent-linkerd-pod-injection-override
│   │   │   │       │   ├── prevent-linkerd-port-skipping
│   │   │   │       │   ├── require-linkerd-mesh-injection
│   │   │   │       │   └── require-linkerd-server
│   │   │   │       ├── linkerd-cel
│   │   │   │       │   ├── prevent-linkerd-pod-injection-override
│   │   │   │       │   ├── prevent-linkerd-port-skipping
│   │   │   │       │   └── require-linkerd-mesh-injection
│   │   │   │       ├── nginx-ingress
│   │   │   │       │   ├── disallow-ingress-nginx-custom-snippets
│   │   │   │       │   ├── restrict-annotations
│   │   │   │       │   └── restrict-ingress-paths
│   │   │   │       ├── nginx-ingress-cel
│   │   │   │       │   ├── disallow-ingress-nginx-custom-snippets
│   │   │   │       │   ├── restrict-annotations
│   │   │   │       │   └── restrict-ingress-paths
│   │   │   │       ├── openshift
│   │   │   │       │   ├── check-routes
│   │   │   │       │   ├── disallow-deprecated-apis
│   │   │   │       │   ├── disallow-jenkins-pipeline-strategy
│   │   │   │       │   ├── disallow-security-context-constraint-anyuid
│   │   │   │       │   ├── disallow-self-provisioner-binding
│   │   │   │       │   ├── enforce-etcd-encryption
│   │   │   │       │   ├── inject-infrastructurename
│   │   │   │       │   ├── team-validate-ns-name
│   │   │   │       │   └── unique-routes
│   │   │   │       ├── openshift-cel
│   │   │   │       │   ├── check-routes
│   │   │   │       │   ├── disallow-deprecated-apis
│   │   │   │       │   ├── disallow-jenkins-pipeline-strategy
│   │   │   │       │   ├── disallow-security-context-constraint-anyuid
│   │   │   │       │   └── enforce-etcd-encryption
│   │   │   │       ├── other
│   │   │   │       │   ├── add-certificates-volume
│   │   │   │       │   ├── add-default-resources
│   │   │   │       │   ├── add-default-securitycontext
│   │   │   │       │   ├── add-emptydir-sizelimit
│   │   │   │       │   ├── add-env-vars-from-cm
│   │   │   │       │   ├── add-image-as-env-var
│   │   │   │       │   ├── add-imagepullsecrets
│   │   │   │       │   ├── add-imagepullsecrets-for-containers-and-initcontainers
│   │   │   │       │   ├── add-labels
│   │   │   │       │   ├── add-ndots
│   │   │   │       │   ├── add-node-affinity
│   │   │   │       │   ├── add-node-labels-pod
│   │   │   │       │   ├── add-nodeSelector
│   │   │   │       │   ├── add-pod-priorityclassname
│   │   │   │       │   ├── add-pod-proxies
│   │   │   │       │   ├── add-tolerations
│   │   │   │       │   ├── add-ttl-jobs
│   │   │   │       │   ├── add-volume-deployment
│   │   │   │       │   ├── advanced-restrict-image-registries
│   │   │   │       │   ├── advertise-node-extended-resources
│   │   │   │       │   ├── allowed-annotations
│   │   │   │       │   ├── allowed-base-images
│   │   │   │       │   ├── allowed-image-repos
│   │   │   │       │   ├── allowed-label-changes
│   │   │   │       │   ├── allowed-pod-priorities
│   │   │   │       │   ├── always-pull-images
│   │   │   │       │   ├── annotate-base-images
│   │   │   │       │   ├── apply-pss-restricted-profile
│   │   │   │       │   ├── audit-event-on-delete
│   │   │   │       │   ├── audit-event-on-exec
│   │   │   │       │   ├── block-cluster-admin-from-ns
│   │   │   │       │   ├── block-ephemeral-containers
│   │   │   │       │   ├── block-images-with-volumes
│   │   │   │       │   ├── block-large-images
│   │   │   │       │   ├── block-pod-exec-by-namespace
│   │   │   │       │   ├── block-pod-exec-by-namespace-label
│   │   │   │       │   ├── block-pod-exec-by-pod-and-container
│   │   │   │       │   ├── block-pod-exec-by-pod-label
│   │   │   │       │   ├── block-pod-exec-by-pod-name
│   │   │   │       │   ├── block-stale-images
│   │   │   │       │   ├── block-updates-deletes
│   │   │   │       │   ├── check-env-vars
│   │   │   │       │   ├── check-hpa-exists
│   │   │   │       │   ├── check-ingress-nginx-controller-version-and-annotation-policy
│   │   │   │       │   ├── check-node-for-cve-2022-0185
│   │   │   │       │   ├── check-nvidia-gpu
│   │   │   │       │   ├── check-serviceaccount
│   │   │   │       │   ├── check-serviceaccount-secrets
│   │   │   │       │   ├── check-subjectaccessreview
│   │   │   │       │   ├── check-vpa-configuration
│   │   │   │       │   ├── concatenate-configmaps
│   │   │   │       │   ├── copy-namespace-labels
│   │   │   │       │   ├── cordon-and-drain-node
│   │   │   │       │   ├── create-default-pdb
│   │   │   │       │   ├── create-pod-antiaffinity
│   │   │   │       │   ├── deny-commands-in-exec-probe
│   │   │   │       │   ├── deny-secret-service-account-token-type
│   │   │   │       │   ├── deployment-replicas-higher-than-pdb
│   │   │   │       │   ├── disable-automountserviceaccounttoken
│   │   │   │       │   ├── disable-service-discovery
│   │   │   │       │   ├── disallow-all-secrets
│   │   │   │       │   ├── disallow-localhost-services
│   │   │   │       │   ├── disallow-secrets-from-env-vars
│   │   │   │       │   ├── dns-policy-and-dns-config
│   │   │   │       │   ├── docker-socket-requires-label
│   │   │   │       │   ├── enforce-pod-duration
│   │   │   │       │   ├── enforce-readwriteonce-pod
│   │   │   │       │   ├── enforce-resources-as-ratio
│   │   │   │       │   ├── ensure-probes-different
│   │   │   │       │   ├── ensure-production-matches-staging
│   │   │   │       │   ├── ensure-readonly-hostpath
│   │   │   │       │   ├── exclude-namespaces-dynamically
│   │   │   │       │   ├── expiration-for-policyexceptions
│   │   │   │       │   ├── forbid-cpu-limits
│   │   │   │       │   ├── generate-networkpolicy-existing
│   │   │   │       │   ├── get-debug-information
│   │   │   │       │   ├── imagepullpolicy-always
│   │   │   │       │   ├── ingress-host-match-tls
│   │   │   │       │   ├── inject-env-var-from-image-label
│   │   │   │       │   ├── inject-sidecar-deployment
│   │   │   │       │   ├── inspect-csr
│   │   │   │       │   ├── kubernetes-version-check
│   │   │   │       │   ├── label-existing-namespaces
│   │   │   │       │   ├── label-nodes-cri
│   │   │   │       │   ├── limit-configmap-for-sa
│   │   │   │       │   ├── limit-containers-per-pod
│   │   │   │       │   ├── limit-hostpath-type-pv
│   │   │   │       │   ├── limit-hostpath-vols
│   │   │   │       │   ├── memory-requests-equal-limits
│   │   │   │       │   ├── metadata-match-regex
│   │   │   │       │   ├── mitigate-log4shell
│   │   │   │       │   ├── mutate-large-termination-gps
│   │   │   │       │   ├── mutate-pod-binding
│   │   │   │       │   ├── namespace-inventory-check
│   │   │   │       │   ├── namespace-protection
│   │   │   │       │   ├── nfs-subdir-external-provisioner-storage-path
│   │   │   │       │   ├── only-trustworthy-registries-set-root
│   │   │   │       │   ├── pdb-maxunavailable
│   │   │   │       │   ├── pdb-maxunavailable-with-deployments
│   │   │   │       │   ├── pdb-minavailable
│   │   │   │       │   ├── policy-for-exceptions
│   │   │   │       │   ├── prepend-image-registry
│   │   │   │       │   ├── prevent-bare-pods
│   │   │   │       │   ├── prevent-cr8escape
│   │   │   │       │   ├── prevent-duplicate-hpa
│   │   │   │       │   ├── prevent-duplicate-vpa
│   │   │   │       │   ├── protect-node-taints
│   │   │   │       │   ├── record-creation-details
│   │   │   │       │   ├── refresh-env-var-in-pod
│   │   │   │       │   ├── refresh-volumes-in-pods
│   │   │   │       │   ├── remove-hostpath-volumes
│   │   │   │       │   ├── remove-serviceaccount-token
│   │   │   │       │   ├── replace-image-registry
│   │   │   │       │   ├── replace-image-registry-with-harbor
│   │   │   │       │   ├── replace-ingress-hosts
│   │   │   │       │   ├── require-annotations
│   │   │   │       │   ├── require-base-image
│   │   │   │       │   ├── require-container-port-names
│   │   │   │       │   ├── require-cpu-limits
│   │   │   │       │   ├── require-deployments-have-multiple-replicas
│   │   │   │       │   ├── require-emptydir-requests-limits
│   │   │   │       │   ├── require-image-checksum
│   │   │   │       │   ├── require-image-source
│   │   │   │       │   ├── require-imagepullsecrets
│   │   │   │       │   ├── require-ingress-https
│   │   │   │       │   ├── require-netpol
│   │   │   │       │   ├── require-non-root-groups
│   │   │   │       │   ├── require-pdb
│   │   │   │       │   ├── require-pod-priorityclassname
│   │   │   │       │   ├── require-qos-burstable
│   │   │   │       │   ├── require-qos-guaranteed
│   │   │   │       │   ├── require-reasonable-pdbs
│   │   │   │       │   ├── require-replicas-allow-disruption
│   │   │   │       │   ├── require-storageclass
│   │   │   │       │   ├── require-unique-external-dns
│   │   │   │       │   ├── require-unique-service-selector
│   │   │   │       │   ├── require-unique-uid-per-workload
│   │   │   │       │   ├── require-vulnerability-scan
│   │   │   │       │   ├── resolve-image-to-digest
│   │   │   │       │   ├── resource-creation-updating-denied
│   │   │   │       │   ├── restart-deployment-on-secret-change
│   │   │   │       │   ├── restrict-annotations
│   │   │   │       │   ├── restrict-automount-sa-token
│   │   │   │       │   ├── restrict-binding-clusteradmin
│   │   │   │       │   ├── restrict-binding-system-groups
│   │   │   │       │   ├── restrict-clusterrole-csr
│   │   │   │       │   ├── restrict-clusterrole-mutating-validating-admission-webhooks
│   │   │   │       │   ├── restrict-clusterrole-nodesproxy
│   │   │   │       │   ├── restrict-controlplane-scheduling
│   │   │   │       │   ├── restrict-deprecated-registry
│   │   │   │       │   ├── restrict-edit-for-endpoints
│   │   │   │       │   ├── restrict-escalation-verbs-roles
│   │   │   │       │   ├── restrict-ingress-classes
│   │   │   │       │   ├── restrict-ingress-defaultbackend
│   │   │   │       │   ├── restrict-ingress-host
│   │   │   │       │   ├── restrict-ingress-wildcard
│   │   │   │       │   ├── restrict-jobs
│   │   │   │       │   ├── restrict-loadbalancer
│   │   │   │       │   ├── restrict-networkpolicy-empty-podselector
│   │   │   │       │   ├── restrict-node-affinity
│   │   │   │       │   ├── restrict-node-label-changes
│   │   │   │       │   ├── restrict-node-label-creation
│   │   │   │       │   ├── restrict-node-selection
│   │   │   │       │   ├── restrict-pod-controller-serviceaccount-updates
│   │   │   │       │   ├── restrict-pod-count-per-node
│   │   │   │       │   ├── restrict-sa-automount-sa-token
│   │   │   │       │   ├── restrict-scale
│   │   │   │       │   ├── restrict-secret-role-verbs
│   │   │   │       │   ├── restrict-secrets-by-label
│   │   │   │       │   ├── restrict-secrets-by-name
│   │   │   │       │   ├── restrict-service-account
│   │   │   │       │   ├── restrict-service-port-range
│   │   │   │       │   ├── restrict-storageclass
│   │   │   │       │   ├── restrict-usergroup-fsgroup-id
│   │   │   │       │   ├── restrict-wildcard-resources
│   │   │   │       │   ├── restrict-wildcard-verbs
│   │   │   │       │   ├── scale-deployment-zero
│   │   │   │       │   ├── spread-pods-across-topology
│   │   │   │       │   ├── sync-secrets
│   │   │   │       │   ├── time-bound-policy
│   │   │   │       │   ├── topologyspreadconstraints-policy
│   │   │   │       │   ├── unique-ingress-host-and-path
│   │   │   │       │   ├── unique-ingress-paths
│   │   │   │       │   ├── update-image-tag
│   │   │   │       │   ├── verify-image
│   │   │   │       │   ├── verify-image-cve-2022-42889
│   │   │   │       │   ├── verify-image-gcpkms
│   │   │   │       │   ├── verify-image-slsa
│   │   │   │       │   ├── verify-image-with-multi-keys
│   │   │   │       │   ├── verify-manifest-integrity
│   │   │   │       │   ├── verify-sbom-cyclonedx
│   │   │   │       │   └── verify-vpa-target
│   │   │   │       ├── other-cel
│   │   │   │       │   ├── advanced-restrict-image-registries
│   │   │   │       │   ├── allowed-annotations
│   │   │   │       │   ├── allowed-pod-priorities
│   │   │   │       │   ├── block-ephemeral-containers
│   │   │   │       │   ├── check-env-vars
│   │   │   │       │   ├── check-node-for-cve-2022-0185
│   │   │   │       │   ├── check-serviceaccount-secrets
│   │   │   │       │   ├── deny-commands-in-exec-probe
│   │   │   │       │   ├── deny-secret-service-account-token-type
│   │   │   │       │   ├── disallow-all-secrets
│   │   │   │       │   ├── disallow-localhost-services
│   │   │   │       │   ├── disallow-secrets-from-env-vars
│   │   │   │       │   ├── docker-socket-requires-label
│   │   │   │       │   ├── enforce-pod-duration
│   │   │   │       │   ├── enforce-readwriteonce-pod
│   │   │   │       │   ├── ensure-probes-different
│   │   │   │       │   ├── ensure-readonly-hostpath
│   │   │   │       │   ├── exclude-namespaces-dynamically
│   │   │   │       │   ├── forbid-cpu-limits
│   │   │   │       │   ├── imagepullpolicy-always
│   │   │   │       │   ├── ingress-host-match-tls
│   │   │   │       │   ├── limit-containers-per-pod
│   │   │   │       │   ├── limit-hostpath-type-pv
│   │   │   │       │   ├── limit-hostpath-vols
│   │   │   │       │   ├── memory-requests-equal-limits
│   │   │   │       │   ├── metadata-match-regex
│   │   │   │       │   ├── pdb-maxunavailable
│   │   │   │       │   ├── prevent-bare-pods
│   │   │   │       │   ├── prevent-cr8escape
│   │   │   │       │   ├── require-annotations
│   │   │   │       │   ├── require-container-port-names
│   │   │   │       │   ├── require-deployments-have-multiple-replicas
│   │   │   │       │   ├── require-emptydir-requests-limits
│   │   │   │       │   ├── require-image-checksum
│   │   │   │       │   ├── require-ingress-https
│   │   │   │       │   ├── require-non-root-groups
│   │   │   │       │   ├── require-pod-priorityclassname
│   │   │   │       │   ├── require-qos-burstable
│   │   │   │       │   ├── require-qos-guaranteed
│   │   │   │       │   ├── require-storageclass
│   │   │   │       │   ├── restrict-annotations
│   │   │   │       │   ├── restrict-binding-clusteradmin
│   │   │   │       │   ├── restrict-binding-system-groups
│   │   │   │       │   ├── restrict-clusterrole-nodesproxy
│   │   │   │       │   ├── restrict-controlplane-scheduling
│   │   │   │       │   ├── restrict-deprecated-registry
│   │   │   │       │   ├── restrict-edit-for-endpoints
│   │   │   │       │   ├── restrict-escalation-verbs-roles
│   │   │   │       │   ├── restrict-ingress-classes
│   │   │   │       │   ├── restrict-ingress-defaultbackend
│   │   │   │       │   ├── restrict-ingress-wildcard
│   │   │   │       │   ├── restrict-jobs
│   │   │   │       │   ├── restrict-loadbalancer
│   │   │   │       │   ├── restrict-networkpolicy-empty-podselector
│   │   │   │       │   ├── restrict-node-affinity
│   │   │   │       │   ├── restrict-node-label-creation
│   │   │   │       │   ├── restrict-pod-controller-serviceaccount-updates
│   │   │   │       │   ├── restrict-sa-automount-sa-token
│   │   │   │       │   ├── restrict-secret-role-verbs
│   │   │   │       │   ├── restrict-secrets-by-name
│   │   │   │       │   ├── restrict-service-port-range
│   │   │   │       │   ├── restrict-storageclass
│   │   │   │       │   ├── restrict-usergroup-fsgroup-id
│   │   │   │       │   ├── restrict-wildcard-resources
│   │   │   │       │   ├── restrict-wildcard-verbs
│   │   │   │       │   └── topologyspreadconstraints-policy
│   │   │   │       ├── pod-security
│   │   │   │       │   ├── baseline
│   │   │   │       │   │   ├── disallow-capabilities
│   │   │   │       │   │   ├── disallow-host-namespaces
│   │   │   │       │   │   ├── disallow-host-path
│   │   │   │       │   │   ├── disallow-host-ports
│   │   │   │       │   │   ├── disallow-host-ports-range
│   │   │   │       │   │   ├── disallow-host-process
│   │   │   │       │   │   ├── disallow-privileged-containers
│   │   │   │       │   │   ├── disallow-proc-mount
│   │   │   │       │   │   ├── disallow-selinux
│   │   │   │       │   │   ├── restrict-apparmor-profiles
│   │   │   │       │   │   ├── restrict-seccomp
│   │   │   │       │   │   └── restrict-sysctls
│   │   │   │       │   ├── restricted
│   │   │   │       │   │   ├── disallow-capabilities-strict
│   │   │   │       │   │   ├── disallow-privilege-escalation
│   │   │   │       │   │   ├── require-run-as-non-root-user
│   │   │   │       │   │   ├── require-run-as-nonroot
│   │   │   │       │   │   ├── restrict-seccomp-strict
│   │   │   │       │   │   └── restrict-volume-types
│   │   │   │       │   └── subrule
│   │   │   │       │       ├── podsecurity-subrule-baseline
│   │   │   │       │       └── restricted
│   │   │   │       │           ├── restricted-exclude-capabilities
│   │   │   │       │           ├── restricted-exclude-seccomp
│   │   │   │       │           └── restricted-latest
│   │   │   │       ├── pod-security-cel
│   │   │   │       │   ├── baseline
│   │   │   │       │   │   ├── disallow-capabilities
│   │   │   │       │   │   ├── disallow-host-namespaces
│   │   │   │       │   │   ├── disallow-host-path
│   │   │   │       │   │   ├── disallow-host-ports
│   │   │   │       │   │   ├── disallow-host-ports-range
│   │   │   │       │   │   ├── disallow-host-process
│   │   │   │       │   │   ├── disallow-privileged-containers
│   │   │   │       │   │   ├── disallow-proc-mount
│   │   │   │       │   │   ├── disallow-selinux
│   │   │   │       │   │   ├── restrict-seccomp
│   │   │   │       │   │   └── restrict-sysctls
│   │   │   │       │   └── restricted
│   │   │   │       │       ├── disallow-capabilities-strict
│   │   │   │       │       ├── disallow-privilege-escalation
│   │   │   │       │       ├── require-run-as-non-root-user
│   │   │   │       │       ├── require-run-as-nonroot
│   │   │   │       │       ├── restrict-seccomp-strict
│   │   │   │       │       └── restrict-volume-types
│   │   │   │       ├── psa
│   │   │   │       │   ├── add-privileged-existing-namespaces
│   │   │   │       │   ├── add-psa-labels
│   │   │   │       │   ├── add-psa-namespace-reporting
│   │   │   │       │   └── deny-privileged-profile
│   │   │   │       ├── psa-cel
│   │   │   │       │   ├── add-psa-namespace-reporting
│   │   │   │       │   └── deny-privileged-profile
│   │   │   │       ├── psp-migration
│   │   │   │       │   ├── add-apparmor
│   │   │   │       │   ├── add-capabilities
│   │   │   │       │   ├── add-runtimeClassName
│   │   │   │       │   ├── check-supplemental-groups
│   │   │   │       │   ├── restrict-adding-capabilities
│   │   │   │       │   └── restrict-runtimeClassName
│   │   │   │       ├── psp-migration-cel
│   │   │   │       │   ├── check-supplemental-groups
│   │   │   │       │   ├── restrict-adding-capabilities
│   │   │   │       │   └── restrict-runtimeClassName
│   │   │   │       ├── tekton
│   │   │   │       │   ├── block-tekton-task-runs
│   │   │   │       │   ├── require-tekton-bundle
│   │   │   │       │   ├── require-tekton-namespace-pipelinerun
│   │   │   │       │   ├── require-tekton-securitycontext
│   │   │   │       │   ├── verify-tekton-pipeline-bundle-signatures
│   │   │   │       │   ├── verify-tekton-taskrun-signatures
│   │   │   │       │   └── verify-tekton-taskrun-vuln-scan
│   │   │   │       ├── tekton-cel
│   │   │   │       │   ├── block-tekton-task-runs
│   │   │   │       │   └── require-tekton-bundle
│   │   │   │       ├── traefik
│   │   │   │       │   └── disallow-default-tlsoptions
│   │   │   │       ├── traefik-cel
│   │   │   │       │   └── disallow-default-tlsoptions
│   │   │   │       ├── velero
│   │   │   │       │   ├── backup-all-volumes
│   │   │   │       │   ├── block-velero-restore
│   │   │   │       │   └── validate-cron-schedule
│   │   │   │       ├── velero-cel
│   │   │   │       │   ├── block-velero-restore
│   │   │   │       │   └── validate-cron-schedule
│   │   │   │       └── windows-security
│   │   │   │           └── require-run-as-containeruser
│   │   │   └── traefik
│   │   └── variables
│   ├── clusters
│   │   └── local
│   │       ├── infrastructure
│   │       └── variables
│   └── distributions
│       ├── k3s
│       ├── native
│       │   ├── infrastructure
│       │   └── variables
│       └── talos
├── k8s-old
│   ├── clusters
│   │   ├── homelab-local
│   │   │   ├── apps
│   │   │   ├── flux-system
│   │   │   ├── infrastructure
│   │   │   │   └── controllers
│   │   │   └── variables
│   │   └── homelab-prod
│   │       ├── apps
│   │       ├── flux-system
│   │       ├── infrastructure
│   │       │   ├── controllers
│   │       │   └── gha-runner-scale-sets
│   │       └── variables
│   ├── components
│   │   ├── flux-kustomization-post-build-variables-label
│   │   ├── flux-kustomization-sops-label
│   │   ├── helm-release-crds-label
│   │   ├── helm-release-helm-test-label
│   │   ├── helm-release-remediation-label
│   │   └── network-policy-default-deny
│   ├── distributions
│   │   └── talos
│   │       └── infrastructure
│   │           └── controllers
│   │               ├── cilium
│   │               ├── kubelet-serving-cert-approver
│   │               └── longhorn
│   └── shared
│       ├── apps
│       │   ├── fleetdm
│       │   ├── headlamp
│       │   ├── homepage
│       │   ├── open-webui
│       │   └── plantuml
│       ├── infrastructure
│       │   ├── cloudflared
│       │   ├── controllers
│       │   │   ├── capi-operator
│       │   │   ├── cert-manager
│       │   │   ├── dex
│       │   │   ├── gha-runner-scale-set-controller
│       │   │   ├── k8sgpt-operator
│       │   │   ├── kyverno
│       │   │   ├── metrics-server
│       │   │   ├── reloader
│       │   │   ├── testkube
│       │   │   │   └── crds
│       │   │   ├── traefik
│       │   │   └── trivy-operator
│       │   ├── goldilocks
│       │   ├── harbor
│       │   ├── helm-charts-oci-proxy
│       │   ├── middlewares
│       │   │   ├── basic-auth
│       │   │   └── forward-auth
│       │   ├── oauth2-proxy
│       │   ├── ollama
│       │   └── selfsigned-cluster-issuer
│       ├── tenants
│       └── variables
└── talos
    └── patches
        ├── cluster
        └── nodes

603 directories
```
<!-- readme-tree end -->

</details>

## Prerequisites

For development:

- [Docker](https://docs.docker.com/get-docker/)
- [KSail](https://github.com/devantler/ksail)

For production:

- A Talos Cluster

> [!NOTE]
> You can use other distributions as well, but the configuration is optimized for Talos, and thus it is not guaranteed to work with other distributions.

## Usage

To run this cluster locally, simply run the following command:

```bash
ksail up homelab-local
```

> [!NOTE]
> To run this cluster on your metal, would require that you have access to my SOPS keys. This is ofcourse not possible, so you would need to create your own keys and replace the existing ones, if you want to run my cluster configuration on your own metal.
>
> - The keys that `KSail` uses are stored in `~/.ksail/age` where one Age key is store for each cluster, and named according to the cluster name. For example `~/.ksail/age/homelab-local`.
> - To update SOPS to work with `Ksail`, you need to update the `.sops.yaml` file in the root of the repository, and replace the `age` keys with your own keys.
> - To update the manifests to work with `KSail`, you need to replace all `.sops.yaml` files with new ones, that are encrypted with your own keys.
>
> For the production cluster, you would need to do the same, but in addition to storing the keys in `~/.ksail/age`, you would also need to store the keys in GitHub Secrets, such that the CI/CD pipeline can provision the keys to the cluster.

## Stack

The cluster uses Flux GitOps to reconcile the state of the cluster with single source of truth stored in this repository and published as an OCI image. For development, the cluster is spun up by `KSail` and for production, the cluster is provisioned by `Talos Omni`.

The cluster configuration is stored in the `k8s/*` directories where the structure is as follows:

- [`clusters/`](k8s/clusters): Contains the the cluster specific configuration for each environment.
- [`components/`](k8s/components): Contains the reusable components that are used across the cluster.
- [`distributions/`](k8s/distributions): Contains the distribution specific configuration.
- [`shared/`](k8s/shared): Contains the shared configuration for all clusters.
  - [`apps/`](k8s/shared/apps): Contains the application specific manifests.
    - [FleetDM](k8s/shared/apps/fleetdm) - To provide a device management for my devices. (currently not in use, as it does not support ARM64)
    - [Headlamp](k8s/shared/apps/headlamp) - To provide a lightweight and extensible Kubernetes UI.
    - [Homepage](k8s/shared/apps/homepage) - To provide a dashborad for the cluster.
    - [Open WebUI](k8s/shared/apps/open-webui) - To provide a web interface and a REST API for interacting with LLM's.
    - [PlantUML](k8s/shared/infrastructure/plantuml) - To provide a web interface and a REST API for generating PlantUML diagrams.
    - [Traefik](k8s/shared/infrastructure/controllers/traefik) - To provide an ingress controller for the cluster.
  - [`custom-resources/`](k8s/shared/custom-resources): Contains the custom resources that are used across the cluster.
    - [Middlewares](k8s/shared/infrastructure/middlewares) - Contains the middlewares that are used by Traefik.
    - [Selfsigned Cluster Issuer](k8s/shared/infrastructure/selfsigned-cluster-issuer) - Contains the selfsigned cluster issuer that is used by Traefik.
  - [`infrastructure/`](k8s/shared/infrastructure): Contains the infrastructure specific manifests.
    - [Cert Manager](k8s/shared/infrastructure/cert-manager) - For managing certificates in the cluster.
    - [Cloudflared](k8s/shared/infrastructure/cloudflared) - For tunneling traffic to the cluster.
    - [Dex](k8s/shared/infrastructure/controllers/dex) - For providing OIDC authentication for the cluster.
    - [Cluster API Operator](k8s/shared/infrastructure/capi-operator) - For managing the lifecycle of Kubernetes clusters.
    - [GitHub Actions Runner Scale Set Controller](k8s/shared/infrastructure/gha-runner-scale-set-controller) - To manage GitHub Actions Runner Scale Sets in the cluster.
    - [GitHub Actions Runner Scale Sets](k8s/clusters/homelab-prod/infrastructure/gha-runner-scale-sets) - To run GitHub Actions in the cluster.
    - [Goldilocks](k8s/shared/infrastructure/controllers/goldilocks) - To provide and apply resource recommendations for pods.
    - [Harbor](k8s/shared/infrastructure/harbor) - To store and distribute container images.
    - [K8sGPT Operator](k8s/shared/infrastructure/controllers/k8sgpt-operator) - To analyze the cluster for improvements, vulnerabilities or bugs.
    - [Kyverno](k8s/shared/infrastructure/controllers/kyverno) - To enforce policies in the cluster.
    - [Longhorn](k8s/distributions/talos/core/longhorn) - To provide distributed storage for the cluster.
    - [Metrics Server](k8s/shared/infrastructure/controllers/metrics-server) - To provide metrics for the cluster.
    - [OAuth2 Proxy](k8s/shared/infrastructure/oauth2-proxy) - To provide authentication for the cluster.
    - [Ollama](k8s/shared/infrastructure/ollama) - To run LLM's on the cluster.
    - [Reloader](k8s/shared/infrastructure/controllers/reloader) - To reload deployments when secrets or configmaps change.
    - [Testkube](k8s/shared/infrastructure/controllers/testkube) - To provide a testing framework for the cluster.
    - [Trivy Operator](k8s/shared/infrastructure/controllers/trivy-operator) - To analyze the cluster for vulnerabilities.
  - [`tenants`](k8s/shared/tenants): Contains Flux kustomizations to bootstrap and onboard tenants. (currently not in use)
  - [`variables/`](k8s/shared/variables): Contains global variables, that are the same for all clusters.

## Kustomize and Flux Kustomization Flow

To support hooking into the kustomize flow for adding or modifying resources for a specific cluster, a specific distribution, or shared across all clusters, the following structure is used:

![Structure](docs/images/gitops-structure.drawio.png)

This means that for every root level kustomization that is applied to the cluster, there should be a corresponding folder in either `clusters`, `distributions`, or `shared` that contains the resources that should be applied to the cluster at that scope. For example, for a root level kustomization in `k8s/clusters/<cluster-name>/flux-system/infrastructure.yaml`, there should be a corresponding folder in:

- `k8s/clusters/<cluster-name>/infrastructure/`
- `k8s/distributions/<distribution-name>/infrastructure/`
- `k8s/shared/infrastructure/`

## Production Environment

### Nodes

- 1x [Hetzner CAX21 node](https://www.hetzner.com/cloud/) (QEMU ARM64 4CPU 8Gb RAM 160Gb SSD) for both control plane and worker node
- 2x [Hetzner CAX41 node](https://www.hetzner.com/cloud/) (QEMU ARM64 16CPU 32Gb RAM 320Gb SSD) for both control plane and worker nodes
- 1x Apple Hypervisor ARM64 VM (Running on Mac Mini M2 Pro with access to 32GB RAM and 20 cores (overprovisioned 2/1) as a worker node

### Hardware

- [Unifi Cloud Gateway](https://eu.store.ui.com/eu/en/pro/products/ucg-ultra) - For networking and firewall.
- [External Samsung T5/T7 SSD Disks](https://www.samsung.com/dk/memory-storage/portable-ssd/portable-ssd-t7-1tb-gray-mu-pc1t0t-ww/) - For distributed storage across the cluster.

### Software

- [Unifi](https://ui.com/) - For configuring a DMZ zone for my own nodes to run in, along with other security features.
- [UTM](https://mac.getutm.app) - For running Kubernetes on Mac Mini via Apple Hypervisor.
- [Talos Omni](https://www.siderolabs.com/platform/saas-for-kubernetes/) - For provisioning the production cluster, and managing nodes, updates, and the Talos configuration.
- [Cloudflare](https://www.cloudflare.com) - For etcd backups, DNS, and tunneling all traffic so my network stays private.
- [Flux GitOps](https://fluxcd.io) - For managing the kubernetes applications and infrastructure declaratively.
- [SOPS](https://getsops.io) and [Age](https://github.com/FiloSottile/age) - For encrypting secrets at rest, allowing me to store them in this repository with confidence.
- [KSail](https://github.com/devantler/ksail) - For developing the cluster locally, and for running the cluster in CI to ensure all changes are properly tested before being applied to the production cluster.
- [K8sGPT](https://k8sgpt.ai) - To analyze the cluster for improvements, vulnerabilities or bugs. It integrates with Trivy and Kuverno to also provide security and policy suggestions.

### Monthly Cost

| Item               | No. | Per unit | Total      |
| ------------------ | --- | -------- | ---------- |
| Hetzner CAX21      | 3   | 7,49€    | $24,9      |
| Hetzner CAX41      | 1   | 29,99€   | $33,23     |
| Talos Omni         | 1   | $10      | $10        |
| Cloudflare Domains | 2   | $0,87    | $1,74      |
|                    |     |          | **$69,87** |

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=devantler/homelab&type=Date)](https://star-history.com/#devantler/homelab&Date)
