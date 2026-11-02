package namespaceinventory

import (
	"context"
	"errors"
	"sort"
	"strings"
)

type servedResource struct{ prefix, name, kind string }
type discovery struct {
	versions  []string
	resources []servedResource
}
type versionsResponse struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Versions   []string `json:"versions"`
}
type groupVersion struct {
	GroupVersion string `json:"groupVersion"`
	Version      string `json:"version"`
}
type groupsResponse struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Groups     []struct {
		Name     string         `json:"name"`
		Versions []groupVersion `json:"versions"`
	} `json:"groups"`
}
type resourcesResponse struct {
	APIVersion   string `json:"apiVersion"`
	Kind         string `json:"kind"`
	GroupVersion string `json:"groupVersion"`
	Resources    []struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		Namespaced *bool    `json:"namespaced"`
		Verbs      []string `json:"verbs"`
	} `json:"resources"`
}

var discoveryFields = map[string][]string{
	"":                    {"apiVersion", "kind", "versions", "groups", "groupVersion", "resources"},
	"groups[]":            {"name", "versions"},
	"groups[].versions[]": {"groupVersion", "version"},
	"resources[]":         {"name", "kind", "namespaced", "verbs"},
}

func (r *reader) discovery(ctx context.Context) (discovery, error) {
	result := discovery{}
	raw, err := r.get(ctx, "/api", "application/json")
	if err != nil {
		return result, err
	}
	var core versionsResponse
	if decode(raw, &core, discoveryFields) != nil || core.APIVersion != "v1" || core.Kind != "APIVersions" || len(core.Versions) == 0 {
		return result, errors.New("core API discovery is incomplete")
	}
	prefixes := map[string]string{}
	for _, version := range core.Versions {
		if !versionPattern.MatchString(version) || len(version) > 63 || prefixes[version] != "" {
			return discovery{}, errors.New("core API versions are ambiguous")
		}
		prefixes[version] = "/api/" + version
	}
	raw, err = r.get(ctx, "/apis", "application/json")
	if err != nil {
		return discovery{}, err
	}
	var groups groupsResponse
	if decode(raw, &groups, discoveryFields) != nil || groups.APIVersion != "v1" || groups.Kind != "APIGroupList" || groups.Groups == nil {
		return discovery{}, errors.New("group API discovery is incomplete")
	}
	seenGroups := map[string]bool{}
	for _, group := range groups.Groups {
		if !groupPattern.MatchString(group.Name) || len(group.Name) > 253 || seenGroups[group.Name] || len(group.Versions) == 0 {
			return discovery{}, errors.New("served API group is ambiguous")
		}
		seenGroups[group.Name] = true
		for _, version := range group.Versions {
			if !versionPattern.MatchString(version.Version) || len(version.Version) > 63 || version.GroupVersion != group.Name+"/"+version.Version || prefixes[version.GroupVersion] != "" {
				return discovery{}, errors.New("served API version is ambiguous")
			}
			prefixes[version.GroupVersion] = "/apis/" + version.GroupVersion
		}
	}
	for version := range prefixes {
		result.versions = append(result.versions, version)
	}
	sort.Strings(result.versions)
	for _, version := range result.versions {
		raw, err = r.get(ctx, prefixes[version], "application/json")
		if err != nil {
			return discovery{}, err
		}
		var resources resourcesResponse
		if decode(raw, &resources, discoveryFields) != nil || resources.APIVersion != "v1" || resources.Kind != "APIResourceList" || resources.GroupVersion != version || resources.Resources == nil {
			return discovery{}, errors.New("served resource discovery is incomplete")
		}
		seen := map[string]bool{}
		for _, resource := range resources.Resources {
			if resource.Name == "" || len(resource.Name) > 253 || seen[resource.Name] || resource.Kind == "" || resource.Verbs == nil || resource.Namespaced == nil {
				return discovery{}, errors.New("served resource definition is ambiguous")
			}
			seen[resource.Name] = true
			parts := strings.Split(resource.Name, "/")
			if len(parts) > 2 {
				return discovery{}, errors.New("served resource path is invalid")
			}
			for _, part := range parts {
				if !namePattern.MatchString(part) {
					return discovery{}, errors.New("served resource name is invalid")
				}
			}
			if len(parts) == 2 {
				continue
			}
			verbs := map[string]bool{}
			for _, verb := range resource.Verbs {
				if verbs[verb] || verb == "" {
					return discovery{}, errors.New("served resource verbs are ambiguous")
				}
				verbs[verb] = true
			}
			if !*resource.Namespaced || !verbs["delete"] {
				continue
			}
			if !verbs["list"] {
				return discovery{}, errors.New("a namespace-deletable resource cannot be inventoried")
			}
			result.resources = append(result.resources, servedResource{prefixes[version], resource.Name, resource.Kind})
		}
	}
	sort.Slice(result.resources, func(a, b int) bool {
		if result.resources[a].prefix == result.resources[b].prefix {
			return result.resources[a].name < result.resources[b].name
		}
		return result.resources[a].prefix < result.resources[b].prefix
	})
	return result, nil
}
