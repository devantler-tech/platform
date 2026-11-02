package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

var controlNamePattern = regexp.MustCompile(`^longhorn-cache-(signed-before|unsigned|signed-after)-[1-9][0-9]{0,19}$`)

func commandAllowed(program string, args []string, input []byte, env map[string]string) bool {
	if program == "kubectl" && len(env) == 0 {
		if len(args) == 6 && reflect.DeepEqual(args[:5], []string{"--context", "admin@prod", "--request-timeout=15s", "get", "--raw"}) && len(input) == 0 {
			if args[5] == "/api/v1/namespaces/kyverno/secrets/ghcr-auth" {
				return true
			}
			for _, target := range publicReads {
				if args[5] == target.path {
					return true
				}
			}
		}
		if reflect.DeepEqual(args, []string{"--context", "admin@prod", "--request-timeout=45s", "create", "--dry-run=server", "--validate=strict", "--namespace", "default", "--filename", "-", "-o", "json"}) {
			pod, err := decodeObject(input)
			if err != nil || text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" || text(pod, "metadata", "namespace") != "default" || !controlNamePattern.MatchString(text(pod, "metadata", "name")) || at(pod, "spec", "automountServiceAccountToken") != false || at(pod, "spec", "volumes") != nil {
				return false
			}
			containers, ok := at(pod, "spec", "containers").([]any)
			if !ok || len(containers) != 1 {
				return false
			}
			want := signedImage
			if strings.HasPrefix(text(pod, "metadata", "name"), "longhorn-cache-unsigned-") {
				want = unsignedImage
			}
			return text(containers[0], "name") == "probe" && text(containers[0], "image") == want
		}
	}
	if program != "bash" || len(input) != 0 {
		return false
	}
	if len(args) == 2 && args[0] == "./scripts/verify-prod-recovery-source.sh" && sourcePattern.MatchString(args[1]) && len(env) == 0 {
		return true
	}
	return reflect.DeepEqual(args, []string{"./scripts/verify-image-unsigned.sh", "--image", unsignedImage}) && len(env) == 3 && env["REGISTRY_USERNAME"] != "" && env["REGISTRY_PASSWORD"] != "" && !strings.ContainsAny(env["REGISTRY_USERNAME"]+env["REGISTRY_PASSWORD"], "\r\n") && credentialPattern.MatchString(env["TMPDIR"])
}

type boundedCommandOutput struct {
	mu        sync.Mutex
	data      bytes.Buffer
	truncated bool
}

func (b *boundedCommandOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated || len(p) > 2*1024*1024-b.data.Len() {
		b.truncated = true
		b.data.Reset()
		return 0, errors.New("command output exceeded its bound")
	}
	return b.data.Write(p)
}
func (b *boundedCommandOutput) result() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return nil, true
	}
	return append([]byte(nil), b.data.Bytes()...), false
}

func execute(ctx context.Context, program string, args []string, input []byte, env map[string]string) ([]byte, error) {
	if !commandAllowed(program, args, input, env) {
		return nil, errors.New("command is outside the fixed read and dry-run scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := active(ctx); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stdin = bytes.NewReader(input)
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.New("could not open command response pipe")
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	var output boundedCommandOutput
	// Own the pipe so Cmd.Wait cannot turn a forced pipe close into an
	// apparently complete exit-1 admission denial.
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("fixed command could not start")
	}
	_ = writer.Close()
	drained := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&output, reader)
		drained <- copyErr
	}()
	err = cmd.Wait()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var drainErr error
	select {
	case drainErr = <-drained:
	case <-ctx.Done():
		drainErr = ctx.Err()
		_ = reader.Close()
		<-drained
	case <-timer.C:
		drainErr = errors.New("command response did not reach EOF")
		_ = reader.Close()
		<-drained
	}
	raw, truncated := output.result()
	if truncated || drainErr != nil {
		return nil, errors.New("command response is incomplete")
	}
	if deadlineErr := active(ctx); deadlineErr != nil {
		return nil, deadlineErr
	}
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return nil, errors.New("fixed command could not complete")
		}
		if exitError.ExitCode() != 1 {
			return nil, err
		}
	}
	return raw, err
}
