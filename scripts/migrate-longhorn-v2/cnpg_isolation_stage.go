package main

import (
	"context"
	"errors"
	"time"
)

// prepareIsolation qualifies the owned empty namespace before any data-bearing
// resource is admitted. Its checkpoint is not permission for production cutover.
func (p *cnpgProofRun) prepareIsolation(ctx context.Context) error {
	refused := errors.New("recovery isolation requires the original empty owned environment")
	if p.c.context != "admin@prod" || p.record.Version != 1 || p.record.Phase != "policy-created" || p.record.Plan == nil || len(p.record.Plan.Resources) != 4 || len(p.record.Resources) != 2 || p.record.Preparation != nil || p.record.RestoreVerified || p.record.Cleaned {
		return refused
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if _, err := p.currentExecution(ctx); err != nil {
		return err
	}
	if err := p.emptyRecoveryNamespace(ctx); err != nil {
		return err
	}
	input, err := p.recoveryPolicyInput(ctx)
	if err != nil {
		return err
	}
	bound, err := p.c.observeRecoveryPolicyPreparation(ctx, input)
	if err != nil {
		return err
	}
	if err = p.emptyRecoveryNamespace(ctx); err != nil {
		return err
	}
	after, err := p.recoveryPolicyInput(ctx)
	if err != nil {
		return err
	}
	agent, err := bindRecoveryCiliumAgent(after.Agent, after.DaemonSet, after.Node)
	generation, ok := nativeObservationUint(value(after.Policy, "metadata", "generation"))
	if err != nil || agent != bound.Agent || !ok || generation != bound.Generation || text(after.Policy, "metadata", "uid") != bound.PolicyUID || !commandContextActive(ctx) {
		return refused
	}
	p.record.Preparation, p.record.Phase = &bound, "isolation-prepared"
	return p.save(ctx)
}

// emptyRecoveryNamespace uses complete inventories, never a selector that could
// hide an unexpected controller, credential, claim, or already-started restore.
func (p *cnpgProofRun) emptyRecoveryNamespace(ctx context.Context) error {
	refused := errors.New("recovery namespace is replaced, populated or unreadable")
	identity := p.record.Resources["Namespace"]
	if identity.Name != "longhorn-db-proof-"+p.record.Target+"-"+p.record.Run || identity.Namespace != "" || !isolationUID.MatchString(identity.UID) {
		return refused
	}
	readNamespace := func() error {
		namespace, err := p.c.get(ctx, "namespace", "", identity.Name, false)
		if err != nil || benchmarkIdentity(namespace) != identity || value(namespace, "metadata", "deletionTimestamp") != nil || text(namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" || text(namespace, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(namespace, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
			return refused
		}
		return nil
	}
	if err := readNamespace(); err != nil {
		return err
	}
	for _, kind := range []string{"pods", "jobs", "deployments", "statefulsets", "daemonsets", "cronjobs", "replicasets", "persistentvolumeclaims", "secrets", "clusters.postgresql.cnpg.io", "objectstores.barmancloud.cnpg.io"} {
		objects, err := p.c.list(ctx, kind, identity.Name)
		if err != nil || len(objects) != 0 {
			return refused
		}
	}
	return readNamespace()
}

// recoveryPolicyInput selects the planned consumer, its unique hostname
// and exactly one native agent from complete inventories. No caller supplies a Pod.
func (p *cnpgProofRun) recoveryPolicyInput(ctx context.Context) (recoveryCiliumObservation, error) {
	refused := errors.New("the recovery node, policy or unique agent could not be bound")
	var zero recoveryCiliumObservation
	hostname := cnpgRecoveryHostname(p.record.Plan)
	if hostname == "" || len(hostname) > 63 || !kubeName.MatchString(hostname) {
		return zero, refused
	}
	nodes, err := p.c.list(ctx, "nodes", "")
	if err != nil {
		return zero, refused
	}
	var selected object
	for _, node := range nodes {
		if text(node, "metadata", "name") == hostname || text(node, "metadata", "labels", "kubernetes.io/hostname") == hostname {
			if selected != nil || text(node, "metadata", "name") != hostname || text(node, "metadata", "labels", "kubernetes.io/hostname") != hostname || value(node, "spec", "unschedulable") == true {
				return zero, refused
			}
			selected = node
		}
	}
	if selected == nil {
		return zero, refused
	}
	ds, err := p.c.get(ctx, "daemonset", "kube-system", "cilium", false)
	if err != nil {
		return zero, refused
	}
	pods, err := p.c.list(ctx, "pods", "kube-system")
	if err != nil {
		return zero, refused
	}
	var agent object
	for _, pod := range pods {
		if text(pod, "spec", "nodeName") == hostname && text(pod, "metadata", "labels", "k8s-app") == "cilium" {
			if agent != nil {
				return zero, refused
			}
			agent = pod
		}
	}
	if _, err = bindRecoveryCiliumAgent(agent, ds, selected); err != nil {
		return zero, refused
	}
	identity := p.record.Resources["Policy"]
	policy, err := p.c.get(ctx, "ciliumnetworkpolicies.cilium.io", identity.Namespace, identity.Name, false)
	if err != nil || benchmarkIdentity(policy) != identity || identity.Name != "restore-isolation" || identity.Namespace != p.record.Resources["Namespace"].Name || value(policy, "metadata", "deletionTimestamp") != nil || text(policy, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(policy, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" || !recoveryJSONEqual(value(policy, "spec"), value(p.record.Plan.Resources[1], "spec")) {
		return zero, refused
	}
	return recoveryCiliumObservation{Agent: agent, DaemonSet: ds, Node: selected, Policy: policy, ArchiveEndpoint: p.record.Plan.Source.Endpoint}, nil
}
