package main

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// cnpgOwned joins the instance label with the native controlling Cluster UID.
func cnpgOwned(o, cluster object) bool {
	return text(o, "metadata", "labels", "cnpg.io/cluster") == text(cluster, "metadata", "name") && cnpgOwnerReference(o, cluster)
}

// cnpgOwnerReference requires the selected namespaced Cluster controller.
func cnpgOwnerReference(o, cluster object) bool {
	if text(o, "metadata", "namespace") != text(cluster, "metadata", "namespace") {
		return false
	}
	refs, _ := value(o, "metadata", "ownerReferences").([]any)
	for _, ref := range refs {
		r, _ := ref.(object)
		if text(r, "apiVersion") == "postgresql.cnpg.io/v1" && text(r, "kind") == "Cluster" && text(r, "name") == text(cluster, "metadata", "name") && text(r, "uid") == text(cluster, "metadata", "uid") && text(r, "uid") != "" && value(r, "controller") == true {
			return true
		}
	}
	return false
}

// cnpgCondition admits only the operator's positive condition receipt.
func cnpgCondition(cluster object, condition string) bool {
	conditions, _ := value(cluster, "status", "conditions").([]any)
	for _, raw := range conditions {
		c, _ := raw.(object)
		if text(c, "type") == condition && text(c, "status") == "True" {
			return true
		}
	}
	return false
}

// cnpgInstanceID rejects foreign, zero and noncanonical instance serials.
func cnpgInstanceID(name, cluster string) (uint64, error) {
	if !strings.HasPrefix(name, cluster+"-") {
		return 0, errors.New("instance is outside the selected database")
	}
	suffix := strings.TrimPrefix(name, cluster+"-")
	id, err := strconv.ParseUint(suffix, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != suffix {
		return 0, errors.New("database instance identity is not canonical")
	}
	return id, nil
}

// healthyCNPGPods binds the three stable owned instances from a complete Pod
// inventory. It establishes neither maintenance exclusivity nor storage clearance.
func healthyCNPGPods(cluster object, allPods []object) ([]object, error) {
	primary := text(cluster, "status", "currentPrimary")
	name, ns := text(cluster, "metadata", "name"), text(cluster, "metadata", "namespace")
	if !knownCNPGSource(ns, name) || ns == "" || text(cluster, "metadata", "uid") == "" || text(cluster, "metadata", "resourceVersion") == "" || value(cluster, "metadata", "deletionTimestamp") != nil ||
		value(cluster, "spec", "instances") != float64(3) || value(cluster, "status", "instances") != float64(3) || value(cluster, "status", "readyInstances") != float64(3) || !cnpgCondition(cluster, "Ready") || !cnpgCondition(cluster, "ContinuousArchiving") || primary == "" || primary != text(cluster, "status", "targetPrimary") {
		return nil, errors.New("the selected database is not stable, archived and ready")
	}
	if fence := text(cluster, "metadata", "annotations", "cnpg.io/fencedInstances"); fence != "" && fence != "[]" {
		return nil, errors.New("database already has a maintenance fence")
	}
	healthy, _ := value(cluster, "status", "instancesStatus", "healthy").([]any)
	known := map[string]bool{}
	for _, entry := range healthy {
		instance, ok := entry.(string)
		if !ok || known[instance] {
			return nil, errors.New("database healthy-instance inventory is ambiguous")
		}
		if _, err := cnpgInstanceID(instance, name); err != nil {
			return nil, err
		}
		known[instance] = true
	}
	if len(known) != 3 || !known[primary] {
		return nil, errors.New("three current healthy database instances are required")
	}
	pods := []object{}
	for _, pod := range allPods {
		if text(pod, "metadata", "namespace") == ns && (text(pod, "metadata", "labels", "cnpg.io/cluster") == name || cnpgOwnerReference(pod, cluster)) {
			pods = append(pods, pod)
		}
	}
	if len(pods) != 3 {
		return nil, errors.New("database pod inventory does not match the healthy instances")
	}
	byName, err := indexed(pods)
	if err != nil {
		return nil, err
	}
	instances := make([]string, 0, 3)
	for instance := range known {
		instances = append(instances, instance)
	}
	sort.Slice(instances, func(i, j int) bool {
		a, _ := cnpgInstanceID(instances[i], name)
		b, _ := cnpgInstanceID(instances[j], name)
		return a < b
	})
	ordered := make([]object, 0, 3)
	for _, instance := range instances {
		pod := byName[instance]
		status, _ := value(pod, "status").(object)
		role := "replica"
		if instance == primary {
			role = "primary"
		}
		if !cnpgOwned(pod, cluster) || value(pod, "metadata", "deletionTimestamp") != nil || text(pod, "status", "phase") != "Running" || !ready(status) || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != role {
			return nil, errors.New("database pod ownership, health or primary role changed")
		}
		containers, _ := value(pod, "spec", "containers").([]any)
		postgres := 0
		for _, raw := range containers {
			c, _ := raw.(object)
			if text(c, "name") == "postgres" {
				postgres++
				if text(c, "image") == "" || text(c, "image") != text(cluster, "spec", "imageName") {
					return nil, errors.New("database operand image is changing")
				}
			}
		}
		if postgres != 1 {
			return nil, errors.New("database PostgreSQL executable is ambiguous")
		}
		ordered = append(ordered, pod)
	}
	return ordered, nil
}
