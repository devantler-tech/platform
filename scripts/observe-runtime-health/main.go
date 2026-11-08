// observe-runtime-health reads a fixed production scope and emits aggregate facts only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

const (
	corootProxy  = "/api/v1/namespaces/observability/services/coroot-coroot:8080/proxy"
	rulesPath    = "/apis/kubescape.io/v1/namespaces/kubescape/rules"
	bindingsPath = "/apis/kubescape.io/v1/runtimerulealertbindings"
)

var (
	projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	commitPattern    = regexp.MustCompile(`^[a-f0-9]{40}$`)
	logCountPattern  = regexp.MustCompile(`^([1-9][0-9]*) unique errors?$`)
	errEvidence      = errors.New("incomplete runtime observation")
)

type readFunc func(context.Context, string) ([]byte, error)

type snapshot struct {
	SourceSHA                  string    `json:"source_sha"`
	ObservedAt                 time.Time `json:"observed_at"`
	CompletedAt                time.Time `json:"completed_at"`
	WindowFrom                 time.Time `json:"application_window_from"`
	WindowTo                   time.Time `json:"application_window_to"`
	CorootHealth               string    `json:"coroot_health"`
	Applications               int       `json:"applications"`
	ApplicationIssues          int       `json:"application_issues"`
	InfoApplications           int       `json:"info_applications"`
	UnknownApplications        int       `json:"unknown_applications"`
	LogErrorPatterns           int       `json:"application_log_error_patterns"`
	Alerts                     int       `json:"firing_alerts_observed"`
	UnresolvedAlerts           int       `json:"unresolved_alerts_total"`
	Risks                      int       `json:"risks_including_dismissed"`
	DismissedRisks             int       `json:"dismissed_risks"`
	UnknownRisks               int       `json:"unknown_risks"`
	RuleObjects                int       `json:"runtime_rule_objects"`
	EnabledRules               int       `json:"enabled_runtime_rules"`
	Bindings                   int       `json:"runtime_binding_objects"`
	BoundEntries               int       `json:"runtime_binding_entries"`
	PortForwardEnabledAndBound bool      `json:"port_forward_enabled_and_bound"`
	FullAcceptanceVerified     bool      `json:"full_platform_acceptance_verified"`
}

type envelope struct {
	Context *struct {
		Status *struct {
			Status *string `json:"status"`
			Error  string  `json:"error"`
		} `json:"status"`
		Alerts map[string]int `json:"alerts"`
	} `json:"context"`
	Data json.RawMessage `json:"data"`
}

func readEnvelope(ctx context.Context, read readFunc, path string, s *snapshot, alerts map[string]int) (json.RawMessage, error) {
	body, err := read(ctx, path)
	if err != nil {
		return nil, errEvidence
	}
	var e envelope
	if json.Unmarshal(body, &e) != nil || e.Context == nil || e.Context.Status == nil ||
		e.Context.Status.Status == nil || e.Context.Status.Error != "" || e.Context.Alerts == nil || len(e.Data) == 0 || bytes.Equal(e.Data, []byte("null")) {
		return nil, errEvidence
	}
	health := *e.Context.Status.Status
	if severityRank(health) == 0 {
		return nil, errEvidence
	}
	if severityRank(health) > severityRank(s.CorootHealth) {
		s.CorootHealth = health
	}
	for severity, count := range e.Context.Alerts {
		if count < 0 || count > 1000000000 {
			return nil, errEvidence
		}
		// Contexts are successive observations, not disjoint alert sets. Preserve the
		// highest count at each severity instead of double-counting or losing a spike.
		if count > alerts[severity] {
			alerts[severity] = count
		}
	}
	return e.Data, nil
}

func severityRank(status string) int {
	switch status {
	case "ok":
		return 1
	case "info":
		return 2
	case "warning":
		return 3
	case "critical":
		return 4
	case "unknown":
		return 5
	default:
		return 0
	}
}

func readApplications(data json.RawMessage, s *snapshot) error {
	var v struct {
		Applications []struct {
			Status *string `json:"status"`
			Logs   *struct {
				Value *string `json:"value"`
			} `json:"logs"`
		} `json:"applications"`
	}
	if json.Unmarshal(data, &v) != nil || len(v.Applications) == 0 {
		return errEvidence
	}
	for _, a := range v.Applications {
		if a.Status == nil || severityRank(*a.Status) == 0 || a.Logs == nil || a.Logs.Value == nil {
			return errEvidence
		}
		s.Applications++
		switch *a.Status {
		case "warning", "critical":
			s.ApplicationIssues++
		case "info":
			s.InfoApplications++
		case "unknown":
			s.UnknownApplications++
		}
		if *a.Logs.Value == "" {
			continue
		}
		match := logCountPattern.FindStringSubmatch(*a.Logs.Value)
		if len(match) != 2 {
			return errEvidence
		}
		count, err := strconv.Atoi(match[1])
		if err != nil || count > 1000000000 {
			return errEvidence
		}
		s.LogErrorPatterns += count
	}
	return nil
}

func readRisks(data json.RawMessage, s *snapshot) error {
	var v struct {
		Risks json.RawMessage `json:"risks"`
	}
	if json.Unmarshal(data, &v) != nil || len(v.Risks) == 0 {
		return errEvidence
	}
	// Coroot serializes the genuine empty result as null, rather than [].
	var risks []struct {
		Severity  *string         `json:"severity"`
		Dismissal json.RawMessage `json:"dismissal"`
	}
	if json.Unmarshal(v.Risks, &risks) != nil {
		return errEvidence
	}
	for _, risk := range risks {
		if risk.Severity == nil || severityRank(*risk.Severity) == 0 {
			return errEvidence
		}
		s.Risks++
		if *risk.Severity == "unknown" {
			s.UnknownRisks++
		}
		if len(risk.Dismissal) > 0 && !bytes.Equal(risk.Dismissal, []byte("null")) {
			s.DismissedRisks++
		}
	}
	return nil
}

func readRuleBaseline(ctx context.Context, read readFunc, s *snapshot) error {
	var rules struct {
		Items *[]struct {
			Spec struct {
				Rules []struct {
					ID      *string  `json:"id"`
					Name    *string  `json:"name"`
					Tags    []string `json:"tags"`
					Enabled *bool    `json:"enabled"`
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	body, err := read(ctx, rulesPath)
	if err != nil || json.Unmarshal(body, &rules) != nil || rules.Items == nil {
		return errEvidence
	}
	enabled := false
	portForwardNames := map[string]bool{}
	portForwardTags := map[string]bool{}
	otherEnabledNames := map[string]bool{}
	s.RuleObjects = len(*rules.Items)
	for _, object := range *rules.Items {
		if object.Spec.Rules == nil {
			return errEvidence
		}
		for _, rule := range object.Spec.Rules {
			if rule.ID == nil || *rule.ID == "" || rule.Name == nil || rule.Enabled == nil {
				return errEvidence
			}
			if *rule.Enabled {
				s.EnabledRules++
				if *rule.ID == "R2001" {
					enabled = true
					portForwardNames[*rule.Name] = true
					for _, tag := range rule.Tags {
						portForwardTags[tag] = true
					}
				} else {
					otherEnabledNames[*rule.Name] = true
				}
			}
		}
	}
	var bindings struct {
		Items *[]struct {
			Spec struct {
				Rules []struct {
					ID   string   `json:"ruleID"`
					Name string   `json:"ruleName"`
					Tags []string `json:"ruleTags"`
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	body, err = read(ctx, bindingsPath)
	if err != nil || json.Unmarshal(body, &bindings) != nil || bindings.Items == nil {
		return errEvidence
	}
	s.Bindings = len(*bindings.Items)
	for _, binding := range *bindings.Items {
		if binding.Spec.Rules == nil {
			return errEvidence
		}
		for _, rule := range binding.Spec.Rules {
			s.BoundEntries++
			// The released node-agent resolves ID first, then exact name, then any
			// matching tag. An invalid earlier selector cannot fall through.
			switch {
			case rule.ID != "":
				if enabled && rule.ID == "R2001" {
					s.PortForwardEnabledAndBound = true
				}
			case rule.Name != "":
				if portForwardNames[rule.Name] && otherEnabledNames[rule.Name] {
					return errEvidence
				}
				if portForwardNames[rule.Name] {
					s.PortForwardEnabledAndBound = true
				}
			default:
				for _, tag := range rule.Tags {
					if portForwardTags[tag] {
						s.PortForwardEnabledAndBound = true
					}
				}
			}
		}
	}
	return nil
}

func readAlerts(data json.RawMessage, s *snapshot) error {
	var v struct {
		Total    *int            `json:"total"`
		Firing   *int            `json:"firing"`
		Resolved *int            `json:"resolved"`
		Alerts   json.RawMessage `json:"alerts"`
	}
	if json.Unmarshal(data, &v) != nil || v.Total == nil || v.Firing == nil || v.Resolved == nil {
		return errEvidence
	}
	for _, count := range []int{*v.Total, *v.Firing, *v.Resolved} {
		if count < 0 || count > 1000000000 {
			return errEvidence
		}
	}
	var alerts []json.RawMessage
	if json.Unmarshal(v.Alerts, &alerts) != nil || alerts == nil || len(alerts) > 1 || len(alerts) > *v.Total {
		return errEvidence
	}
	s.UnresolvedAlerts = *v.Total
	if *v.Firing > s.Alerts {
		s.Alerts = *v.Firing
	}
	return nil
}

func observe(ctx context.Context, read readFunc, now time.Time) (snapshot, error) {
	// Coroot parses absolute milliseconds at whole-second resolution.
	windowEnd := now.UTC().Truncate(time.Second)
	s := snapshot{ObservedAt: now.UTC(), WindowFrom: windowEnd.Add(-time.Hour), WindowTo: windowEnd}
	body, err := read(ctx, corootProxy+"/api/user")
	if err != nil {
		return s, errors.New("project-discovery")
	}
	var user struct {
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if json.Unmarshal(body, &user) != nil {
		return s, errors.New("project-discovery")
	}
	var project string
	for _, p := range user.Projects {
		if p.Name != "platform" {
			continue
		}
		if project != "" || !projectIDPattern.MatchString(p.ID) {
			return s, errors.New("project-discovery")
		}
		project = p.ID
	}
	if project == "" {
		return s, errors.New("project-discovery")
	}
	query := url.Values{"from": {strconv.FormatInt(s.WindowFrom.UnixMilli(), 10)}, "to": {strconv.FormatInt(s.WindowTo.UnixMilli(), 10)}}.Encode()
	prefix := corootProxy + "/api/project/" + url.PathEscape(project) + "/overview/"
	alerts := map[string]int{}
	data, err := readEnvelope(ctx, read, prefix+"applications?"+query, &s, alerts)
	if err != nil || readApplications(data, &s) != nil {
		return s, errors.New("application-overview")
	}
	data, err = readEnvelope(ctx, read, prefix+"risks?"+query, &s, alerts)
	if err != nil || readRisks(data, &s) != nil {
		return s, errors.New("risk-overview")
	}
	// The dedicated alerts endpoint propagates database/query errors, whereas the
	// overview context silently substitutes an empty severity map on such an error.
	data, err = readEnvelope(ctx, read, corootProxy+"/api/project/"+url.PathEscape(project)+"/alerts?"+query+"&limit=1&include_resolved=false", &s, alerts)
	if err != nil || readAlerts(data, &s) != nil {
		return s, errors.New("alerts-query")
	}
	contextPeak := 0
	for _, count := range alerts {
		contextPeak += count
	}
	if contextPeak > s.Alerts {
		s.Alerts = contextPeak
	}
	if readRuleBaseline(ctx, read, &s) != nil {
		return s, errors.New("runtime-rule-readback")
	}
	// This snapshot neither queries every raw log event nor triggers a detector.
	// Presence of definitions and a binding is not end-to-end alert delivery.
	return s, nil
}

func allowedEnvironment(get func(string) string) bool {
	return get("GITHUB_REPOSITORY") == "devantler-tech/platform" && get("GITHUB_EVENT_NAME") == "workflow_dispatch" &&
		get("GITHUB_REF") == "refs/heads/main" && get("GITHUB_RUN_ATTEMPT") == "1" &&
		get("RUNTIME_HEALTH_CONFIRM") == "observe-runtime-health" && commitPattern.MatchString(get("GITHUB_SHA"))
}

type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 8*1024*1024 {
		return 0, errEvidence
	}
	return b.buffer.Write(p)
}

func kubectlRead(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--context=admin@prod", "--request-timeout=30s", "get", "--raw", path)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if cmd.Run() != nil {
		return nil, errEvidence
	}
	return output.Bytes(), nil
}

func main() {
	if len(os.Args) != 1 || !allowedEnvironment(os.Getenv) {
		fmt.Println("RUNTIME_HEALTH_OBSERVATION=UNVERIFIED")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := observe(ctx, kubectlRead, time.Now())
	if err != nil {
		stage := "unknown"
		switch err.Error() {
		case "project-discovery", "application-overview", "risk-overview", "alerts-query", "runtime-rule-readback":
			stage = err.Error()
		}
		fmt.Printf("RUNTIME_HEALTH_OBSERVATION=UNVERIFIED stage=%s\n", stage)
		os.Exit(2)
	}
	s.SourceSHA = os.Getenv("GITHUB_SHA")
	s.CompletedAt = time.Now().UTC()
	if json.NewEncoder(os.Stdout).Encode(s) != nil {
		os.Exit(2)
	}
}
