// Package portalstorage validates the fixed portal's retained storage identities.
package portalstorage

import (
	"errors"
	"regexp"
	"sort"
)

// Binding is a reciprocally bound claim and its persistent volume.
type Binding struct {
	Claim, Volume map[string]any
}

// ValidateBindings joins complete claim and volume censuses without changing them.
func ValidateBindings(clusterUID string, claims, volumes map[string]any, requireRetain bool) ([]Binding, error) {
	if clusterUID == "" {
		return nil, errors.New("database identity missing")
	}
	cs, err := list(claims, "PersistentVolumeClaim")
	if err != nil || len(cs) == 0 {
		return nil, errors.New("claim census incomplete")
	}
	vs, err := list(volumes, "PersistentVolume")
	if err != nil {
		return nil, errors.New("volume census incomplete")
	}
	byName := map[string]map[string]any{}
	for _, v := range vs {
		if scope := at(v, "metadata", "namespace"); scope != nil && scope != "" {
			return nil, errors.New("volume scope unverified")
		}
		byName[str(at(v, "metadata", "name"))] = v
	}
	used := map[string]bool{}
	claimUIDs := map[string]bool{}
	bindings := make([]Binding, 0, len(cs))
	for _, c := range cs {
		owners, ok := at(c, "metadata", "ownerReferences").([]any)
		if at(c, "metadata", "namespace") != "backstage" || !ok || len(owners) != 1 {
			return nil, errors.New("claim ownership unverified")
		}
		o := obj(owners[0])
		if o["apiVersion"] != "postgresql.cnpg.io/v1" || o["kind"] != "Cluster" || o["name"] != "backstage-db" || o["uid"] != clusterUID || o["controller"] != true {
			return nil, errors.New("claim database ownership unverified")
		}
		name := str(at(c, "spec", "volumeName"))
		class := str(at(c, "spec", "storageClassName"))
		v := byName[name]
		if name == "" || class == "" || v == nil || used[name] || at(v, "metadata", "deletionTimestamp") != nil || at(c, "status", "phase") != "Bound" || at(v, "status", "phase") != "Bound" || at(v, "spec", "storageClassName") != class {
			return nil, errors.New("claim volume binding unverified")
		}
		ref := obj(at(v, "spec", "claimRef"))
		if ref["apiVersion"] != "v1" || ref["kind"] != "PersistentVolumeClaim" || ref["namespace"] != "backstage" || ref["name"] != at(c, "metadata", "name") || ref["uid"] != at(c, "metadata", "uid") {
			return nil, errors.New("reciprocal claim identity unverified")
		}
		policy := at(v, "spec", "persistentVolumeReclaimPolicy")
		if policy != "Retain" && (requireRetain || policy != "Delete") {
			return nil, errors.New("physical retention unverified")
		}
		used[name] = true
		claimUIDs[str(at(c, "metadata", "uid"))] = true
		bindings = append(bindings, Binding{Claim: c, Volume: v})
	}
	for name, v := range byName {
		if (at(v, "spec", "claimRef", "namespace") == "backstage" || claimUIDs[str(at(v, "spec", "claimRef", "uid"))]) && !used[name] {
			return nil, errors.New("volume reverse census differs")
		}
	}
	sort.Slice(bindings, func(i, j int) bool {
		return str(at(bindings[i].Claim, "metadata", "name")) < str(at(bindings[j].Claim, "metadata", "name"))
	})
	return bindings, nil
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string         { s, _ := v.(string); return s }
func at(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		v = obj(v)[k]
	}
	return v
}

func list(l map[string]any, kind string) ([]map[string]any, error) {
	items, ok := l["items"].([]any)
	remaining := at(l, "metadata", "remainingItemCount")
	if l["apiVersion"] != "v1" || l["kind"] != "List" && l["kind"] != kind+"List" || !ok || len(items) > 512 || at(l, "metadata", "continue") != nil && at(l, "metadata", "continue") != "" || remaining != nil && remaining != 0 && remaining != float64(0) {
		return nil, errors.New("incomplete storage list")
	}
	result := make([]map[string]any, 0, len(items))
	names, uids := map[string]bool{}, map[string]bool{}
	for _, raw := range items {
		r := obj(raw)
		name, uid := str(at(r, "metadata", "name")), str(at(r, "metadata", "uid"))
		if r["apiVersion"] != "v1" || r["kind"] != kind || len(name) > 253 || !regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`).MatchString(name) || uid == "" || str(at(r, "metadata", "resourceVersion")) == "" || names[name] || uids[uid] {
			return nil, errors.New("incomplete or duplicate storage identity")
		}
		// Foreign volumes may be terminating; only the bound targets need a
		// non-terminating identity. Claims are all fixed-namespace targets.
		if kind == "PersistentVolumeClaim" && at(r, "metadata", "deletionTimestamp") != nil {
			return nil, errors.New("claim is terminating")
		}
		names[name], uids[uid] = true, true
		result = append(result, r)
	}
	return result, nil
}
