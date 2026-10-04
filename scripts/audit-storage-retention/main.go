package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Report struct {
	Status                 string `json:"status"`
	Classes                int    `json:"classes"`
	Volumes                int    `json:"volumes"`
	DeleteClasses          int    `json:"deleteClasses"`
	DeleteVolumes          int    `json:"deleteVolumes"`
	TemporaryVolumes       int    `json:"temporaryVolumes"`
	ReleasedVolumes        int    `json:"releasedVolumes"`
	OverdueReleasedVolumes int    `json:"overdueReleasedVolumes"`
}

type Reference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Namespace  string `json:"namespace"`
	Controller bool   `json:"controller"`
}

type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string      `json:"name"`
		UID             string      `json:"uid"`
		Namespace       string      `json:"namespace"`
		OwnerReferences []Reference `json:"ownerReferences"`
	} `json:"metadata"`
	ReclaimPolicy string `json:"reclaimPolicy"`
	Spec          struct {
		Policy     string    `json:"persistentVolumeReclaimPolicy"`
		Class      string    `json:"storageClassName"`
		VolumeName string    `json:"volumeName"`
		ClaimRef   Reference `json:"claimRef"`
		DataSource struct {
			APIGroup string `json:"apiGroup"`
			Kind     string `json:"kind"`
			Name     string `json:"name"`
		} `json:"dataSource"`
	} `json:"spec"`
	Status struct {
		Phase     string `json:"phase"`
		PhaseTime string `json:"lastPhaseTransitionTime"`
	} `json:"status"`
}

func readJSON(dir, name string, value any) error {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return fmt.Errorf("%s is unreadable", name)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%s is invalid JSON", name)
	}
	return nil
}

func readList(dir, name, api, kind string, namespaced bool) (map[string]Object, error) {
	var list struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Version  string `json:"resourceVersion"`
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []Object `json:"items"`
	}
	if err := readJSON(dir, name, &list); err != nil {
		return nil, err
	}
	if list.APIVersion != api || list.Kind != kind+"List" || list.Metadata.Version == "" || list.Metadata.Continue != "" || list.Items == nil {
		return nil, fmt.Errorf("%s is not a complete versioned %s list", name, kind)
	}
	objects := map[string]Object{}
	uids := map[string]bool{}
	for _, item := range list.Items {
		// Core typed API lists omit item type headers; only inherit from an exact typed list.
		if item.Kind == "" {
			item.Kind = kind
		}
		if item.APIVersion == "" {
			item.APIVersion = api
		}
		if item.Kind != kind || item.APIVersion != api || item.Metadata.Name == "" || item.Metadata.UID == "" || (namespaced && item.Metadata.Namespace == "") || (!namespaced && item.Metadata.Namespace != "") {
			return nil, fmt.Errorf("%s has an incomplete resource identity", name)
		}
		key := item.Metadata.Namespace + "/" + item.Metadata.Name
		if _, exists := objects[key]; exists || uids[item.Metadata.UID] {
			return nil, fmt.Errorf("%s has duplicate resource identities", name)
		}
		objects[key] = item
		uids[item.Metadata.UID] = true
	}
	return objects, nil
}

func temporaryVolume(pv Object, claims, uploads map[string]Object) (bool, error) {
	ref := pv.Spec.ClaimRef
	claim, exists := claims[ref.Namespace+"/"+ref.Name]
	if !exists || ref.UID == "" || claim.Metadata.UID != ref.UID || claim.Spec.VolumeName != pv.Metadata.Name || claim.Status.Phase != "Bound" || claim.Spec.Class != pv.Spec.Class {
		return false, errors.New("bound volume and claim identities do not join; recapture")
	}
	if ref.Namespace != "velero" {
		return false, nil
	}
	for _, owner := range claim.Metadata.OwnerReferences {
		if owner.Kind != "DataUpload" || owner.APIVersion != "velero.io/v2alpha1" || !owner.Controller {
			continue
		}
		upload, exists := uploads[ref.Namespace+"/"+owner.Name]
		if !exists || owner.UID == "" || upload.Metadata.UID != owner.UID {
			return false, errors.New("backup owner identity does not join; recapture")
		}
		if claim.Metadata.Name != owner.Name || claim.Spec.DataSource.Kind != "VolumeSnapshot" || claim.Spec.DataSource.APIGroup != "snapshot.storage.k8s.io" || claim.Spec.DataSource.Name == "" {
			return false, errors.New("backup claim does not match the pinned CSI exposer shape")
		}
		return true, nil
	}
	return false, nil
}

func audit(dir string, now time.Time, threshold time.Duration) (report Report, err error) {
	report.Status = "UNKNOWN"
	if threshold <= 0 {
		return report, errors.New("released threshold must be positive")
	}
	var receipt struct {
		Started   string `json:"startedAt"`
		Completed string `json:"completedAt"`
	}
	if err := readJSON(dir, "capture.json", &receipt); err != nil {
		return report, err
	}
	start, err := time.Parse(time.RFC3339, receipt.Started)
	if err != nil {
		return report, errors.New("invalid capture start")
	}
	end, err := time.Parse(time.RFC3339, receipt.Completed)
	if err != nil {
		return report, errors.New("invalid capture completion")
	}
	if end.Before(start) || end.After(now) || now.Sub(start) > 5*time.Minute {
		return report, errors.New("capture is stale, incomplete or from the future; recapture")
	}
	classes, err := readList(dir, "classes.json", "storage.k8s.io/v1", "StorageClass", false)
	if err != nil {
		return report, err
	}
	volumes, err := readList(dir, "volumes.json", "v1", "PersistentVolume", false)
	if err != nil {
		return report, err
	}
	claims, err := readList(dir, "claims.json", "v1", "PersistentVolumeClaim", true)
	if err != nil {
		return report, err
	}
	uploads, err := readList(dir, "uploads.json", "velero.io/v2alpha1", "DataUpload", true)
	if err != nil {
		return report, err
	}
	report.Classes = len(classes)
	report.Volumes = len(volumes)
	if report.Classes == 0 || report.Volumes == 0 {
		return report, errors.New("empty storage census cannot establish retention coverage")
	}
	for _, class := range classes {
		switch class.ReclaimPolicy {
		case "Retain":
		case "Delete":
			report.DeleteClasses++
		default:
			return report, errors.New("missing or unsupported class reclaim policy")
		}
	}
	for _, volume := range volumes {
		if volume.Spec.Policy != "Retain" && volume.Spec.Policy != "Delete" {
			return report, errors.New("missing or unsupported volume reclaim policy")
		}
		temporary := false
		switch volume.Status.Phase {
		case "Bound":
			if volume.Spec.Class != "" {
				if _, exists := classes["/"+volume.Spec.Class]; !exists {
					return report, errors.New("bound volume class is missing; recapture")
				}
			}
			temporary, err = temporaryVolume(volume, claims, uploads)
			if err != nil {
				return report, err
			}
		case "Released":
			report.ReleasedVolumes++
			transition, err := time.Parse(time.RFC3339, volume.Status.PhaseTime)
			if err != nil || transition.After(end) {
				return report, errors.New("released volume age is unknown")
			}
			if end.Sub(transition) >= threshold {
				report.OverdueReleasedVolumes++
			}
		case "Available":
		case "Pending", "Failed":
			return report, errors.New("volume phase prevents a complete retention audit")
		default:
			return report, errors.New("missing or unsupported volume phase")
		}
		if temporary {
			report.TemporaryVolumes++
		} else if volume.Spec.Policy == "Delete" {
			report.DeleteVolumes++
		}
	}
	report.Status = "CLEAN"
	if report.DeleteClasses+report.DeleteVolumes+report.OverdueReleasedVolumes > 0 {
		report.Status = "WARNING"
	}
	return report, nil
}

func main() {
	dir := flag.String("snapshot-dir", "", "private capture directory")
	threshold := flag.Duration("released-threshold", 24*time.Hour, "age at which Released volumes warn")
	flag.Parse()
	if *dir == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "--snapshot-dir is required; no cluster writes are performed")
		os.Exit(2)
	}
	report, err := audit(*dir, time.Now().UTC(), *threshold)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if report.Status != "CLEAN" {
		os.Exit(1)
	}
}
