package worldtrialprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Tools are absolute native executables. The caller installs the approved
// Cosign/ORAS releases; kubectl is additionally byte-pinned here.
type Tools struct{ Cosign, ORAS, Kubectl string }

// VerifyNative verifies both public packages anonymously, fetches immutable
// metadata/content only, safely renders all six objects, and retains full tool
// stdout/stderr/status. It never downloads executable image layers, runs an
// image/application, contacts a cluster, or uses host registry credentials.
// evidence must be a new, private directory; operational failures refuse proof.
func VerifyNative(ctx context.Context, root, evidence string, tools Tools) error {
	if err := Validate(root); err != nil {
		return err
	}
	r, err := reviewed()
	if err != nil {
		return err
	}
	for _, bin := range []string{tools.Cosign, tools.ORAS, tools.Kubectl} {
		if !filepath.IsAbs(bin) {
			return fmt.Errorf("absolute native tool path required")
		}
	}
	if err := os.Mkdir(evidence, 0700); err != nil {
		return fmt.Errorf("new private evidence directory required: %w", err)
	}
	anonymous := filepath.Join(evidence, "anonymous")
	if err := os.Mkdir(anonymous, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(anonymous, "config.json"), []byte("{\"auths\":{}}\n"), 0600); err != nil {
		return err
	}
	env := []string{"HOME=" + anonymous, "DOCKER_CONFIG=" + anonymous, "KUBECONFIG=/dev/null", "PATH=" + os.Getenv("PATH"), "GODEBUG=netdns=go", "TMPDIR=" + anonymous}
	run := func(name, bin string, args ...string) ([]byte, error) {
		child, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(child, bin, args...)
		cmd.Env = env
		cmd.Dir = evidence
		cmd.WaitDelay = 5 * time.Second
		out, err := os.Create(filepath.Join(evidence, name+".stdout"))
		if err != nil {
			return nil, err
		}
		defer func() { _ = out.Close() }()
		stderr, err := os.Create(filepath.Join(evidence, name+".stderr"))
		if err != nil {
			return nil, err
		}
		defer func() { _ = stderr.Close() }()
		cmd.Stdout = out
		cmd.Stderr = stderr
		runErr := cmd.Run()
		code := 0
		if runErr != nil {
			code = 2
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
		}
		// ProcessState may be nil when exec never started.
		if code < 0 {
			code = 2
		}
		if err := os.WriteFile(filepath.Join(evidence, name+".exit"), fmt.Appendf(nil, "%d\n", code), 0600); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(out.Name())
		if err != nil {
			return nil, err
		}
		if child.Err() != nil {
			return data, fmt.Errorf("%s: %w", name, child.Err())
		}
		if runErr != nil {
			return data, fmt.Errorf("%s: %w", name, runErr)
		}
		return data, nil
	}
	// Version checks supplement the caller's checksum-verified installation.
	version, err := run("cosign-version", tools.Cosign, "version", "--json")
	if err != nil {
		return err
	}
	var cv struct {
		GitVersion string `json:"gitVersion"`
	}
	if json.Unmarshal(version, &cv) != nil || cv.GitVersion != "v3.1.4" {
		return fmt.Errorf("cosign v3.1.4 required")
	}
	version, err = run("oras-version", tools.ORAS, "version")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(version), "Version:        1.3.6\n") {
		return fmt.Errorf("ORAS v1.3.6 required")
	}
	kb, err := os.ReadFile(tools.Kubectl)
	if err != nil {
		return err
	}
	wantKubectl := map[string]string{"darwin/arm64": "4408c85c83fd3a31adaa555bdf3c7a6c81f74b19449a9060ba31ab91926f023d", "linux/amd64": "1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82"}[runtime.GOOS+"/"+runtime.GOARCH]
	if wantKubectl == "" || hash(kb) != wantKubectl {
		return fmt.Errorf("approved kubectl v1.36.2 bytes required")
	}
	for _, p := range []struct{ name, ref string }{{"artifact", r.Artifact}, {"image", r.Image}} {
		data, err := run(p.name+"-cosign", tools.Cosign, "verify", "--certificate-identity", r.Publisher, "--certificate-oidc-issuer", r.Issuer, "--output=json", p.ref)
		if err != nil {
			return err
		}
		var claims []struct {
			Critical struct {
				Image struct {
					Digest string `json:"docker-manifest-digest"`
				} `json:"image"`
			} `json:"critical"`
		}
		if json.Unmarshal(data, &claims) != nil || len(claims) == 0 {
			return fmt.Errorf("%s: missing native verified claims", p.name)
		}
		for _, claim := range claims {
			if claim.Critical.Image.Digest != strings.Split(p.ref, "@")[1] {
				return fmt.Errorf("%s: native claim digest mismatch", p.name)
			}
		}
	}
	// This is a real adverse publisher verification, not a mocked failure.
	if _, err := run("wrong-publisher", tools.Cosign, "verify", "--certificate-identity", strings.Replace(r.Publisher, "v0.167.0", "v0.167.1", 1), "--certificate-oidc-issuer", r.Issuer, r.Artifact); err == nil {
		return fmt.Errorf("wrong publisher unexpectedly verified")
	}
	wrong, err := os.ReadFile(filepath.Join(evidence, "wrong-publisher.stderr"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(wrong), "v0.167.0") || !strings.Contains(string(wrong), "v0.167.1") {
		return fmt.Errorf("wrong publisher failed without concrete identity mismatch")
	}
	wrongStatus, err := os.ReadFile(filepath.Join(evidence, "wrong-publisher.exit"))
	if err != nil {
		return err
	}
	if string(wrongStatus) != "1\n" {
		return fmt.Errorf("wrong publisher did not fail with native identity-refusal status 1")
	}
	fetch := func(name, kind, ref string) ([]byte, error) {
		path := filepath.Join(evidence, name)
		if _, err := run("fetch-"+name, tools.ORAS, kind, "fetch", "--output", path, ref); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if hash(b) != r.FixtureFiles[name+".golden"] {
			return nil, fmt.Errorf("native %s differs from complete audited bytes", name)
		}
		return b, nil
	}
	manifest, err := fetch("manifest.json", "manifest", r.Artifact)
	if err != nil {
		return err
	}
	var m struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return err
	}
	base := strings.Split(r.Artifact, "@")[0]
	if _, err := fetch("config.json", "blob", base+"@"+m.Config.Digest); err != nil {
		return err
	}
	layer, err := fetch("layer.tar.gz", "blob", base+"@"+m.Layers[0].Digest)
	if err != nil {
		return err
	}
	files, err := inspectLayer(layer)
	if err != nil {
		return err
	}
	payload := filepath.Join(evidence, "payload")
	if err := os.Mkdir(payload, 0700); err != nil {
		return err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(payload, name), data, 0600); err != nil {
			return err
		}
	}
	rendered, err := run("rendered", tools.Kubectl, "kustomize", payload)
	if err != nil {
		return err
	}
	if hash(rendered) != r.FixtureFiles["rendered.yaml.golden"] {
		return fmt.Errorf("complete native seven-file/six-object render changed")
	}
	index, err := fetch("image-manifest.json", "manifest", r.Image)
	if err != nil {
		return err
	}
	var idx struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return err
	}
	imageBase := strings.Split(r.Image, "@")[0]
	for _, arch := range []string{"amd64", "arm64"} {
		var digest string
		for _, d := range idx.Manifests {
			if d.Platform.OS == "linux" && d.Platform.Architecture == arch {
				digest = d.Digest
			}
		}
		if digest == "" {
			return fmt.Errorf("missing image architecture %s", arch)
		}
		b, err := fetch("image-linux-"+arch+".json", "manifest", imageBase+"@"+digest)
		if err != nil {
			return err
		}
		var im struct {
			Config descriptor `json:"config"`
		}
		if err := json.Unmarshal(b, &im); err != nil {
			return err
		}
		if _, err := fetch("image-linux-"+arch+"-config.json", "blob", imageBase+"@"+im.Config.Digest); err != nil {
			return err
		}
	}
	if err := verifySource(ctx, evidence, r); err != nil {
		return err
	}
	if err := Validate(root); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(evidence, "verified-receipt.json"), receiptBytes, 0600)
}

type descriptor struct {
	Digest   string `json:"digest"`
	Platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

func verifySource(ctx context.Context, evidence string, r receipt) error {
	resolver := &net.Resolver{PreferGo: true}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, Resolver: resolver}).DialContext, TLSHandshakeTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 45 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return fmt.Errorf("source redirect refused") }}
	get := func(url, name string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("source HTTP status %d", res.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if len(b) >= 1<<20 {
			return nil, fmt.Errorf("source response oversized")
		}
		return b, os.WriteFile(filepath.Join(evidence, name), b, 0600)
	}
	for _, src := range []struct{ name, path string }{{"zone-main.go", "server/cmd/zone/main.go"}, {"server-cd.yaml", ".github/workflows/server-cd.yaml"}} {
		b, err := get("https://raw.githubusercontent.com/devantler-tech/world-at-ruin/"+r.Source+"/"+src.path, src.name)
		if err != nil {
			return err
		}
		if hash(b) != r.FixtureFiles[src.name+".golden"] {
			return fmt.Errorf("audited source %s changed", src.name)
		}
	}
	b, err := get("https://api.github.com/repos/devantler-tech/world-at-ruin/git/ref/tags/v0.167.0", "source-tag.json")
	if err != nil {
		return err
	}
	var tag struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if json.Unmarshal(b, &tag) != nil || tag.Ref != "refs/tags/v0.167.0" || tag.Object.SHA != r.Source || tag.Object.Type != "commit" {
		return fmt.Errorf("publisher tag/source revision changed")
	}
	return nil
}
