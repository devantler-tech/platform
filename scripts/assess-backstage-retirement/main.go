// assess-backstage-retirement inspects the installed release; it never retires it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"
)

const maxResponse = 4 << 20

type result struct {
	Code    string
	Objects int
}

// assess proves a bounded stateless release footprint, not data retirement safety.
func assess(release, live []byte) result {
	hold := result{Code: "HOLD_INPUT"}
	r, ok := jsonObject(release)
	if !ok || r["name"] != "backstage" || r["namespace"] != "backstage" || object(r["info"])["status"] != "deployed" || !validRelatedPodEvidence(r) {
		return hold
	}
	if revision, ok := r["version"].(int); !ok || revision < 1 {
		return hold
	}
	if hooks, exists := r["hooks"]; exists && hooks != nil {
		list, ok := hooks.([]any)
		if !ok || len(list) != 0 {
			return result{Code: "HOLD_HOOKS"}
		}
	}
	manifest, ok := r["manifest"].(string)
	if !ok || manifest == "" || len(manifest) > maxResponse {
		return hold
	}
	decoder := yaml.NewDecoder(bytes.NewBufferString(manifest))
	seen := map[string]bool{}
	deployment, service := false, false
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return hold
		}
		if len(resource) == 0 {
			continue
		}
		kind, api := text(resource["kind"]), text(resource["apiVersion"])
		allowed := api == "v1" && (kind == "Service" || kind == "ConfigMap" || kind == "Secret" || kind == "ServiceAccount") || api == "apps/v1" && kind == "Deployment"
		if !allowed {
			return result{Code: "HOLD_RESOURCE_KIND"}
		}
		meta := object(resource["metadata"])
		name, namespace := text(meta["name"]), text(meta["namespace"])
		if name == "" || namespace != "" && namespace != "backstage" || len(seen) >= 64 {
			return hold
		}
		identity := api + "/" + kind + "/" + name
		if seen[identity] {
			return hold
		}
		seen[identity] = true
		annotations := object(meta["annotations"])
		if _, exists := annotations["helm.sh/hook"]; exists {
			return result{Code: "HOLD_HOOKS"}
		}
		if _, exists := annotations["helm.sh/resource-policy"]; exists {
			return result{Code: "HOLD_RETAINED_OBJECT"}
		}
		if kind == "Deployment" {
			if name != "backstage" || !statelessPod(object(object(resource["spec"])["template"])["spec"]) {
				return result{Code: "HOLD_WORKLOAD"}
			}
			deployment = true
		}
		if kind == "Service" && name == "backstage" {
			service = true
		}
	}
	if !deployment || !service {
		return hold
	}
	l, ok := jsonObject(live)
	items, itemsOK := l["items"].([]any)
	if !ok || l["kind"] != "List" || l["apiVersion"] != "v1" || !itemsOK || len(items) == 0 || len(items) > 256 {
		return hold
	}
	var databaseUID string
	liveSeen := map[string]bool{}
	for _, value := range items {
		resource := object(value)
		meta := object(resource["metadata"])
		if text(meta["name"]) == "" || meta["namespace"] != "backstage" || text(meta["uid"]) == "" || text(meta["resourceVersion"]) == "" || meta["deletionTimestamp"] != nil {
			return hold
		}
		identity := text(resource["apiVersion"]) + "/" + text(resource["kind"]) + "/" + text(meta["name"])
		if liveSeen[identity] {
			return hold
		}
		liveSeen[identity] = true
		if resource["apiVersion"] == "postgresql.cnpg.io/v1" && resource["kind"] == "Cluster" && meta["name"] == "backstage-db" {
			databaseUID = text(meta["uid"])
		}
	}
	if databaseUID == "" {
		return hold
	}
	liveDeployment, pods, claims := false, 0, 0
	for _, value := range items {
		resource := object(value)
		meta := object(resource["metadata"])
		switch resource["kind"] {
		case "Deployment":
			if meta["name"] == "backstage" {
				selector := map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "backstage", "app.kubernetes.io/instance": "backstage", "app.kubernetes.io/component": "backstage"}}
				if resource["apiVersion"] != "apps/v1" || !reflect.DeepEqual(object(resource["spec"])["selector"], selector) || !statelessPod(object(object(resource["spec"])["template"])["spec"]) {
					return result{Code: "HOLD_WORKLOAD"}
				}
				liveDeployment = true
			}
		case "Pod":
			labels := object(meta["labels"])
			if labels["app.kubernetes.io/name"] == "backstage" && labels["app.kubernetes.io/instance"] == "backstage" && labels["app.kubernetes.io/component"] == "backstage" {
				if resource["apiVersion"] != "v1" || !statelessPod(resource["spec"]) {
					return result{Code: "HOLD_WORKLOAD"}
				}
				pods++
			}
		case "PersistentVolumeClaim":
			if resource["apiVersion"] != "v1" {
				return hold
			}
			owners, exists := meta["ownerReferences"]
			if exists && owners != nil {
				list, ok := owners.([]any)
				if !ok {
					return hold
				}
				for _, value := range list {
					owner := object(value)
					if owner["apiVersion"] != "postgresql.cnpg.io/v1" || owner["kind"] != "Cluster" || owner["name"] != "backstage-db" || owner["uid"] != databaseUID {
						return result{Code: "HOLD_CLAIM_OWNER"}
					}
				}
			}
			claims++
		case "Cluster":
			if resource["apiVersion"] != "postgresql.cnpg.io/v1" || meta["name"] != "backstage-db" {
				return hold
			}
		default:
			return hold
		}
	}
	if !liveDeployment || pods == 0 || claims == 0 {
		return hold
	}
	return result{Code: "ASSESSED_STATELESS", Objects: len(seen)}
}

// object returns nil for a malformed object so its caller fails the assessment.
func object(value any) map[string]any { result, _ := value.(map[string]any); return result }

// text returns an empty string for a non-string identity.
func text(value any) string { result, _ := value.(string); return result }

// yaml.v3 rejects duplicate JSON keys as well as duplicate YAML keys. Require
// JSON syntax before decoding command output; never treat partial output as empty.
func jsonObject(data []byte) (map[string]any, bool) {
	var result map[string]any
	if len(data) > maxResponse || !json.Valid(data) || yaml.Unmarshal(data, &result) != nil || result == nil {
		return nil, false
	}
	return result, true
}

// statelessPod rejects persistent volumes and termination hooks.
func statelessPod(value any) bool {
	spec := object(value)
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) == 0 {
		return false
	}
	for _, key := range []string{"containers", "initContainers", "ephemeralContainers"} {
		values, exists := spec[key]
		if !exists || values == nil {
			continue
		}
		list, ok := values.([]any)
		if !ok {
			return false
		}
		for _, value := range list {
			container := object(value)
			if text(container["name"]) == "" {
				return false
			}
			if _, exists := object(container["lifecycle"])["preStop"]; exists {
				return false
			}
		}
	}
	volumes, exists := spec["volumes"]
	if !exists || volumes == nil {
		return true
	}
	list, ok := volumes.([]any)
	if !ok {
		return false
	}
	for _, value := range list {
		volume := object(value)
		if text(volume["name"]) == "" || len(volume) != 2 {
			return false
		}
		allowed := false
		for _, key := range []string{"emptyDir", "configMap", "secret", "projected", "downwardAPI"} {
			if source, exists := volume[key]; exists && object(source) != nil {
				allowed = true
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

type command func(context.Context, string, ...string) ([]byte, error)

// capture executes only collect's fixed readers and withholds raw stderr.
func capture(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stderr = io.Discard
	var output boundedOutput
	cmd.Stdout = &output
	if cmd.Run() != nil {
		return nil, fmt.Errorf("read unavailable")
	}
	return output.Bytes(), nil
}

type boundedOutput struct{ bytes.Buffer }

// Write bounds captured output before any raw data reaches assessment.
func (output *boundedOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > maxResponse {
		return 0, fmt.Errorf("read too large")
	}
	return output.Buffer.Write(data)
}

// collect reads the stored release and live resources without mutating either.
func collect(ctx context.Context, execute command) ([]byte, []byte, error) {
	release, err := execute(ctx, "helm", "status", "backstage", "--namespace=backstage", "--kube-context=admin@prod", "--output=json")
	if err != nil {
		return nil, nil, err
	}
	live, err := execute(ctx, "kubectl", "--context=admin@prod", "--namespace=backstage", "get", "deployment,pod,persistentvolumeclaim,clusters.postgresql.cnpg.io", "--output=json", "--request-timeout=30s")
	return release, live, err
}

// validRelatedPodEvidence requires complete native evidence without changing it.
func validRelatedPodEvidence(release map[string]any) bool {
	raw, exists := object(release["info"])["resources"]
	if !exists || raw == nil {
		return false
	}
	resources, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	raw, exists = resources["v1/Pod(related)"]
	if !exists {
		return false
	}
	lists, ok := raw.([]any)
	if !ok || len(lists) == 0 {
		return false
	}
	for _, raw := range lists {
		list := object(raw)
		meta := object(list["metadata"])
		if list["apiVersion"] != "v1" || list["kind"] != "PodList" || text(meta["resourceVersion"]) == "" {
			return false
		}
		items, ok := list["items"].([]any)
		if !ok || len(items) == 0 {
			return false
		}
		for _, raw := range items {
			pod := object(raw)
			meta := object(pod["metadata"])
			if pod["apiVersion"] != "v1" || pod["kind"] != "Pod" ||
				meta["namespace"] != "backstage" || text(meta["name"]) == "" ||
				text(meta["uid"]) == "" || text(meta["resourceVersion"]) == "" {
				return false
			}
		}
	}
	return true
}

// normalizeStatusClock excludes only Helm's derived related-list collection clock.
// Helm 4.2 Status.Run appends KubeClient.Get(..., true), including PodLists
// whose resourceVersion advances on unrelated writes. Their member identities,
// versions and contents, and every stored release field, remain compared.
func normalizeStatusClock(release map[string]any) bool {
	if !validRelatedPodEvidence(release) {
		return false
	}
	lists := object(object(release["info"])["resources"])["v1/Pod(related)"].([]any)
	for _, raw := range lists {
		delete(object(object(raw)["metadata"]), "resourceVersion")
	}
	return true
}

// run fails closed and emits only the assessment code and object count.
func run(args []string, input io.Reader, output io.Writer, execute command) int {
	var release, live []byte
	if len(args) != 1 {
		_, _ = fmt.Fprintln(output, "PORTAL-FOOTPRINT=HOLD_ARGUMENTS")
		return 2
	}
	switch args[0] {
	case "--stdin":
		data, err := io.ReadAll(io.LimitReader(input, 2*maxResponse+1))
		var fixture struct {
			Release json.RawMessage
			Live    json.RawMessage
		}
		_, valid := jsonObject(data)
		if err != nil || !valid || json.Unmarshal(data, &fixture) != nil {
			_, _ = fmt.Fprintln(output, "PORTAL-FOOTPRINT=HOLD_INPUT")
			return 2
		}
		release, live = fixture.Release, fixture.Live
	case "--live":
		if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || os.Getenv("GITHUB_REF") != "refs/heads/main" {
			_, _ = fmt.Fprintln(output, "PORTAL-FOOTPRINT=HOLD_PROTECTED_RUNNER")
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var err error
		release, live, err = collect(ctx, execute)
		if err != nil {
			_, _ = fmt.Fprintln(output, "PORTAL-FOOTPRINT=HOLD_READ")
			return 2
		}
		againRelease, againLive, err := collect(ctx, execute)
		r, rok := jsonObject(release)
		l, lok := jsonObject(live)
		ar, arok := jsonObject(againRelease)
		al, alok := jsonObject(againLive)
		if err != nil || !rok || !lok || !arok || !alok ||
			!normalizeStatusClock(r) || !normalizeStatusClock(ar) ||
			!reflect.DeepEqual(r, ar) || !reflect.DeepEqual(l, al) {
			_, _ = fmt.Fprintln(output, "PORTAL-FOOTPRINT=HOLD_CHANGED_SNAPSHOT")
			return 2
		}
	default:
		_, _ = fmt.Fprintln(output, "PORTAL-FOOTPRINT=HOLD_ARGUMENTS")
		return 2
	}
	result := assess(release, live)
	if _, err := fmt.Fprintf(output, "PORTAL-FOOTPRINT=%s objects=%d dataRetirement=UNASSESSED mutation=NONE\n", result.Code, result.Objects); err != nil {
		return 2
	}
	if result.Code != "ASSESSED_STATELESS" {
		return 2
	}
	return 0
}

// main routes the real CLI through the same tested boundary as offline fixtures.
func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, capture)) }
