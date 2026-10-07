// verify-arc-app-identity is an opt-in, protected identity check. It never
// creates an App, changes a permission, registers a runner or installs ARC.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const baoTLSName = "openbao-arc.openbao.svc.cluster.local"

func main() {
	os.Exit(run(os.Args[1:], ".", os.Stdout, runtimeModes{identity: verifyProduction, transport: verifyTransportProduction}))
}

type command func(context.Context, ...string) ([]byte, error)

func kubectl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context=admin@prod"}, args...)...)
	// Capture neither errors nor bodies in a transcript. In particular, the
	// short-lived reader JWT is returned directly to the verifier in memory.
	cmd.Stderr = io.Discard
	var output boundedOutput
	cmd.Stdout = &output
	if cmd.Run() != nil {
		return nil, fmt.Errorf("cluster operation unavailable")
	}
	return output.Bytes(), nil
}

type boundedOutput struct{ bytes.Buffer }

func (output *boundedOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > maxResponse {
		return 0, fmt.Errorf("response too large")
	}
	return output.Buffer.Write(data)
}

func liveConfiguration(ctx context.Context, reviewed configuration, execute command) ([]byte, outcome) {
	bootstrap, err := execute(ctx, "--namespace=flux-system", "get", "configmap", "variables-cluster", "--output=json")
	if err != nil {
		return nil, failConfig
	}
	var value map[string]any
	if len(bootstrap) > maxResponse || validateJSON(bootstrap) != nil || json.Unmarshal(bootstrap, &value) != nil {
		return nil, failConfig
	}
	clientID, status := parseBootstrap(value)
	if status != pass || clientID != reviewed.clientID {
		return nil, failIdentity
	}
	store, err := execute(ctx, "--namespace=arc-runners", "get", "secretstore", "openbao", "--output=json")
	if err != nil {
		return nil, failConfig
	}
	if len(store) > maxResponse || validateJSON(store) != nil || json.Unmarshal(store, &value) != nil {
		return nil, failConfig
	}
	live, status := parseStore(value)
	if status != pass {
		return nil, status
	}
	if live != reviewed.store {
		return nil, failConfig
	}
	if live.caBundle != "" {
		ca, err := base64.StdEncoding.DecodeString(live.caBundle)
		if err != nil {
			return nil, holdTransport
		}
		return ca, pass
	}
	caResponse, err := execute(ctx, "--namespace=arc-runners", "get", "configmap", live.caName, "--output=json")
	if err != nil || len(caResponse) > maxResponse || validateJSON(caResponse) != nil || json.Unmarshal(caResponse, &value) != nil || !namedResource(value, "ConfigMap", live.caName, "arc-runners") {
		return nil, holdTransport
	}
	ca := []byte(stringValue(object(value["data"])[live.caKey]))
	if trustedRoots(ca) == nil {
		return nil, holdTransport
	}
	return ca, pass
}

func verifyProduction(reviewed configuration) outcome {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return verifyRuntime(ctx, reviewed, runtimeOperations{execute: kubectl, forward: forwardBao, handshake: listenerTLS, identity: verify})
}

type runtimeOperations struct {
	execute   command
	forward   func(context.Context, int) (string, func(), error)
	handshake func(context.Context, string, []byte) error
	identity  func(context.Context, verificationOptions) outcome
	health    func(context.Context, string, []byte) outcome
}

func verifyRuntime(ctx context.Context, reviewed configuration, operations runtimeOperations) outcome {
	ca, status := liveConfiguration(ctx, reviewed, operations.execute)
	if status != pass {
		return status
	}
	roots := trustedRoots(ca)
	if roots == nil {
		return holdTransport
	}
	port, ok := baoListenerPort(reviewed.store.server)
	if !ok {
		return holdTransport
	}
	endpoint, stop, err := operations.forward(ctx, port)
	if err != nil {
		return failTransport
	}
	defer stop()
	// A loopback tunnel carries the listener's real TLS session unchanged. It
	// supplements, never substitutes for, the reviewed AND live HTTPS SecretStore.
	if operations.handshake(ctx, endpoint, ca) != nil {
		return failTransport
	}
	// Transport is verified before obtaining any credential-bearing token.
	jwt, err := operations.execute(ctx, "--namespace=arc-runners", "create", "token", "arc-secret-reader", "--duration=10m")
	if err != nil || len(jwt) > maxResponse {
		return "HOLD_READER"
	}
	reader := strings.TrimSpace(string(jwt))
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`).MatchString(reader) {
		return "HOLD_READER"
	}
	return operations.identity(ctx, verificationOptions{baoURL: endpoint, githubURL: "https://api.github.com", expectedClientID: reviewed.clientID, readerJWT: reader, client: secureClient(nil, ""), baoClient: secureClient(roots, baoTLSName), now: time.Now})
}

func listenerTLS(ctx context.Context, endpoint string, ca []byte) error {
	if !httpsOrigin(endpoint) || trustedRoots(ca) == nil {
		return fmt.Errorf("transport unavailable")
	}
	address := strings.TrimPrefix(endpoint, "https://")
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trustedRoots(ca), ServerName: baoTLSName}}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("transport unavailable")
	}
	return connection.Close()
}

func forwardBao(parent context.Context, port int) (string, func(), error) {
	if port != baoTLSPort {
		return "", nil, fmt.Errorf("transport unavailable")
	}
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, "kubectl", "--context=admin@prod", "--namespace=openbao", "port-forward", "--address=127.0.0.1", "service/openbao-arc", fmt.Sprintf("0:%d", port))
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, fmt.Errorf("transport unavailable")
	}
	if cmd.Start() != nil {
		cancel()
		return "", nil, fmt.Errorf("transport unavailable")
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	stop := func() { cancel(); <-waited }
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024), 4096)
		reported := false
		for scanner.Scan() {
			if endpoint := forwardingAddress(scanner.Text()); endpoint != "" && !reported {
				ready <- endpoint
				reported = true
			}
		}
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case endpoint := <-ready:
		return endpoint, stop, nil
	case <-ctx.Done():
		stop()
		return "", nil, fmt.Errorf("transport unavailable")
	case <-timer.C:
		stop()
		return "", nil, fmt.Errorf("transport unavailable")
	case <-waited:
		stop()
		return "", nil, fmt.Errorf("transport unavailable")
	}
}

func forwardingAddress(line string) string {
	// kubectl resolves a Service port to its pod targetPort. Only the allocated
	// loopback endpoint is needed here; the fixed command and TLS hostname/CA
	// checks bind the destination, rather than this informational target number.
	pattern := regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:([0-9]{1,5}) -> ([0-9]{1,5})$`)
	match := pattern.FindStringSubmatch(line)
	if len(match) != 3 {
		return ""
	}
	for _, value := range match[1:] {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != value {
			return ""
		}
	}
	return "https://127.0.0.1:" + match[1]
}
