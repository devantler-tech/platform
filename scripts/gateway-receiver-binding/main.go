package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const destination = "k8s/clusters/prod/bootstrap/secret-variables-gateway-receiver.enc.yaml"
const recipient = "age1rk6fs67kly3h4zux5za429z3grjtvs8vcunav4sa28pxk738najsk8wgs6"

func read(ctx context.Context, kubeContext, kind, name string) ([]byte, error) {
	return readNamespace(ctx, kubeContext, "kube-system", kind, name)
}
func readNamespace(ctx context.Context, kubeContext, namespace, kind, name string) ([]byte, error) {
	args := []string{"--context=" + kubeContext, "--request-timeout=20s"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "get", kind, name, "-o=json")
	command := exec.CommandContext(ctx, "kubectl", args...)
	command.Stderr = io.Discard
	result, err := command.Output()
	if err != nil || len(result) > 512<<10 {
		return nil, errors.New("metadata-read")
	}
	return result, nil
}
func collect(ctx context.Context, kubeContext string) (binding, error) {
	return collectExpected(ctx, kubeContext, false)
}

func collectExpected(ctx context.Context, kubeContext string, wantProxy bool) (binding, error) {
	s, err := read(ctx, kubeContext, "service", "cilium-gateway-platform")
	if err != nil {
		return binding{}, err
	}
	g, err := read(ctx, kubeContext, "gateway", "platform")
	if err != nil {
		return binding{}, err
	}
	return deriveExpectedTransport(s, g, wantProxy, false)
}
func plaintext(b binding) ([]byte, error) {
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": "variables-gateway-receiver", "namespace": "flux-system"}, "type": "Opaque", "stringData": map[string]string{"gateway_receiver_source_cidr": b.CIDR, "gateway_receiver_service_uid": b.ServiceUID, "gateway_receiver_gateway_uid": b.GatewayUID}})
}
func encrypted(raw []byte, b binding) bool {
	if len(raw) == 0 || len(raw) > 32768 || bytes.Contains(raw, []byte(b.CIDR)) || bytes.Contains(raw, []byte(b.ServiceUID)) || bytes.Contains(raw, []byte(b.GatewayUID)) {
		return false
	}
	var o struct {
		APIVersion string `yaml:"apiVersion"`
		Kind, Type string
		Metadata   struct{ Name, Namespace string }
		StringData map[string]string `yaml:"stringData"`
		Sops       struct {
			Age []struct{ Recipient string }
			Mac string
		} `yaml:"sops"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&o) != nil {
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return false
	}
	if o.APIVersion != "v1" || o.Kind != "Secret" || o.Type != "Opaque" || o.Metadata.Name != "variables-gateway-receiver" || o.Metadata.Namespace != "flux-system" || len(o.StringData) != 3 || len(o.Sops.Age) != 1 || o.Sops.Age[0].Recipient != recipient || !strings.HasPrefix(o.Sops.Mac, "ENC[AES256_GCM,") {
		return false
	}
	for _, key := range []string{"gateway_receiver_source_cidr", "gateway_receiver_service_uid", "gateway_receiver_gateway_uid"} {
		if !strings.HasPrefix(o.StringData[key], "ENC[AES256_GCM,") {
			return false
		}
	}
	return true
}
func prepare(ctx context.Context, kubeContext, outputPath string) error {
	if _, err := os.Lstat(outputPath); !os.IsNotExist(err) {
		return errors.New("destination-exists")
	}
	first, err := collect(ctx, kubeContext)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "gateway-receiver-binding-")
	if err != nil {
		return errors.New("private-workspace")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rules, err := os.ReadFile(".sops.yaml")
	if err != nil {
		return errors.New("encryption-policy")
	}
	if os.WriteFile(filepath.Join(dir, ".sops.yaml"), rules, 0600) != nil || os.MkdirAll(filepath.Join(dir, filepath.Dir(destination)), 0700) != nil {
		return errors.New("private-workspace")
	}
	plain, err := plaintext(first)
	if err != nil {
		return errors.New("input")
	}
	defer clear(plain)
	path := filepath.Join(dir, destination)
	if os.WriteFile(path, plain, 0600) != nil {
		return errors.New("private-workspace")
	}
	cipher := exec.CommandContext(ctx, "sops", "encrypt", "--input-type", "json", "--output-type", "yaml", "--in-place", destination)
	cipher.Dir = dir
	cipher.Stdout = io.Discard
	cipher.Stderr = io.Discard
	if cipher.Run() != nil {
		return errors.New("encryption")
	}
	output, err := os.ReadFile(path)
	if err != nil || !encrypted(output, first) {
		return errors.New("encrypted-output")
	}
	after, err := collect(ctx, kubeContext)
	if err != nil || after != first {
		return errors.New("source-changed")
	}
	target, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("destination")
	}
	committed := false
	defer func() {
		_ = target.Close()
		if !committed {
			_ = os.Remove(outputPath)
		}
	}()
	if _, err = target.Write(output); err != nil {
		return errors.New("destination")
	}
	if target.Sync() != nil || target.Close() != nil {
		return errors.New("destination")
	}
	committed = true
	return nil
}
func main() {
	kubeContext := "oidc@prod"
	outputPath := destination
	verify := len(os.Args) == 4 && os.Args[1] == "verify" && os.Args[2] == "--context" && os.Args[3] == "admin@prod"
	apply := len(os.Args) == 4 && os.Args[1] == "apply-selected" && os.Args[2] == "--context" && os.Args[3] == "admin@prod"
	restore := len(os.Args) == 4 && os.Args[1] == "restore-disabled" && os.Args[2] == "--context" && os.Args[3] == "admin@prod"
	if verify || restore || apply {
		kubeContext = "admin@prod"
	} else {
		seen := map[string]bool{}
		for i := 1; i < len(os.Args); i += 2 {
			if i+1 >= len(os.Args) || seen[os.Args[i]] {
				fmt.Fprintln(os.Stderr, "GATEWAY_RECEIVER_BINDING=UNVERIFIED stage=arguments")
				os.Exit(2)
			}
			seen[os.Args[i]] = true
			switch os.Args[i] {
			case "--context":
				if os.Args[i+1] != "oidc@prod" && os.Args[i+1] != "admin@prod" {
					fmt.Fprintln(os.Stderr, "GATEWAY_RECEIVER_BINDING=UNVERIFIED stage=arguments")
					os.Exit(2)
				}
				kubeContext = os.Args[i+1]
			case "--output":
				outputPath = os.Args[i+1]
			default:
				fmt.Fprintln(os.Stderr, "GATEWAY_RECEIVER_BINDING=UNVERIFIED stage=arguments")
				os.Exit(2)
			}
		}
	}
	budget := 90 * time.Second
	if apply {
		budget = fenceApplyBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if apply {
		profile, err := renderTransportProfile(ctx)
		wrote := false
		if err == nil && profile.Fence {
			wrote, err = applyTransportProfile(ctx, kubeContext, profile)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "GATEWAY_RECEIVER_FENCE=UNVERIFIED stage=%s\n", err.Error())
			os.Exit(2)
		}
		fmt.Printf("GATEWAY_RECEIVER_FENCE=VERIFIED selected=%t wrote=%t\n", profile.Fence, wrote)
		return
	}
	if restore {
		profile, err := renderTransportProfile(ctx)
		selected := false
		if err == nil {
			selected, err = restoreBaseline(profile, func() error { return restoreDisabled(ctx, kubeContext) })
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "GATEWAY_RECEIVER_DISABLED=UNVERIFIED stage=%s\n", err.Error())
			os.Exit(2)
		}
		if selected {
			fmt.Println("GATEWAY_RECEIVER_DISABLED=NOT-APPLICABLE fence=selected")
			return
		}
		fmt.Println("GATEWAY_RECEIVER_DISABLED=VERIFIED bindings=0")
		return
	}
	if verify {
		profile, err := renderTransportProfile(ctx)
		if err == nil {
			err = verifyTransportProfile(ctx, kubeContext, profile)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "GATEWAY_RECEIVER_SOURCE=UNVERIFIED stage=%s\n", err.Error())
			os.Exit(2)
		}
		fmt.Printf("GATEWAY_RECEIVER_SOURCE=VERIFIED public_probes=4 fence_selected=%t\n", profile.Fence)
		return
	}
	if err := prepare(ctx, kubeContext, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "GATEWAY_RECEIVER_BINDING=UNVERIFIED stage=%s\n", err.Error())
		os.Exit(2)
	}
	fmt.Println("GATEWAY_RECEIVER_BINDING=PREPARED encrypted=true applied=false")
}
