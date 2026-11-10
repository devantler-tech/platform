package namespaceinventory

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

type serviceRegistration struct {
	Name, UID, Version, ResourceVersion string
	Remote                              bool
}
type apiServiceList struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   metadata        `json:"metadata"`
	Items      json.RawMessage `json:"items"`
}
type apiService struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		Group   string `json:"group"`
		Version string `json:"version"`
		Service *struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"service"`
	} `json:"spec"`
}

var coverageFields = map[string][]string{
	"":                     {"apiVersion", "kind", "metadata", "items"},
	"metadata":             {"resourceVersion", "continue", "remainingItemCount"},
	"items[]":              {"apiVersion", "kind", "metadata", "spec"},
	"items[].metadata":     {"name", "namespace", "uid", "resourceVersion"},
	"items[].spec":         {"group", "version", "service"},
	"items[].spec.service": {"name", "namespace"},
}

// Native VAP admission covers generic built-in/CRD storage, including each
// member of DELETECollection. A remotely aggregated API backend is outside that
// guarantee; do not turn its metadata observation into deletion clearance.
func (r *reader) nativeCoverage(ctx context.Context, resources discovery) ([]serviceRegistration, error) {
	raw, err := r.get(ctx, "/apis/apiregistration.k8s.io/v1/apiservices", "application/json")
	if err != nil {
		return nil, err
	}
	var list apiServiceList
	if decode(raw, &list, coverageFields) != nil || list.APIVersion != "apiregistration.k8s.io/v1" || list.Kind != "APIServiceList" || list.Metadata.ResourceVersion == "" || list.Metadata.Continue != "" || list.Metadata.Remaining != nil && *list.Metadata.Remaining != 0 || list.Items == nil {
		return nil, errors.New("APIService coverage roster is incomplete")
	}
	var items []apiService
	if json.Unmarshal(list.Items, &items) != nil {
		return nil, errors.New("APIService coverage items are malformed")
	}
	required := map[string]bool{}
	for _, resource := range resources.resources {
		version := strings.TrimPrefix(resource.prefix, "/apis/")
		if strings.HasPrefix(resource.prefix, "/api/") {
			version = strings.TrimPrefix(resource.prefix, "/api/")
		}
		required[version] = true
	}
	result := make([]serviceRegistration, 0, len(items))
	seen, seenUID := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		group, version := item.Spec.Group, item.Spec.Version
		if !versionPattern.MatchString(version) || len(version) > 63 || group != "" && (!groupPattern.MatchString(group) || len(group) > 253) || item.Metadata.Name != version+"."+group || item.Metadata.Namespace != "" || item.Metadata.UID == "" || item.Metadata.ResourceVersion == "" || seen[item.Metadata.Name] || seenUID[item.Metadata.UID] {
			return nil, errors.New("APIService registration identity is ambiguous")
		}
		seen[item.Metadata.Name], seenUID[item.Metadata.UID] = true, true
		gv := version
		if group != "" {
			gv = group + "/" + version
		}
		remote := item.Spec.Service != nil
		if remote && (item.Spec.Service.Name == "" || item.Spec.Service.Namespace == "") {
			return nil, errors.New("aggregated API service binding is incomplete")
		}
		if remote && required[gv] {
			return nil, errors.New("namespace deletion admission is unverified for a remote API backend")
		}
		result = append(result, serviceRegistration{item.Metadata.Name, item.Metadata.UID, gv, item.Metadata.ResourceVersion, remote})
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Name < result[b].Name })
	return result, nil
}
