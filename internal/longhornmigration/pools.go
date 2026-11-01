// Package longhornmigration binds temporary replica storage to the reviewed
// fixed-size pools and their owned Kubernetes scale-down protection.
package longhornmigration

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

type Object = map[string]any

const (
	PoolLabel         = "storage.platform.devantler.tech/migration-pool"
	EnabledLabel      = "storage.platform.devantler.tech/migration-enabled"
	OwnerAnnotation   = "storage.platform.devantler.tech/migration-capacity"
	OwnerValue        = "longhorn-v2-5582"
	ScaleDownDisabled = "cluster-autoscaler.kubernetes.io/scale-down-disabled"
)

type Pool struct {
	Name, Role, ServerType string
	Count                  int
}

func value(o Object, path ...string) any {
	var v any = o
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func text(o Object, path ...string) string { s, _ := value(o, path...).(string); return s }

func ready(o Object) bool {
	conditions, _ := value(o, "status", "conditions").([]any)
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if ok && text(c, "type") == "Ready" && text(c, "status") == "True" {
			return true
		}
	}
	return false
}

// The reviewed hardware and location of both temporary migration pools. A
// different value changes the capacity this migration was measured against.
const (
	reviewedServerType = "cpx42"
	reviewedLocation   = "fsn1"
)

func ConfiguredPools(data []byte) (map[string]Pool, error) {
	b, err := yaml.YAMLToJSONStrict(data)
	var cfg Object
	if err != nil || json.Unmarshal(b, &cfg) != nil || value(cfg, "spec", "cluster", "controlPlanes") != float64(3) || value(cfg, "spec", "cluster", "workers") != float64(3) ||
		value(cfg, "spec", "cluster", "autoscaler", "node", "maxNodesTotal") != float64(9) || value(cfg, "spec", "provider", "hetzner", "serverLimit") != float64(10) {
		return nil, errors.New("reviewed migration capacity and reserved account headroom are unresolved")
	}
	result := map[string]Pool{}
	pools, _ := value(cfg, "spec", "cluster", "autoscaler", "node", "pools").([]any)
	for _, raw := range pools {
		p, ok := raw.(map[string]any)
		name := text(p, "name")
		if !strings.HasPrefix(name, "autoscale-longhorn-") {
			continue
		}
		role := strings.TrimPrefix(name, "autoscale-longhorn-")
		var count int
		switch role {
		case "v1":
			count = 2
		case "v2":
			count = 1
		default:
			return nil, errors.New("unknown pool uses the reserved migration namespace")
		}
		if !ok || result[role].Name != "" || value(p, "min") != float64(count) || value(p, "max") != float64(count) || text(p, "labels", PoolLabel) != role || text(p, "serverType") != reviewedServerType || text(p, "location") != reviewedLocation {
			return nil, errors.New("migration pools must have their reviewed fixed sizes, hardware, location and role labels")
		}
		result[role] = Pool{Name: name, Role: role, ServerType: text(p, "serverType"), Count: count}
	}
	if len(result) != 2 {
		return nil, errors.New("both reviewed migration pools are required")
	}
	return result, nil
}

func VerifyNode(n Object, pool Pool) error {
	if !regexp.MustCompile("^"+regexp.QuoteMeta(pool.Name)+"-[0-9a-f]{16}$").MatchString(text(n, "metadata", "name")) ||
		text(n, "metadata", "labels", PoolLabel) != pool.Role || text(n, "metadata", "labels", "ksail.io/autoscaled") != "true" ||
		text(n, "metadata", "labels", "node.kubernetes.io/instance-type") != pool.ServerType || !ready(n) ||
		text(n, "metadata", "uid") == "" || text(n, "metadata", "resourceVersion") == "" || value(n, "metadata", "deletionTimestamp") != nil ||
		!regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(text(n, "spec", "providerID")) || text(n, "status", "nodeInfo", "systemUUID") == "" ||
		value(n, "spec", "unschedulable") == true {
		return errors.New("ready migration-pool node identity is not proven")
	}
	return nil
}
