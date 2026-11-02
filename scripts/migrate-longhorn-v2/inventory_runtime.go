package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/grpc"
)

type inventoryOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (w *inventoryOutput) Write(b []byte) (int, error) {
	if len(b) > w.limit-w.buffer.Len() {
		w.exceeded = true
		return 0, errInventory
	}
	return w.buffer.Write(b)
}

func inventoryEndpoint(config *talosconfig.Config) (string, error) {
	if config == nil || config.Context == "" {
		return "", errInventory
	}
	selected := config.Contexts[config.Context]
	if selected == nil || selected.CA == "" || selected.Crt == "" || selected.Key == "" || selected.ProxyURL != "" || selected.Cluster != "" ||
		selected.Auth.Basic != nil || selected.Auth.SideroV1 != nil || len(selected.Endpoints) == 0 || len(selected.Endpoints) > 3 {
		return "", errInventory
	}
	// The SDK otherwise leaves RootCAs nil for base64 input that decodes to no
	// bytes. Require a nonempty exclusive cluster trust pool before constructing it.
	ca, err := base64.StdEncoding.DecodeString(selected.CA)
	if err != nil || len(ca) == 0 || !x509.NewCertPool().AppendCertsFromPEM(ca) {
		return "", errInventory
	}
	endpoints := slices.Clone(selected.Endpoints)
	slices.Sort(endpoints)
	for i, endpoint := range endpoints {
		if i > 0 && endpoint == endpoints[i-1] {
			return "", errInventory
		}
		host := endpoint
		if _, err := netip.ParseAddr(endpoint); err != nil && strings.Contains(endpoint, ":") {
			var port string
			host, port, err = net.SplitHostPort(endpoint)
			if err != nil || port != "50000" {
				return "", errInventory
			}
		}
		address, err := netip.ParseAddr(host)
		if err == nil {
			if address.Zone() != "" || address.String() != host {
				return "", errInventory
			}
		} else if len(host) > 253 || !kubeName.MatchString(host) || strings.ContainsAny(host, "/:@") {
			return "", errInventory
		}
	}
	return endpoints[0], nil
}

func inventoryReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil {
		return nil, errInventory
	}
	allowed := false
	switch program {
	case "kubectl":
		for _, query := range []struct{ kind, namespace string }{{"nodes", ""}, {"nodes.longhorn.io", "longhorn-system"}, {"replicas.longhorn.io", "longhorn-system"}} {
			expected := append([]string{"--context=admin@prod", "--request-timeout=20s", "get", query.kind, "--output=json", "--chunk-size=0"}, namespaceArgs(query.namespace)...)
			allowed = allowed || slices.Equal(args, expected)
		}
	case "talosctl":
		if len(args) == 5 && strings.HasPrefix(args[0], "--nodes=") && slices.Equal(args[1:], []string{"get", "systeminformation", "systeminformation", "--output=json"}) {
			address, err := netip.ParseAddr(strings.TrimPrefix(args[0], "--nodes="))
			allowed = err == nil && netip.MustParsePrefix("10.0.0.0/16").Contains(address)
		}
	case "git":
		allowed = slices.Equal(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"})
	}
	if !allowed {
		return nil, errInventory
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, stderr := inventoryOutput{limit: 16 << 20}, inventoryOutput{limit: 64 << 10}
	command := exec.CommandContext(ctx, program, args...)
	command.Stdout, command.Stderr = &stdout, &stderr
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil || ctx.Err() != nil || stdout.exceeded || stderr.exceeded {
		return nil, errInventory
	}
	return stdout.buffer.Bytes(), nil
}

func newInventoryReader(ctx context.Context, address string) (nativeReplicaReader, func() error, error) {
	directory := os.Getenv("LONGHORN_V2_CREDENTIAL_DIR")
	if !regexp.MustCompile(`^/dev/shm/longhorn-v2\.[A-Za-z0-9]+$`).MatchString(directory) || os.Getenv("TALOSCONFIG") != filepath.Join(directory, "talosconfig") {
		return nil, nil, errInventory
	}
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return nil, nil, errInventory
	}
	path := filepath.Join(directory, "talosconfig")
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || before.Size() <= 0 || before.Size() > 512<<10 {
		return nil, nil, errInventory
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errInventory
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, (512<<10)+1))
	closeErr := file.Close()
	after, lastErr := os.Lstat(path)
	if statErr != nil || readErr != nil || closeErr != nil || lastErr != nil || len(data) != int(before.Size()) ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil, errInventory
	}
	config, err := talosconfig.FromBytes(data)
	if err != nil {
		return nil, nil, errInventory
	}
	endpoint, err := inventoryEndpoint(config)
	if err != nil {
		return nil, nil, err
	}
	connection, err := talos.New(ctx, talos.WithConfig(config), talos.WithContextName(config.Context), talos.WithEndpoints(endpoint), talos.WithGRPCDialOptions(grpc.WithNoProxy()))
	if err != nil {
		return nil, nil, errInventory
	}
	return longhorninventory.Native{API: connection.MachineClient, Target: address}, connection.Close, nil
}

func runReplicaInventory(ctx context.Context, target, source, runID string) error {
	c := client{run: inventoryReadCommand, context: "admin@prod"}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return errInventory
	}
	receipt, err := observeReplicaInventory(ctx, c, target, newInventoryReader)
	if err != nil {
		return errInventory
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return errInventory
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Source    string           `json:"source"`
		Run       string           `json:"run"`
		Inventory inventoryReceipt `json:"inventory"`
	}{source, runID, receipt})
}
