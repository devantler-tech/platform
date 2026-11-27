package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
)

var errCNPGStreamSource = errors.New("the PostgreSQL stream source, role, certificate names or WAL sender capacity could not be verified")

const observeCNPGStreamOperation = "observe-v2-database-stream"

func authorizeCNPGStreamObservation(getenv func(string) string) error {
	target := getenv("LONGHORN_V2_DATABASE")
	confirmation := observeCNPGStreamOperation + ":" + target
	if !knownCNPGStreamTarget(target) || getenv("LONGHORN_V2_NODE") != "" ||
		getenv("LONGHORN_V2_OPERATION") != observeCNPGStreamOperation || getenv("LONGHORN_V2_CONFIRM") != confirmation {
		return errCNPGStreamSource
	}
	return authorizeWorkflow(getenv, confirmation)
}

func knownCNPGStreamTarget(target string) bool {
	return target == "substrate-db" || target == "kagent-db" || target == previewCNPGName
}

type cnpgStreamSource struct {
	Cluster, Member             benchmarkResource
	Claim, PV, Volume           benchmarkResource
	Process                     postgresProcess
	State                       postgresState
	Endpoint, CertificateHost   string
	ServerCA, ReplicationSecret string
	Capacity                    string
}

func resolveCNPGStreamSource(ctx context.Context, c client, target, source string) (cnpgStreamSource, error) {
	const senders = `SELECT pg_catalog.json_build_object(
		'version', pg_catalog.current_setting('server_version_num')::int,
		'senders', pg_catalog.current_setting('max_wal_senders')::int - (SELECT count(*) FROM pg_catalog.pg_stat_replication),
		'slots', pg_catalog.current_setting('max_replication_slots')::int - (SELECT count(*) FROM pg_catalog.pg_replication_slots));`
	var unverified cnpgStreamSource
	if c.context != "admin@prod" || !knownCNPGStreamTarget(target) {
		return unverified, errCNPGStreamSource
	}
	observation, err := observeCNPGMigration(ctx, c, target, source)
	if err != nil {
		return unverified, err
	}
	initial, err := bindCNPGStreamSource(observation)
	if err != nil {
		return unverified, err
	}
	member := cnpgStreamMember(observation.Pods, initial.Member.Name)
	if err := c.bindCNPGStreamStorage(ctx, member, &initial); err != nil {
		return unverified, err
	}
	raw, err := c.readBoundPostgresSQL(ctx, observation.Cluster, member, senders)
	var limits struct{ Version, Senders, Slots int }
	if err != nil || json.Unmarshal(raw, &limits) != nil || limits.Version != 180007 || limits.Senders < 2 || limits.Slots < 1 {
		return unverified, errCNPGStreamSource
	}
	fresh, err := c.readCNPGObservation(ctx, cnpgSource{Namespace: initial.Cluster.Namespace, Name: initial.Cluster.Name})
	if err != nil {
		return unverified, err
	}
	if err := c.readCNPGPhysicalState(ctx, &fresh); err != nil {
		return unverified, err
	}
	current, err := bindCNPGStreamSource(fresh)
	if err != nil || validateCNPGObservation(observation, fresh) != nil {
		return unverified, errCNPGStreamSource
	}
	if err := c.bindCNPGStreamStorage(ctx, cnpgStreamMember(fresh.Pods, current.Member.Name), &current); err != nil {
		return unverified, err
	}
	if !cnpgSameStreamSource(initial, current) {
		return unverified, errCNPGStreamSource
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return unverified, err
	}
	return current, nil
}

func (c client) bindCNPGStreamStorage(ctx context.Context, member object, binding *cnpgStreamSource) error {
	namespace := binding.Cluster.Namespace
	claim, err := c.get(ctx, "persistentvolumeclaim", namespace, binding.Member.Name, false)
	if err != nil || !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", namespace) ||
		!providerRWOFilesystem(claim) || text(claim, "status", "phase") != "Bound" ||
		text(claim, "status", "capacity", "storage") != binding.Capacity || !recoveredPostgresMount(member, claim) {
		return errCNPGStreamSource
	}
	claimID := benchmarkIdentity(claim)
	name, class := text(claim, "spec", "volumeName"), text(claim, "spec", "storageClassName")
	if name != "pvc-"+claimID.UID {
		return errCNPGStreamSource
	}
	engine := "v1"
	switch class {
	case "longhorn", "longhorn-wffc":
	case "longhorn-v2":
		engine = "v2"
	default:
		return errCNPGStreamSource
	}
	pods, err := c.list(ctx, "pods", namespace)
	if err != nil || !onlyCNPGStandbyUsesClaim(pods, member, claimID.Name, namespace) {
		return errCNPGStreamSource
	}
	pv, err := c.get(ctx, "persistentvolume", "", name, false)
	if err != nil || !recoveredNativeObject(pv, "v1", "PersistentVolume", "") ||
		text(pv, "status", "phase") != "Bound" || text(pv, "spec", "storageClassName") != class ||
		!providerRWOFilesystem(pv) || text(pv, "spec", "capacity", "storage") != binding.Capacity ||
		text(pv, "spec", "claimRef", "name") != claimID.Name || text(pv, "spec", "claimRef", "namespace") != namespace ||
		text(pv, "spec", "claimRef", "uid") != claimID.UID || text(pv, "spec", "csi", "driver") != "driver.longhorn.io" ||
		text(pv, "spec", "csi", "volumeHandle") != name {
		return errCNPGStreamSource
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", name, false)
	if err != nil || !recoveredNativeObject(volume, "longhorn.io/v1beta2", "Volume", "longhorn-system") ||
		text(volume, "spec", "dataEngine") != engine || text(volume, "status", "state") != "attached" ||
		text(volume, "status", "robustness") != "healthy" || text(volume, "status", "currentNodeID") != text(member, "spec", "nodeName") ||
		text(volume, "status", "kubernetesStatus", "pvName") != name || text(volume, "status", "kubernetesStatus", "namespace") != namespace ||
		text(volume, "status", "kubernetesStatus", "pvcName") != claimID.Name {
		return errCNPGStreamSource
	}
	binding.Claim, binding.PV, binding.Volume = claimID, benchmarkIdentity(pv), benchmarkIdentity(volume)
	return nil
}

func bindCNPGStreamSource(observation cnpgObservation) (cnpgStreamSource, error) {
	var binding cnpgStreamSource
	cluster := observation.Cluster
	name, namespace := text(cluster, "metadata", "name"), text(cluster, "metadata", "namespace")
	port := text(cluster, "spec", "postgresql", "parameters", "port")
	sources := cnpgSources()
	if !recoveredNativeObject(cluster, "postgresql.cnpg.io/v1", "Cluster", namespace) ||
		!knownCNPGStreamTarget(name) || !slices.Contains(sources[:], cnpgSource{Namespace: namespace, Name: name}) ||
		text(cluster, "spec", "imageName") != "ghcr.io/cloudnative-pg/postgresql:18.7-system-trixie" ||
		(port != "" && port != "5432") ||
		value(cluster, "spec", "backup", "barmanObjectStore") != nil {
		return binding, errCNPGStreamSource
	}
	plugins, _ := value(cluster, "spec", "plugins").([]any)
	for _, raw := range plugins {
		plugin, _ := raw.(object)
		if value(plugin, "isWALArchiver") == true {
			return binding, errCNPGStreamSource
		}
	}
	pods, err := healthyCNPGPodsWithRetention(cluster, observation.Pods, "")
	if err != nil {
		return binding, err
	}
	primary := text(cluster, "status", "currentPrimary")
	suffix := "-rw"
	member := cnpgStreamMember(pods, primary)
	instance := float64(2)
	size := "5Gi"
	if name == previewCNPGName {
		instance, size = 1, "2Gi"
	} else {
		for _, pod := range pods {
			if text(pod, "metadata", "name") != primary {
				member, suffix = pod, "-ro"
			}
		}
	}
	if value(cluster, "spec", "instances") != instance || len(pods) != int(instance) ||
		text(cluster, "spec", "storage", "size") != size || (instance == 2 && suffix != "-ro") {
		return binding, errCNPGStreamSource
	}
	process, err := postgresExecutable(member)
	state, found := observation.States[text(member, "metadata", "name")]
	endpoint, addressErr := netip.ParseAddr(text(member, "status", "podIP"))
	if err != nil || !recoveredNativeObject(member, "v1", "Pod", namespace) ||
		!found || validPostgresState(state) != nil || state.InRecovery != (suffix == "-ro") ||
		addressErr != nil || !endpoint.Is4() || !endpoint.IsPrivate() {
		return binding, errCNPGStreamSource
	}
	host := name + suffix + "." + namespace + ".svc"
	certs := valueObject(cluster, "status", "certificates")
	names, _ := value(certs, "serverAltDNSNames").([]any)
	serverCA, replication := text(certs, "serverCASecret"), text(certs, "replicationTLSSecret")
	if !slices.Contains(names, any(host)) || serverCA != name+"-ca" || replication != name+"-replication" ||
		text(certs, "clientCASecret") != name+"-ca" || text(certs, "serverTLSSecret") != name+"-server" {
		return binding, errCNPGStreamSource
	}
	return cnpgStreamSource{Cluster: benchmarkIdentity(cluster), Member: benchmarkIdentity(member), Process: process,
		State: state, Endpoint: endpoint.String(), CertificateHost: host, ServerCA: serverCA,
		ReplicationSecret: replication, Capacity: size}, nil
}

func cnpgStreamMember(pods []object, name string) object {
	for _, pod := range pods {
		if text(pod, "metadata", "name") == name {
			return pod
		}
	}
	return nil
}

func cnpgSameStreamSource(first, current cnpgStreamSource) bool {
	first.State.FlushLSN, first.State.ReplayLSN = "", ""
	current.State.FlushLSN, current.State.ReplayLSN = "", ""
	return first == current
}
