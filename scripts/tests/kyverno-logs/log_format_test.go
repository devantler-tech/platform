package kyvernologs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/coroot/logparser"
	"gopkg.in/yaml.v3"
)

const coloredTrace = "\x1b[90m2000-01-01T05:03:39Z\x1b[0m \x1b[34mTRC\x1b[0m \x1b[1mgithub.com/kyverno/kyverno/cmd/internal/flag.go:335\x1b[0m\x1b[36m >\x1b[0m \x1b[36mallowedResults=\x1b[0mpass,fail,error,warn,skip \x1b[36mlogger=\x1b[0msetup/flag \x1b[36mv=\x1b[0m2"

// renderArguments uses the declared chart and values, not a substitute renderer.
func renderArguments(t *testing.T, baseline bool) map[string][]string {
	t.Helper()
	helmbin := os.Getenv("KYVERNO_LOG_HELM_BIN")
	if helmbin == "" {
		var err error
		helmbin, err = exec.LookPath("helm")
		if err != nil {
			if os.Getenv("KYVERNO_LOG_REQUIRE_NATIVE") == "1" {
				t.Fatal("native Helm is required")
			}
			t.Skip("native Helm is unavailable")
		}
	}
	root := filepath.Join("..", "..", "..")
	read := func(name string) map[string]any {
		b, err := os.ReadFile(filepath.Join(root, "k8s/bases/infrastructure/controllers/kyverno", name))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	spec := read("helm-release.yaml")["spec"].(map[string]any)
	chart := spec["chart"].(map[string]any)["spec"].(map[string]any)
	values := spec["values"].(map[string]any)
	for name, replicas := range map[string]int{"admissionController": 2, "backgroundController": 1, "reportsController": 1, "cleanupController": 1} {
		// Resolve the authored local/CI replica defaults; no production access.
		values[name].(map[string]any)["replicas"] = replicas
	}
	if baseline {
		delete(values["features"].(map[string]any), "logging")
		if overrides, ok := values["admissionController"].(map[string]any)["featuresOverride"].(map[string]any); ok {
			delete(overrides, "logging")
		}
	}
	b, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	url := read("helm-repository.yaml")["spec"].(map[string]any)["url"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helmbin, "template", "kyverno", chart["chart"].(string), "--repo", url, "--version", chart["version"].(string), "--namespace", "kyverno", "--values", "-")
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("native chart render failed: %v", err)
	}
	result := map[string][]string{}
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Args []string `yaml:"args"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if doc.Kind == "Deployment" {
			if len(doc.Spec.Template.Spec.Containers) != 1 {
				t.Fatal("controller container coverage changed")
			}
			result[doc.Metadata.Name] = doc.Spec.Template.Spec.Containers[0].Args
		}
	}
	if len(result) != 4 {
		t.Fatalf("expected four controller Deployments, got %d", len(result))
	}
	return result
}

type consumedLog struct {
	logparser.LogEntry
	attributes map[string]string
}

// consume exercises the deployed CRI decoder and parser callback, preserving fields.
func consume(t *testing.T, body string) consumedLog {
	t.Helper()
	requireVerifiedConsumer(t)
	entries := make(chan logparser.LogEntry, 1)
	results := make(chan consumedLog, 1)
	parser := logparser.NewParser(entries, logparser.CriDecoder{}, func(timestamp time.Time, level logparser.Level, _, message string, attributes map[string]string) {
		results <- consumedLog{LogEntry: logparser.LogEntry{Timestamp: timestamp, Level: level, Content: message}, attributes: attributes}
	}, 5*time.Millisecond, 256, true, nil)
	defer parser.Stop()
	entries <- logparser.LogEntry{Timestamp: time.Unix(100, 0), Content: "2000-01-01T05:03:39Z stderr F " + body, Level: logparser.LevelUnknown}
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("native CRI consumer timed out")
		return consumedLog{}
	}
}

// TestControllerProducerArguments rejects colored text from any controller,
// while retaining the original admission regression and every other argument.
func TestControllerProducerArguments(t *testing.T) {
	current, baseline := renderArguments(t, false), renderArguments(t, true)
	for _, name := range []string{"kyverno-admission-controller", "kyverno-background-controller", "kyverno-reports-controller", "kyverno-cleanup-controller"} {
		t.Run(name, func(t *testing.T) {
			want := append([]string(nil), baseline[name]...)
			replacements := 0
			for i, arg := range want {
				if arg == "--loggingFormat=text" {
					want[i], replacements = "--loggingFormat=json", replacements+1
				}
			}
			if replacements != 1 || !reflect.DeepEqual(current[name], want) {
				t.Fatal("controller must change only text to JSON; verbosity and other arguments must remain identical")
			}
		})
	}
}

// TestCRISeverity exercises the pinned consumer without requiring Helm.
func TestCRISeverity(t *testing.T) {
	if got := consume(t, coloredTrace); got.Level != logparser.LevelError {
		t.Fatal("the pinned colored-trace defect changed; recharacterize before altering the producer")
	}
	for _, tc := range []struct {
		level string
		want  logparser.Level
	}{
		{"trace", logparser.LevelDebug}, {"debug", logparser.LevelDebug}, {"info", logparser.LevelInfo}, {"warn", logparser.LevelWarning}, {"error", logparser.LevelError}, {"fatal", logparser.LevelCritical},
	} {
		t.Run(tc.level, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"level": tc.level, "time": "2000-01-01T05:03:39Z", "message": "configuration includes error and warn", "allowedResults": "pass,fail,error,warn,skip", "logger": "setup/flag", "v": 2})
			if err != nil {
				t.Fatal(err)
			}
			got := consume(t, string(body))
			if got.Level != tc.want || got.Content != "configuration includes error and warn" || got.attributes["allowedResults"] != "pass,fail,error,warn,skip" {
				t.Fatalf("structured severity or original flag value changed: %s %q", got.Level, got.Content)
			}
		})
	}
}

// A resource kind called Alert must not promote a deprecation notice to fatal.
// Characterize the observed colored-text defect and exercise its JSON replacement.
func TestCRIDeprecationSeverity(t *testing.T) {
	const message = "Warning: v1beta2 Alert is deprecated, upgrade to v1beta3"
	colored := "\x1b[90m2000-01-01T05:03:39Z\x1b[0m \x1b[32mINF\x1b[0m \x1b[1mk8s.io/client-go@v0.36.3/rest/warnings.go:107\x1b[0m\x1b[36m >\x1b[0m \x1b[1m" + message + "\x1b[0m \x1b[36mlogger=\x1b[0mklog \x1b[36mv=\x1b[0m0"
	if got := consume(t, colored); got.Level != logparser.LevelCritical {
		t.Fatalf("observed colored deprecation defect changed: level=%s", got.Level)
	}
	body, err := json.Marshal(map[string]any{"level": "info", "time": "2000-01-01T05:03:39Z", "message": message, "logger": "klog", "v": 0})
	if err != nil {
		t.Fatal(err)
	}
	got := consume(t, string(body))
	if got.Level != logparser.LevelInfo || got.Content != message || got.attributes["logger"] != "klog" {
		t.Fatalf("deprecation severity or content changed: level=%s message=%q", got.Level, got.Content)
	}
}
