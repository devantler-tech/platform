package namespaceinventory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Binding is the identity of one observed object.
type Binding struct{ Name, Namespace, UID, Kind, Resource string }

// Inventory holds the observed bindings. They remain private until a caller
// explicitly copies them into its private journal. Default JSON serialization
// never publishes object identities.
type Inventory struct{ bindings map[string]Binding }

// Bindings returns a copy of the observed bindings ordered by UID.
func (i Inventory) Bindings() []Binding {
	result := make([]Binding, 0, len(i.bindings))
	for _, binding := range i.bindings {
		result = append(result, binding)
	}
	sort.Slice(result, func(a, b int) bool { return result[a].UID < result[b].UID })
	return result
}

type metadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Continue        string `json:"continue"`
	Remaining       *int64 `json:"remainingItemCount"`
}
type partialObject struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
}
type partialList struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   metadata        `json:"metadata"`
	Items      json.RawMessage `json:"items"`
}

func decode(raw []byte, target any, fields map[string][]string) error {
	if strictjson.Validate(raw, fields) != nil || json.Unmarshal(raw, target) != nil {
		return errors.New("namespace evidence is incomplete or ambiguous")
	}
	return nil
}

var metadataFields = map[string][]string{
	"":                 {"apiVersion", "kind", "metadata", "items"},
	"metadata":         {"name", "namespace", "uid", "resourceVersion", "continue", "remainingItemCount"},
	"items[]":          {"apiVersion", "kind", "metadata"},
	"items[].metadata": {"name", "namespace", "uid", "resourceVersion"},
}

func (r *reader) namespaceCurrent(ctx context.Context) error {
	raw, err := r.get(ctx, "/api/v1/namespaces/"+r.namespace, metadataObjectAccept)
	if err != nil {
		return err
	}
	var actual partialObject
	if decode(raw, &actual, metadataFields) != nil || actual.APIVersion != "meta.k8s.io/v1" || actual.Kind != "PartialObjectMetadata" || actual.Metadata.Name != r.namespace || actual.Metadata.Namespace != "" || actual.Metadata.UID != r.uid || actual.Metadata.ResourceVersion == "" {
		return errors.New("namespace identity is unverified or changed")
	}
	return nil
}

// Observe uses the caller's reviewed authenticated HTTP transport, fixed HTTPS
// API root and exact namespace UID. It never invokes a credential plugin or
// chooses a context. Bootstrap credential validation belongs to the caller.
func Observe(ctx context.Context, client *http.Client, endpoint *url.URL, namespace, uid string) (Inventory, error) {
	if !namespacePattern.MatchString(namespace) {
		return Inventory{}, errors.New("a receipted benchmark namespace is required")
	}
	return observeNamespace(ctx, client, endpoint, namespace, uid)
}

// CalibrateLonghornSystem is a fixed read-only production diagnostic. Its
// inventory never authorizes retiring this namespace or any of its resources.
func CalibrateLonghornSystem(ctx context.Context, client *http.Client, endpoint *url.URL, uid string) (Inventory, error) {
	result, err := observeNamespace(ctx, client, endpoint, "longhorn-system", uid)
	if err != nil {
		return Inventory{}, err
	}
	if len(result.bindings) == 0 {
		return Inventory{}, errors.New("longhorn metadata observation is empty and unverified")
	}
	return result, nil
}

// ObserveRetainedEmailRecovery inventories only the fixed retained diagnostic
// copy. A complete inventory alone never authorizes removing any resource.
func ObserveRetainedEmailRecovery(ctx context.Context, client *http.Client, endpoint *url.URL, uid string) (Inventory, error) {
	return observeNamespace(ctx, client, endpoint, "email-data-recovery-41561215415", uid)
}

func observeNamespace(ctx context.Context, client *http.Client, endpoint *url.URL, namespace, uid string) (Inventory, error) {
	if endpoint == nil {
		return Inventory{}, errors.New("authenticated API root is required")
	}
	root := *endpoint
	if client == nil || root.Scheme != "https" || root.Host == "" || root.Opaque != "" || root.ForceQuery || root.User != nil || root.RawQuery != "" || root.Fragment != "" || (root.Path != "" && root.Path != "/") || (!namespacePattern.MatchString(namespace) && namespace != "longhorn-system" && namespace != "email-data-recovery-41561215415") || len(namespace) > 63 || uid == "" || len(uid) > 128 {
		return Inventory{}, errors.New("fixed benchmark namespace and authenticated API root are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := observationActive(ctx); err != nil {
		return Inventory{}, err
	}
	r := reader{client: *client, endpoint: &root, namespace: namespace, uid: uid}
	r.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if r.client.Timeout == 0 || r.client.Timeout > 20*time.Second {
		r.client.Timeout = 20 * time.Second
	}
	if err := r.namespaceCurrent(ctx); err != nil {
		return Inventory{}, err
	}
	resources, err := r.discovery(ctx)
	if err != nil {
		return Inventory{}, err
	}
	coverage, err := r.nativeCoverage(ctx, resources)
	if err != nil {
		return Inventory{}, err
	}
	result := Inventory{bindings: map[string]Binding{}}
	for _, resource := range resources.resources {
		raw, err := r.get(ctx, resource.prefix+"/namespaces/"+namespace+"/"+resource.name, metadataListAccept)
		if err != nil {
			return Inventory{}, err
		}
		var actual partialList
		if decode(raw, &actual, metadataFields) != nil || actual.APIVersion != "meta.k8s.io/v1" || actual.Kind != "PartialObjectMetadataList" || actual.Metadata.ResourceVersion == "" || actual.Metadata.Continue != "" || actual.Metadata.Remaining != nil && *actual.Metadata.Remaining != 0 || actual.Items == nil {
			return Inventory{}, errors.New("namespace collection is partial or not metadata-only")
		}
		// The native metadata converter leaves Items nil for a complete empty
		// list, serialized as null. Missing items is still incomplete evidence.
		var items []partialObject
		if json.Unmarshal(actual.Items, &items) != nil {
			return Inventory{}, errors.New("namespace collection items are malformed")
		}
		seenItems := map[string]bool{}
		for _, item := range items {
			m := item.Metadata
			if item.APIVersion != "meta.k8s.io/v1" || item.Kind != "PartialObjectMetadata" || m.Namespace != namespace || m.UID == "" || len(m.UID) > 128 || m.ResourceVersion == "" || len(m.Name) > 253 || !namePattern.MatchString(m.Name) || seenItems[m.UID] {
				return Inventory{}, errors.New("namespace child identity is incomplete")
			}
			seenItems[m.UID] = true
			group := ""
			if strings.HasPrefix(resource.prefix, "/apis/") {
				group = strings.Split(strings.TrimPrefix(resource.prefix, "/apis/"), "/")[0]
			}
			groupResource := resource.name
			if group != "" {
				groupResource += "." + group
			}
			binding := Binding{m.Name, m.Namespace, m.UID, resource.kind, groupResource}
			if old, exists := result.bindings[m.UID]; exists && old != binding {
				if !eventAlias(old, binding) {
					return Inventory{}, errors.New("namespace child UID has conflicting identities")
				}
				// The same Event is served by both Event APIs; keep its first identity.
				continue
			}
			result.bindings[m.UID] = binding
		}
	}
	checked, err := r.discovery(ctx)
	if err != nil || !reflect.DeepEqual(checked, resources) {
		return Inventory{}, errors.New("served namespace resources changed during inventory")
	}
	checkedCoverage, err := r.nativeCoverage(ctx, checked)
	if err != nil || !reflect.DeepEqual(checkedCoverage, coverage) {
		return Inventory{}, errors.New("APIService deletion coverage changed during inventory")
	}
	if err := r.namespaceCurrent(ctx); err != nil {
		return Inventory{}, err
	}
	if observationActive(ctx) != nil {
		return Inventory{}, errors.New("namespace inventory expired")
	}
	return result, nil
}

// eventAlias reports whether two bindings are one Event read through its two
// served APIs, core v1 and events.k8s.io, which share one stored object and UID.
// Any other difference in identity is still a conflict.
func eventAlias(a, b Binding) bool {
	resources := map[string]bool{a.Resource: true, b.Resource: true}
	return a.Kind == "Event" && b.Kind == "Event" && a.Name == b.Name && a.Namespace == b.Namespace &&
		a.UID == b.UID && len(resources) == 2 && resources["events"] && resources["events.events.k8s.io"]
}
