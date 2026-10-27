// Main-only, bounded observation of the existing Email serving container.
// Raw Kubernetes/probe data and native errors never become public output.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"time"
)

const approvedImage = "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:e51d4d1f574405910f6f070d50efabc5e3c84ec65c33b44e5fad1684e25030d8"
const maxOutput = 16384
const maxAPIOutput = 1048576
const publicConfig = `{"@type":"RocksDb","path":"/var/lib/stalwart/data","bufferSize":33554432,"cacheSize":33554432}`

//go:embed probe.sh
var podProbe string

type measurement struct {
	PID1Stalwart        bool     `json:"pid1_stalwart"`
	UID2000             bool     `json:"uid2000"`
	ConfigSHA256        string   `json:"config_sha256"`
	CertificateReadable bool     `json:"certificate_readable"`
	KeyReadable         bool     `json:"key_readable"`
	DataReadable        bool     `json:"data_readable"`
	DataWritable        bool     `json:"data_writable"`
	TemporaryWritable   bool     `json:"temporary_writable"`
	LockDevice          uint64   `json:"lock_device"`
	LockInode           uint64   `json:"lock_inode"`
	WriteLocks          []string `json:"write_locks"`
	ManagementListeners int      `json:"management_listeners"`
	RecoveryListeners   int      `json:"recovery_listeners"`
	RecoveryMode        bool     `json:"recovery_mode"`
	LocalHealth         int      `json:"local_health"`
}
type metadata struct{ Name, Namespace, UID, ResourceVersion string }
type container struct {
	Name, Image   string
	Command, Args []string
	VolumeMounts  []struct {
		Name, MountPath, SubPath string
		ReadOnly                 bool
	}
}
type pod struct {
	Metadata metadata
	Spec     struct {
		NodeName              string
		ShareProcessNamespace bool
		Containers            []container
		Volumes               []struct {
			Name      string
			ConfigMap *struct{ Name string }
		}
	}
	Status struct {
		ContainerStatuses []struct {
			Name, ContainerID, ImageID string
			RestartCount               int
			Ready                      bool
			State                      struct{ Running *struct{ StartedAt string } }
		}
	}
}
type binding struct {
	UID, Node, ContainerID, ImageID, StartedAt string
	Restarts                                   int
}
type configMap struct {
	Metadata metadata
	Data     map[string]string
}
type executor func(context.Context, ...string) ([]byte, error)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var lockPattern = regexp.MustCompile(`^[0-9a-f]+:[0-9a-f]+:[0-9]+$`)

// hashBytes identifies only the already validated public startup configuration.
func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// linuxDevice decodes Linux/glibc dev_t, independently of the observer host OS.
func linuxDevice(d uint64) string {
	major := ((d >> 8) & 0xfff) | ((d >> 32) & 0xfffff000)
	minor := (d & 0xff) | ((d >> 12) & 0xffffff00)
	return fmt.Sprintf("%02x:%02x", major, minor)
}

// classify returns one canned observation, never a raw field or error payload.
func classify(m measurement, configMatches, podReady bool) string {
	switch {
	case !m.PID1Stalwart || !m.UID2000:
		return "UNEXPECTED_RUNTIME_IDENTITY"
	case !configMatches:
		return "MOUNTED_CONFIGURATION_DIFFERS"
	case !m.CertificateReadable || !m.KeyReadable || !m.DataReadable || !m.DataWritable || !m.TemporaryWritable:
		return "REQUIRED_RUNTIME_INPUT_UNAVAILABLE"
	case !contains(m.WriteLocks, fmt.Sprintf("%s:%d", linuxDevice(m.LockDevice), m.LockInode)):
		return "DATASTORE_NOT_OBSERVED_OPEN"
	case m.RecoveryMode || m.RecoveryListeners > 0:
		return "UNEXPECTED_RECOVERY_MODE"
	case m.ManagementListeners == 0:
		return "MANAGEMENT_LISTENER_ABSENT"
	case m.LocalHealth != 200:
		return "LOCAL_HEALTH_NOT_READY"
	case !podReady:
		return "LOCAL_HEALTHY_KUBERNETES_UNREADY"
	default:
		return "OBSERVED_HEALTHY_SAMPLE"
	}
}

// contains compares complete kernel lock identities, including device and inode.
func contains(all []string, want string) bool {
	for _, value := range all {
		if value == want {
			return true
		}
	}
	return false
}

// decodeMeasurement requires one complete flat schema with no duplicate keys.
func decodeMeasurement(b []byte) (measurement, error) {
	var m measurement
	invalid := errors.New("invalid measurement")
	if len(b) == 0 || len(b) > maxOutput {
		return m, invalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return m, invalid
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return m, invalid
		}
		name, ok := key.(string)
		if !ok {
			return m, invalid
		}
		if _, exists := fields[name]; exists {
			return m, invalid
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return m, invalid
		}
		fields[name] = value
	}
	if _, e = d.Token(); e != nil {
		return m, invalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return m, invalid
	}
	typ := reflect.TypeOf(m)
	if len(fields) != typ.NumField() {
		return m, invalid
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := fields[typ.Field(i).Tag.Get("json")]; !ok {
			return m, invalid
		}
	}
	strict := json.NewDecoder(bytes.NewReader(b))
	strict.DisallowUnknownFields()
	if strict.Decode(&m) != nil {
		return m, invalid
	}
	if !digestPattern.MatchString(m.ConfigSHA256) || m.LockInode == 0 || m.WriteLocks == nil || len(m.WriteLocks) > 256 ||
		m.ManagementListeners < 0 || m.ManagementListeners > 128 || m.RecoveryListeners < 0 || m.RecoveryListeners > 128 ||
		(m.LocalHealth != 0 && (m.LocalHealth < 100 || m.LocalHealth > 599)) {
		return m, invalid
	}
	for _, lock := range m.WriteLocks {
		if !lockPattern.MatchString(lock) {
			return m, invalid
		}
	}
	return m, nil
}

// bindPod refuses a different executable, image, public config mount or identity.
func bindPod(b []byte) (binding, bool, bool, error) {
	var p pod
	invalid := errors.New("invalid pod")
	if len(b) == 0 || len(b) > maxAPIOutput || json.Unmarshal(b, &p) != nil {
		return binding{}, false, false, invalid
	}
	if p.Metadata.Name != "private-mail-0" || p.Metadata.Namespace != "private-mail" || !uuidPattern.MatchString(p.Metadata.UID) || p.Metadata.ResourceVersion == "" ||
		p.Spec.NodeName == "" || p.Spec.ShareProcessNamespace || len(p.Spec.Containers) != 1 || len(p.Status.ContainerStatuses) != 1 {
		return binding{}, false, false, invalid
	}
	c := p.Spec.Containers[0]
	if c.Name != "private-mail" || c.Image != approvedImage || !reflect.DeepEqual(c.Command, []string{"/usr/local/bin/stalwart"}) ||
		!reflect.DeepEqual(c.Args, []string{"--config", "/etc/stalwart/config.json"}) {
		return binding{}, false, false, invalid
	}
	mounted := false
	for _, mount := range c.VolumeMounts {
		if mount.MountPath == "/etc/stalwart/config.json" && mount.SubPath == "config.json" && mount.ReadOnly {
			for _, volume := range p.Spec.Volumes {
				if volume.Name == mount.Name && volume.ConfigMap != nil && volume.ConfigMap.Name == "private-mail-datastore" {
					mounted = true
				}
			}
		}
	}
	if !mounted {
		return binding{}, false, false, invalid
	}
	s := p.Status.ContainerStatuses[0]
	if s.Name != "private-mail" || s.RestartCount < 0 {
		return binding{}, false, false, invalid
	}
	id := binding{UID: p.Metadata.UID, Node: p.Spec.NodeName, ContainerID: s.ContainerID, ImageID: s.ImageID, Restarts: s.RestartCount}
	if s.State.Running == nil {
		return id, false, s.Ready, nil
	}
	if s.ContainerID == "" || s.ImageID == "" {
		return binding{}, false, false, invalid
	}
	if _, e := time.Parse(time.RFC3339, s.State.Running.StartedAt); e != nil {
		return binding{}, false, false, invalid
	}
	id.StartedAt = s.State.Running.StartedAt
	return id, true, s.Ready, nil
}

// readPublicMap rejects unknown configuration fields before hashing the mounted file.
func readPublicMap(b []byte) (configMap, error) {
	var cm configMap
	invalid := errors.New("invalid public config")
	if len(b) == 0 || len(b) > maxAPIOutput || json.Unmarshal(b, &cm) != nil {
		return cm, invalid
	}
	if cm.Metadata.Name != "private-mail-datastore" || cm.Metadata.Namespace != "private-mail" || !uuidPattern.MatchString(cm.Metadata.UID) || cm.Metadata.ResourceVersion == "" || len(cm.Data) != 1 || len(cm.Data["config.json"]) > 8192 {
		return cm, invalid
	}
	var actual, expected any
	if json.Unmarshal([]byte(cm.Data["config.json"]), &actual) != nil || json.Unmarshal([]byte(publicConfig), &expected) != nil || !reflect.DeepEqual(actual, expected) {
		return cm, invalid
	}
	return cm, nil
}

// request always uses one fixed workflow context and finite Kubernetes API deadlines.
func request(ctx context.Context, run executor, args ...string) ([]byte, error) {
	return run(ctx, append([]string{"--context", "admin@prod", "--request-timeout=10s"}, args...)...)
}

// collect permits one fixed exec only after a verified running identity is observed.
func collect(ctx context.Context, run executor, pause func(context.Context) error) (string, int) {
	unknown := func() (string, int) { return "INCONCLUSIVE", 3 }
	var before binding
	found := false
	for attempt := 0; attempt < 11; attempt++ {
		raw, e := request(ctx, run, "get", "pod", "private-mail-0", "-n", "private-mail", "-o", "json")
		if e != nil {
			return unknown()
		}
		id, running, _, e := bindPod(raw)
		if e != nil {
			return unknown()
		}
		if running {
			before = id
			found = true
			break
		}
		if attempt < 10 && pause(ctx) != nil {
			return unknown()
		}
	}
	if !found {
		return unknown()
	}
	raw, e := request(ctx, run, "get", "configmap", "private-mail-datastore", "-n", "private-mail", "-o", "json")
	if e != nil {
		return unknown()
	}
	cm, e := readPublicMap(raw)
	if e != nil {
		return unknown()
	}
	raw, e = request(ctx, run, "exec", "-n", "private-mail", "private-mail-0", "-c", "private-mail", "--", "/bin/bash", "--noprofile", "--norc", "-euo", "pipefail", "-c", podProbe)
	if e != nil {
		return unknown()
	}
	observed, e := decodeMeasurement(raw)
	if e != nil {
		return unknown()
	}
	raw, e = request(ctx, run, "get", "pod", "private-mail-0", "-n", "private-mail", "-o", "json")
	if e != nil {
		return unknown()
	}
	after, running, ready, e := bindPod(raw)
	if e != nil || !running || after != before {
		return unknown()
	}
	raw, e = request(ctx, run, "get", "configmap", "private-mail-datastore", "-n", "private-mail", "-o", "json")
	if e != nil {
		return unknown()
	}
	afterMap, e := readPublicMap(raw)
	if e != nil || afterMap.Metadata != cm.Metadata || !reflect.DeepEqual(afterMap.Data, cm.Data) {
		return unknown()
	}
	verdict := classify(observed, observed.ConfigSHA256 == hashBytes([]byte(cm.Data["config.json"])), ready)
	if verdict == "OBSERVED_HEALTHY_SAMPLE" {
		return verdict, 0
	}
	return verdict, 1
}

// runEntry enforces the workflow contract before any client or credential access.
func runEntry(ctx context.Context, env func(string) string, run executor, pause func(context.Context) error, out io.Writer) int {
	verdict, code := "INCONCLUSIVE", 3
	if env("GITHUB_REPOSITORY") == "devantler-tech/platform" && env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_REF") == "refs/heads/main" && env("GITHUB_RUN_ATTEMPT") == "1" && env("MAIL_STARTUP_CONFIRM") == "read-email-startup-state" {
		verdict, code = collect(ctx, run, pause)
	}
	line := fmt.Sprintf("MAIL_STARTUP_SAMPLE=%s\n", verdict)
	if n, e := io.WriteString(out, line); e != nil || n != len(line) {
		return 3
	}
	return code
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

// Write refuses an oversized response rather than returning truncated evidence.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("output bound")
	}
	return b.Buffer.Write(p)
}

// execute captures bounded stdout and suppresses all native error payloads.
func execute(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	limit := maxAPIOutput
	if len(args) > 3 && args[3] == "exec" {
		limit = maxOutput
	}
	out := &boundedBuffer{limit: limit}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 3 * time.Second
	if e := cmd.Run(); e != nil {
		return nil, errors.New("client failed")
	}
	return out.Bytes(), nil
}

// wait observes a backoff interval without retrying a failed exec or API request.
func wait(ctx context.Context) error {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// main gives the entire observation a finite five-minute lifetime.
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	code := runEntry(ctx, os.Getenv, execute, wait, os.Stdout)
	cancel()
	os.Exit(code)
}
