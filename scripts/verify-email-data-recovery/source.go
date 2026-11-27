package main

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strings"
)

var kubernetesName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

const (
	servingClaim        = "email-data-encrypted-v2"
	servingStorageClass = "email-encrypted-v2"
	servingVolume       = "v2-data"
	servingStorePath    = "/var/lib/stalwart-v2"
)

// Both serving profiles use the reviewed bundle. Bind runtime identities in a
// fixed name order so source retention detects replacement and restart drift.
func servingProfile(pod object, expectedImage string) ([2]sourceContainer, error) {
	var binding [2]sourceContainer
	fail := func() ([2]sourceContainer, error) { return binding, errors.New("serving-profile") }
	containers, statuses := items(child(pod, "spec"), "containers"), items(child(pod, "status"), "containerStatuses")
	if expectedImage == "" || len(containers) < 1 || len(containers) > 2 || len(statuses) != len(containers) {
		return fail()
	}
	specs, observed := map[string]object{}, map[string]object{}
	for _, container := range containers {
		name := text(container, "name")
		if (name != "private-mail" && name != "client-proxy") || specs[name] != nil || text(container, "image") != expectedImage {
			return fail()
		}
		specs[name] = container
	}
	if specs["private-mail"] == nil {
		return fail()
	}
	for _, status := range statuses {
		name := text(status, "name")
		if specs[name] == nil || observed[name] != nil || status["ready"] != true || text(status, "imageID") == "" {
			return fail()
		}
		observed[name] = status
	}
	for index, name := range []string{"private-mail", "client-proxy"} {
		if specs[name] == nil {
			continue
		}
		restarts, ok := observed[name]["restartCount"].(float64)
		if !ok || restarts < 0 || restarts > 1<<31-1 || restarts != float64(int64(restarts)) {
			return fail()
		}
		binding[index] = sourceContainer{image: text(specs[name], "image"), runtime: text(observed[name], "imageID"), restarts: int64(restarts)}
	}
	return binding, nil
}

func proveServingSource(pod, pvc, pv object, expectedImage string) (sourceIdentity, error) {
	pm, cm, vm := child(pod, "metadata"), child(pvc, "metadata"), child(pv, "metadata")
	fail := func() (sourceIdentity, error) { return sourceIdentity{}, errors.New("serving-source") }
	if text(pm, "name") != "private-mail-0" || text(pm, "namespace") != "private-mail" || text(pm, "uid") == "" || pm["deletionTimestamp"] != nil || text(cm, "name") != servingClaim || text(cm, "namespace") != "private-mail" || text(cm, "uid") == "" || cm["deletionTimestamp"] != nil || text(vm, "uid") == "" || vm["deletionTimestamp"] != nil {
		return fail()
	}
	if text(child(pm, "labels"), "statefulset.kubernetes.io/pod-name") != "private-mail-0" {
		return fail()
	}
	owners := items(pm, "ownerReferences")
	if len(owners) != 1 || text(owners[0], "kind") != "StatefulSet" || text(owners[0], "name") != "private-mail" || text(owners[0], "uid") == "" || owners[0]["controller"] != true {
		return fail()
	}
	for key := range child(pm, "annotations") {
		if strings.Contains(key, "hook.backup.velero.io/") {
			return fail()
		}
	}
	ready := false
	for _, condition := range items(child(pod, "status"), "conditions") {
		if text(condition, "type") == "Ready" && text(condition, "status") == "True" {
			ready = true
		}
	}
	containers, err := servingProfile(pod, expectedImage)
	if !ready || text(child(pod, "status"), "phase") != "Running" || err != nil {
		return fail()
	}
	found := false
	for _, volume := range items(child(pod, "spec"), "volumes") {
		if volume["persistentVolumeClaim"] == nil {
			continue
		}
		claim := child(volume, "persistentVolumeClaim")
		if found || text(volume, "name") != servingVolume || text(claim, "claimName") != servingClaim ||
			(claim["readOnly"] != nil && claim["readOnly"] != false) {
			return fail()
		}
		found = true
	}
	cs, vs := child(pvc, "spec"), child(pv, "spec")
	name := text(cs, "volumeName")
	claimRef, csi := child(vs, "claimRef"), child(vs, "csi")
	if !found || !privateMailServingMount(pod) || !kubernetesName.MatchString(name) || text(cs, "storageClassName") != servingStorageClass ||
		text(vs, "storageClassName") != servingStorageClass || text(child(child(cs, "resources"), "requests"), "storage") != "2Gi" ||
		text(child(pvc, "status"), "phase") != "Bound" || text(vm, "name") != name || text(child(pv, "status"), "phase") != "Bound" ||
		text(claimRef, "uid") != text(cm, "uid") || text(claimRef, "namespace") != "private-mail" || text(claimRef, "name") != servingClaim ||
		text(csi, "driver") != "driver.longhorn.io" || text(csi, "volumeHandle") != name {
		return fail()
	}
	reclaim := text(vs, "persistentVolumeReclaimPolicy")
	attributes := child(csi, "volumeAttributes")
	if reclaim != "Retain" || text(attributes, "dataEngine") != "v2" || text(attributes, "encrypted") != "true" ||
		text(attributes, "numberOfReplicas") != "3" {
		return fail()
	}
	return sourceIdentity{podUID: text(pm, "uid"), claimUID: text(cm, "uid"), volume: servingVolume, pv: name,
		claimSize: "2Gi", pvUID: text(vm, "uid"), reclaim: reclaim, containers: containers}, nil
}

func privateMailServingMount(pod object) bool {
	for _, server := range items(child(pod, "spec"), "containers") {
		if text(server, "name") != "private-mail" {
			continue
		}
		found := false
		for _, mount := range items(server, "volumeMounts") {
			location := text(mount, "mountPath")
			if !strings.HasPrefix(location, "/") || path.Clean(location) != location {
				return false
			}
			if text(mount, "name") == servingVolume {
				if found || location != servingStorePath || (mount["readOnly"] != nil && mount["readOnly"] != false) ||
					mount["subPath"] != nil || mount["subPathExpr"] != nil {
					return false
				}
				found = true
			} else if location == "/" || location == servingStorePath ||
				strings.HasPrefix(location, servingStorePath+"/") || strings.HasPrefix(servingStorePath, location+"/") {
				return false
			}
		}
		return found
	}
	return false
}
func (d *recoveryDriver) readSource(ctx context.Context) (sourceIdentity, error) {
	pod, found, err := d.lookup(ctx, "Pod", "private-mail-0", "private-mail")
	if err != nil || !found {
		return sourceIdentity{}, errors.New("source-pod")
	}
	if d.image == "" {
		d.image, err = loadRecoveryImage()
		if err != nil {
			return sourceIdentity{}, err
		}
	}
	pvc, found, err := d.lookup(ctx, "PersistentVolumeClaim", servingClaim, "private-mail")
	if err != nil || !found {
		return sourceIdentity{}, errors.New("source-claim")
	}
	name := text(child(pvc, "spec"), "volumeName")
	if !kubernetesName.MatchString(name) {
		return sourceIdentity{}, errors.New("source-volume-name")
	}
	pv, found, err := d.lookup(ctx, "PersistentVolume", name, "")
	if err != nil || !found {
		return sourceIdentity{}, errors.New("source-volume")
	}
	return proveServingSource(pod, pvc, pv, d.image)
}
func (d *recoveryDriver) sourceRetained(ctx context.Context) error {
	actual, err := d.readSource(ctx)
	if err != nil || actual != d.source {
		return errors.New("source-retention")
	}
	return nil
}
