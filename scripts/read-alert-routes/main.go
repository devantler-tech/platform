package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type executor func(context.Context, ...string) ([]byte, error)

const maxResponse = 8 << 20

var targets = []string{
	"/apis/coroot.com/v1/namespaces/observability/coroots/coroot",
	"/apis/notification.toolkit.fluxcd.io/v1beta3/providers",
	"/apis/notification.toolkit.fluxcd.io/v1beta3/alerts",
	"/apis/batch/v1/cronjobs",
	"/api/v1/namespaces/kubescape/pods/alertmanager-0",
	"/api/v1/namespaces/kubescape/pods/alertmanager-1",
	"/api/v1/namespaces/kubescape/pods/http:alertmanager-0:9093/proxy/api/v2/status",
	"/api/v1/namespaces/kubescape/pods/http:alertmanager-1:9093/proxy/api/v2/status",
}

type metadata struct {
	UID, Name, Namespace string
	Generation           int64
}
type object struct {
	Kind     string
	Metadata metadata
	Spec     json.RawMessage
}
type summary struct{ projects, corootSlack, providers, fluxSlack, alerts, checks, candidates, amSlack int }

func allowed(env func(string) string) bool {
	return env("GITHUB_ACTIONS") == "true" && env("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_REF") == "refs/heads/main" &&
		env("GITHUB_RUN_ATTEMPT") == "1" && env("GITHUB_ACTOR") == "devantler" &&
		env("RUNNER_ENVIRONMENT") == "github-hosted" && env("ALERT_ROUTES_CONFIRM") == "read-alert-routes" &&
		regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(env("GITHUB_SHA"))
}

func observe(ctx context.Context, env func(string) string, read executor, out io.Writer) int {
	unknown := func() int { _, _ = fmt.Fprintln(out, "ALERT_ROUTE_INVENTORY=UNKNOWN"); return 3 }
	if !allowed(env) {
		return unknown()
	}
	var first [32]byte
	var result summary
	// A second complete observation rejects replaced peers or changed configuration.
	for pass := 0; pass < 2; pass++ {
		var payloads [8][]byte
		for i, target := range targets {
			raw, err := read(ctx, "--context=admin@prod", "--request-timeout=20s", "get", "--raw", target)
			if err != nil || len(raw) == 0 || len(raw) > maxResponse || ctx.Err() != nil {
				return unknown()
			}
			payloads[i] = raw
		}
		var signature [32]byte
		var err error
		result, signature, err = assess(payloads)
		if err != nil {
			return unknown()
		}
		if pass == 0 {
			first = signature
		} else if first != signature {
			return unknown()
		}
	}
	if _, err := fmt.Fprintf(out, "ALERT_ROUTE_INVENTORY=OBSERVED scope=cluster-configuration coroot_projects=%d coroot_slack=%d flux_providers=%d flux_slack=%d flux_alerts=%d active_cronjobs=%d webhook_candidates=%d alertmanager_peers=2 alertmanager_slack=%d\n", result.projects, result.corootSlack, result.providers, result.fluxSlack, result.alerts, result.checks, result.candidates, result.amSlack); err != nil {
		return 3
	}
	return 0
}

func decode(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > maxResponse {
		return errors.New("response size")
	}
	return json.Unmarshal(raw, v)
}

func list(raw []byte, kind string) ([]object, error) {
	var data struct {
		Kind     string
		Metadata struct {
			Continue           string
			RemainingItemCount int64
		}
		Items []object
	}
	if decode(raw, &data) != nil || data.Kind != kind || len(data.Items) == 0 || data.Metadata.Continue != "" || data.Metadata.RemainingItemCount != 0 {
		return nil, errors.New("incomplete list")
	}
	seen := map[string]bool{}
	for _, item := range data.Items {
		if item.Metadata.UID == "" || item.Metadata.Generation < 1 || seen[item.Metadata.UID] || len(item.Spec) == 0 {
			return nil, errors.New("incomplete object")
		}
		seen[item.Metadata.UID] = true
	}
	sort.Slice(data.Items, func(i, j int) bool { return data.Items[i].Metadata.UID < data.Items[j].Metadata.UID })
	return data.Items, nil
}

func assess(payloads [8][]byte) (summary, [32]byte, error) {
	var result summary
	var zero [32]byte
	fail := func() (summary, [32]byte, error) { return summary{}, zero, errors.New("incomplete observation") }
	var coroot object
	if decode(payloads[0], &coroot) != nil || coroot.Kind != "Coroot" || coroot.Metadata.UID == "" || coroot.Metadata.Generation < 1 {
		return fail()
	}
	var spec struct {
		Projects []struct {
			Name                     string
			NotificationIntegrations map[string]json.RawMessage
		}
	}
	if decode(coroot.Spec, &spec) != nil || len(spec.Projects) == 0 {
		return fail()
	}
	names := map[string]bool{}
	for _, project := range spec.Projects {
		if project.Name == "" || names[project.Name] {
			return fail()
		}
		names[project.Name] = true
		result.projects++
		for name, raw := range project.NotificationIntegrations {
			switch name {
			case "baseURL":
			case "webhook":
				var webhook struct {
					URL               string
					Incidents, Alerts *bool
				}
				if decode(raw, &webhook) != nil || webhook.Incidents == nil || webhook.Alerts == nil {
					return fail()
				}
				endpoint, err := url.Parse(webhook.URL)
				if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil {
					return fail()
				}
				if endpoint.Hostname() == "hooks.slack.com" && (*webhook.Incidents || *webhook.Alerts) {
					result.corootSlack++
				}
			default:
				return fail() // An unmodelled integration must not disappear from this inventory.
			}
		}
	}
	providers, err := list(payloads[1], "ProviderList")
	if err != nil {
		return fail()
	}
	providerNames := map[string]bool{}
	for _, provider := range providers {
		var config struct {
			Type    string
			Suspend bool
		}
		if decode(provider.Spec, &config) != nil || config.Type == "" || provider.Metadata.Name == "" || provider.Metadata.Namespace == "" {
			return fail()
		}
		key := provider.Metadata.Namespace + "/" + provider.Metadata.Name
		if providerNames[key] {
			return fail()
		}
		providerNames[key] = true
		result.providers++
		if config.Type == "slack" && !config.Suspend {
			result.fluxSlack++
		}
	}
	alerts, err := list(payloads[2], "AlertList")
	if err != nil {
		return fail()
	}
	for _, alert := range alerts {
		var config struct {
			ProviderRef  struct{ Name string }
			EventSources []json.RawMessage
			Suspend      bool
		}
		if decode(alert.Spec, &config) != nil || len(config.EventSources) == 0 || !providerNames[alert.Metadata.Namespace+"/"+config.ProviderRef.Name] {
			return fail()
		}
		if !config.Suspend {
			result.alerts++
		}
	}
	crons, err := list(payloads[3], "CronJobList")
	if err != nil {
		return fail()
	}
	for _, cron := range crons {
		var config struct {
			Schedule string
			Suspend  bool
		}
		if decode(cron.Spec, &config) != nil || config.Schedule == "" {
			return fail()
		}
		if !config.Suspend {
			result.checks++
			// This is a candidate, not proof of a Slack destination. Never read its Secret.
			text := strings.ToLower(string(cron.Spec))
			if strings.Contains(text, "slack") || strings.Contains(text, "webhook") {
				result.candidates++
			}
		}
	}
	var peers [2]any
	peerUIDs := map[string]bool{}
	for i := 0; i < 2; i++ {
		var pod struct {
			Kind     string
			Metadata metadata
			Status   struct {
				Phase             string
				Conditions        []struct{ Type, Status string }
				ContainerStatuses []struct {
					Name, ContainerID string
					Ready             bool
					RestartCount      int
				}
			}
		}
		if decode(payloads[4+i], &pod) != nil || pod.Kind != "Pod" || pod.Metadata.UID == "" || pod.Status.Phase != "Running" || len(pod.Status.ContainerStatuses) == 0 {
			return fail()
		}
		if peerUIDs[pod.Metadata.UID] {
			return fail()
		}
		peerUIDs[pod.Metadata.UID] = true
		ready, manager := false, false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		for _, container := range pod.Status.ContainerStatuses {
			if !container.Ready || container.ContainerID == "" {
				return fail()
			}
			if container.Name == "alertmanager" {
				manager = true
			}
		}
		if !ready || !manager {
			return fail()
		}
		peers[i] = struct {
			UID    string
			Status any
		}{pod.Metadata.UID, pod.Status}
	}
	var configs [2]string
	for i := 0; i < 2; i++ {
		var status struct{ Config struct{ Original string } }
		if decode(payloads[6+i], &status) != nil || status.Config.Original == "" {
			return fail()
		}
		configs[i] = status.Config.Original
	}
	if configs[0] != configs[1] {
		return fail()
	}
	var am struct {
		Route     *struct{ Receiver string }
		Receivers []struct {
			Name         string
			SlackConfigs []yaml.Node `yaml:"slack_configs"`
		}
	}
	decoder := yaml.NewDecoder(strings.NewReader(configs[0]))
	if decoder.Decode(&am) != nil || am.Route == nil || len(am.Receivers) == 0 {
		return fail()
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fail()
	}
	receiverNames := map[string]bool{}
	for _, receiver := range am.Receivers {
		if receiver.Name == "" || receiverNames[receiver.Name] {
			return fail()
		}
		receiverNames[receiver.Name] = true
		result.amSlack += len(receiver.SlackConfigs)
	}
	if !receiverNames[am.Route.Receiver] {
		return fail()
	}
	// Keep private configuration and identity bytes in memory; publish no digest or raw fields.
	evidence := struct {
		Coroot                   object
		Providers, Alerts, Crons []object
		Peers                    [2]any
		Config                   string
	}{coroot, providers, alerts, crons, peers, configs[0]}
	canonical, err := json.Marshal(evidence)
	if err != nil {
		return fail()
	}
	return result, sha256.Sum256(canonical), nil
}

type boundedResponse struct {
	body     []byte
	overflow bool
}

func (b *boundedResponse) Write(data []byte) (int, error) {
	if len(data) > maxResponse-len(b.body) {
		b.overflow = true
		return 0, errors.New("response limit")
	}
	b.body = append(b.body, data...)
	return len(data), nil
}
func nativeRead(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "kubectl", args...)
	var response boundedResponse
	command.Stdout = &response
	command.Stderr = io.Discard
	if command.Run() != nil || response.overflow {
		return nil, errors.New("observation unavailable")
	}
	return response.body, nil
}

func main() {
	if len(os.Args) != 1 || !allowed(os.Getenv) {
		fmt.Println("ALERT_ROUTE_INVENTORY=UNKNOWN")
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	code := observe(ctx, os.Getenv, nativeRead, os.Stdout)
	cancel()
	os.Exit(code)
}
