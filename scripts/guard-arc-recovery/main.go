// guard-arc-recovery proves the retained ARC charts cannot admit work while
// their Helm reconciliation recovers. It never requests credentials or JIT data.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const metadataAccept = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
const controllerDeploymentPath = "/apis/apps/v1/namespaces/arc-systems/deployments/arc-controller"
const retainedPoolPath = "/apis/actions.github.com/v1alpha1/namespaces/arc-ksail-analysis/autoscalingrunnersets"
const retainedPoolName = "ksail-code-quality"

// RE2 has no negative lookahead; these alternatives match every nonempty
// method except the exact GET spelling, including nonstandard methods.
const rejectNonGET = "^(G|GE|GET.+|[^G].*|G[^E].*|GE[^T].*)$"

var recoveryEndpoints = []string{
	"/apis/actions.github.com/v1alpha1/namespaces/arc-runners/autoscalingrunnersets",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-runners/autoscalinglisteners",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-runners/ephemeralrunnersets",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-runners/ephemeralrunners",
	retainedPoolPath,
	"/apis/actions.github.com/v1alpha1/namespaces/arc-ksail-analysis/autoscalinglisteners",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-ksail-analysis/ephemeralrunnersets",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-ksail-analysis/ephemeralrunners",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-systems/autoscalingrunnersets",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-systems/autoscalinglisteners",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-systems/ephemeralrunnersets",
	"/apis/actions.github.com/v1alpha1/namespaces/arc-systems/ephemeralrunners",
	"/apis/helm.toolkit.fluxcd.io/v2/namespaces/arc-runners/helmreleases",
	"/apis/external-secrets.io/v1/namespaces/arc-runners/externalsecrets",
	"/api/v1/namespaces/arc-runners/pods",
	"/api/v1/namespaces/arc-ksail-analysis/pods",
	"/api/v1/namespaces/arc-systems/pods?labelSelector=platform.devantler.tech%2Farc-role%3Dlistener",
}

type release struct {
	Metadata struct {
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Suspend *bool `yaml:"suspend"`
		Values  struct {
			Flags struct {
				Namespace string `yaml:"watchSingleNamespace"`
			} `yaml:"flags"`
		} `yaml:"values"`
	} `yaml:"spec"`
}

func recoveryArmed(root string) (bool, error) {
	controller, err := os.ReadFile(filepath.Join(root, "k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml"))
	if err != nil {
		return false, errors.New("controller declaration unreadable")
	}
	var c release
	if yaml.Unmarshal(controller, &c) != nil {
		return false, errors.New("controller declaration invalid")
	}
	mode := c.Metadata.Annotations["platform.devantler.tech/arc-recovery"]
	if mode == "" {
		return false, nil
	}
	if mode != "drain-only" || c.Spec.Suspend == nil || *c.Spec.Suspend || c.Spec.Values.Flags.Namespace != "arc-runners" {
		return false, errors.New("recovery controller scope invalid")
	}
	data, err := os.ReadFile(filepath.Join(root, "k8s/providers/hetzner/infrastructure/retained-ksail-analysis/helm-release.yaml"))
	if err != nil {
		return false, errors.New("legacy drain declaration unreadable")
	}
	// Use explicit pointer fields: omitted zero bounds mean unbounded ARC scaling.
	var legacy struct {
		Spec struct {
			Suspend *bool `yaml:"suspend"`
			Values  struct {
				Name string `yaml:"runnerScaleSetName"`
				Min  *int   `yaml:"minRunners"`
				Max  *int   `yaml:"maxRunners"`
			} `yaml:"values"`
		} `yaml:"spec"`
	}
	if yaml.Unmarshal(data, &legacy) != nil || legacy.Spec.Suspend == nil || *legacy.Spec.Suspend || legacy.Spec.Values.Name != retainedPoolName || legacy.Spec.Values.Min == nil || legacy.Spec.Values.Max == nil || *legacy.Spec.Values.Min != 0 || *legacy.Spec.Values.Max != 0 {
		return false, errors.New("legacy pool is not explicitly drained and reconciling")
	}
	return true, nil
}

func metadataClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// encoding/json accepts duplicate object keys and case-insensitive struct field
// aliases. Neither can establish absence or an effective controller scope.
// Check the complete bounded document before any typed decoding; unrelated
// Deployment fields remain readable without making their spelling a proof gate.
func requireUnambiguousJSON(data []byte) error {
	fields := map[string][]string{
		"":                                {"apiVersion", "kind", "metadata", "items", "spec", "status"},
		"metadata":                        {"name", "namespace", "uid", "generation", "deletionTimestamp", "resourceVersion", "continue", "remainingItemCount"},
		"items[]":                         {"apiVersion", "kind", "metadata"},
		"items[].metadata":                {"name", "namespace", "uid", "resourceVersion", "deletionTimestamp"},
		"spec":                            {"replicas", "template"},
		"spec.template":                   {"spec"},
		"spec.template.spec":              {"containers"},
		"spec.template.spec.containers[]": {"name", "command", "args"},
		"status":                          {"observedGeneration", "replicas", "updatedReplicas", "readyReplicas", "availableReplicas", "unavailableReplicas"},
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(string, int) error
	walk = func(path string, depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting exceeds the proof bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("JSON proof is incomplete")
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return errors.New("JSON proof contains ambiguous object fields")
				}
				seen[key] = true
				for _, canonical := range fields[path] {
					if key != canonical && strings.EqualFold(key, canonical) {
						return errors.New("JSON proof contains a case-aliased field")
					}
				}
				child := key
				if path != "" {
					child = path + "." + key
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(path+"[]", depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("JSON proof contains an unexpected delimiter")
		}
		end, err := decoder.Token()
		if err != nil || (delimiter == '{' && end != json.Delim('}')) || (delimiter == '[' && end != json.Delim(']')) {
			return errors.New("JSON proof is incomplete")
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON proof has trailing data")
	}
	return nil
}

func readMetadata(ctx context.Context, client *http.Client, endpoint string) ([]json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("metadata request invalid")
	}
	request.Header.Set("Accept", metadataAccept)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("metadata read failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("metadata read refused or redirected")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("metadata response incomplete or oversized")
	}
	if err := requireUnambiguousJSON(data); err != nil {
		return nil, err
	}
	var list struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			ResourceVersion string `json:"resourceVersion"`
			Continue        string `json:"continue"`
			Remaining       *int64 `json:"remainingItemCount"`
		} `json:"metadata"`
		Items json.RawMessage `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&list) != nil || list.APIVersion != "meta.k8s.io/v1" || list.Kind != "PartialObjectMetadataList" || list.Metadata.ResourceVersion == "" || list.Metadata.Continue != "" || len(list.Items) == 0 || (list.Metadata.Remaining != nil && *list.Metadata.Remaining != 0) {
		return nil, errors.New("metadata response is not a complete metadata list")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("metadata response has trailing data")
	}
	// Kubernetes encodes a native empty metadata list as null. Keep the field
	// presence check above so an omitted list cannot prove absence.
	if bytes.Equal(bytes.TrimSpace(list.Items), []byte("null")) {
		return nil, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(list.Items, &items) != nil || items == nil {
		return nil, errors.New("metadata response items is not an array or native null")
	}
	return items, nil
}

func requireEmptyMetadata(ctx context.Context, client *http.Client, endpoint string) error {
	items, err := readMetadata(ctx, client, endpoint)
	if err != nil {
		return err
	}
	if len(items) != 0 {
		return errors.New("ARC recovery found a live pool, credential-sync resource or pod")
	}
	return nil
}

// Helm retains its declared AutoscalingRunnerSet even with both bounds at zero.
// Observe only that exact declaration; listeners, runner sets and pods must be
// absent, and after-reconcile independently proves the controller excludes it.
func requireRetainedMetadata(ctx context.Context, client *http.Client, endpoint string) error {
	items, err := readMetadata(ctx, client, endpoint)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	if len(items) != 1 {
		return errors.New("retained namespace contains unexpected pools")
	}
	var item struct {
		APIVersion string                     `json:"apiVersion"`
		Kind       string                     `json:"kind"`
		Metadata   map[string]json.RawMessage `json:"metadata"`
	}
	decoder := json.NewDecoder(bytes.NewReader(items[0]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&item) != nil || item.APIVersion != "meta.k8s.io/v1" || item.Kind != "PartialObjectMetadata" {
		return errors.New("retained pool response is not metadata")
	}
	// The pinned chart names this object from runnerScaleSetName, not the
	// HelmRelease name. recoveryArmed binds that same explicit source value.
	for key, expected := range map[string]string{"name": retainedPoolName, "namespace": "arc-ksail-analysis"} {
		var actual string
		if json.Unmarshal(item.Metadata[key], &actual) != nil || actual != expected {
			return errors.New("retained pool identity does not match its declaration")
		}
	}
	for _, key := range []string{"uid", "resourceVersion"} {
		var value string
		if json.Unmarshal(item.Metadata[key], &value) != nil || value == "" {
			return errors.New("retained pool identity is incomplete")
		}
	}
	if value, exists := item.Metadata["deletionTimestamp"]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return errors.New("retained pool is being deleted")
	}
	return nil
}

func requireControllerScope(ctx context.Context, client *http.Client, endpoint string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("controller scope request invalid")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("controller scope read failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("controller scope read refused or redirected")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("controller scope response incomplete or oversized")
	}
	if err := requireUnambiguousJSON(data); err != nil {
		return err
	}
	var deployment struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name, Namespace, UID string
			Generation           int64
			DeletionTimestamp    *string
		} `json:"metadata"`
		Spec struct {
			Replicas int
			Template struct {
				Spec struct {
					Containers []struct {
						Name          string
						Command, Args []string
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration                                                               int64
			Replicas, UpdatedReplicas, ReadyReplicas, AvailableReplicas, UnavailableReplicas int
		} `json:"status"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&deployment) != nil || decoder.Decode(new(any)) != io.EOF || deployment.APIVersion != "apps/v1" || deployment.Kind != "Deployment" || deployment.Metadata.Name != "arc-controller" || deployment.Metadata.Namespace != "arc-systems" || deployment.Metadata.UID == "" || deployment.Metadata.Generation < 1 || deployment.Metadata.DeletionTimestamp != nil {
		return errors.New("controller scope response invalid")
	}
	if deployment.Status.ObservedGeneration != deployment.Metadata.Generation || deployment.Spec.Replicas != 1 || deployment.Status.Replicas != 1 || deployment.Status.UpdatedReplicas != 1 || deployment.Status.ReadyReplicas != 1 || deployment.Status.AvailableReplicas != 1 || deployment.Status.UnavailableReplicas != 0 || len(deployment.Spec.Template.Spec.Containers) != 1 {
		return errors.New("controller scope has not fully rolled out")
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Name != "manager" || len(container.Command) != 1 || container.Command[0] != "/manager" {
		return errors.New("controller executable differs from the pinned chart")
	}
	scope, mode := 0, 0
	option := regexp.MustCompile(`^--[a-z][a-z0-9-]*(=.*)?$`)
	for _, arg := range container.Args {
		if !option.MatchString(arg) {
			return errors.New("controller arguments contain an option terminator or operand")
		}
		switch {
		case strings.HasPrefix(arg, "--watch-single-namespace"):
			if arg != "--watch-single-namespace=arc-runners" {
				return errors.New("controller still watches an unapproved namespace")
			}
			scope++
		case strings.HasPrefix(arg, "--watch-namespace"):
			return errors.New("controller carries an additional namespace scope")
		case strings.HasPrefix(arg, "--auto-scaling-runner-set-only"):
			if arg != "--auto-scaling-runner-set-only" {
				return errors.New("controller mode differs from the pinned chart")
			}
			mode++
		}
	}
	if scope != 1 || mode != 1 {
		return errors.New("controller watch scope or mode is missing or duplicated")
	}
	return nil
}

// kubectl retains its existing protected CI kubeconfig. The short-lived proxy
// accepts only these GET paths on loopback; no token is copied into this command.
func startMetadataProxy(ctx context.Context, command func(context.Context, ...string) *exec.Cmd) (string, func(), error) {
	childCtx, cancel := context.WithCancel(ctx)
	paths := make([]string, len(recoveryEndpoints)+1)
	for i, path := range recoveryEndpoints {
		paths[i] = regexp.QuoteMeta(strings.SplitN(path, "?", 2)[0])
	}
	paths[len(recoveryEndpoints)] = regexp.QuoteMeta(controllerDeploymentPath)
	cmd := command(childCtx, "--context", "admin@prod", "proxy", "--address=127.0.0.1", "--port=0", "--accept-hosts=^127\\.0\\.0\\.1$", "--accept-paths=^("+strings.Join(paths, "|")+")$", "--reject-methods="+rejectNonGET)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, errors.New("metadata proxy output unavailable")
	}
	if cmd.Start() != nil {
		cancel()
		return "", nil, errors.New("metadata proxy failed to start")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop := func() { cancel(); <-done }
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024), 4096)
		if scanner.Scan() {
			address <- scanner.Text()
		} else {
			address <- ""
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case line := <-address:
		match := regexp.MustCompile(`^Starting to serve on 127\.0\.0\.1:([0-9]+)$`).FindStringSubmatch(line)
		if len(match) != 2 {
			stop()
			return "", nil, errors.New("metadata proxy address invalid")
		}
		port, err := strconv.Atoi(match[1])
		if err != nil || port < 1 || port > 65535 {
			stop()
			return "", nil, errors.New("metadata proxy port invalid")
		}
		return "http://127.0.0.1:" + match[1], stop, nil
	case <-ctx.Done():
		stop()
		return "", nil, errors.New("metadata proxy readiness timed out")
	}
}

func run(stage string) error {
	if stage != "before-publish" && stage != "after-reconcile" {
		return errors.New("invalid recovery observation stage")
	}
	armed, err := recoveryArmed(".")
	if err != nil {
		return err
	}
	if !armed {
		fmt.Println("ARC drain recovery guard is not armed by this declaration")
		return nil
	}
	ref := os.Getenv("GITHUB_REF")
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Getenv("GITHUB_SHA")) || (ref != "refs/heads/main" && !strings.HasPrefix(ref, "refs/heads/gh-readonly-queue/main/")) {
		return errors.New("ARC recovery metadata proof requires the protected production workflow")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	base, stop, err := startMetadataProxy(ctx, func(ctx context.Context, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "kubectl", args...)
	})
	if err != nil {
		return err
	}
	defer stop()
	client := metadataClient()
	defer client.CloseIdleConnections()
	if stage == "after-reconcile" {
		if err := requireControllerScope(ctx, client, base+controllerDeploymentPath); err != nil {
			return err
		}
	}
	for _, endpoint := range recoveryEndpoints {
		if endpoint == retainedPoolPath {
			if err := requireRetainedMetadata(ctx, client, base+endpoint); err != nil {
				return err
			}
			continue
		}
		if err := requireEmptyMetadata(ctx, client, base+endpoint); err != nil {
			return err
		}
	}
	fmt.Printf("ARC drain recovery %s: %d complete metadata lists prove no active organization resources, retained runner children or ARC pods\n", stage, len(recoveryEndpoints))
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: guard-arc-recovery <before-publish|after-reconcile>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "ARC recovery refused:", err)
		os.Exit(1)
	}
}
