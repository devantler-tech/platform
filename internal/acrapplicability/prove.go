// Package acrapplicability proves one source-only ACR advisory disposition with
// the native Go analyzer and a real, never-executed paired reachability control.
package acrapplicability

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

const scannerSum = "h1:clG4qBU6zH5VKjti8n5j8BBuYzoSha392xXMkXS351U="
const pairedSource = "// Static reachability control only. This test is never executed.\npackage main\nimport (\n \"testing\"\n credhelper \"github.com/chrismellard/docker-credential-acr-env/pkg/credhelper\"\n)\nfunc TestACRReachabilityControl(t *testing.T) {\n helper := credhelper.ACRCredHelper{}\n _, _, err := helper.Get(\"untrusted.invalid\")\n if err != nil { t.Fatal(err) }\n}\n"

// Proof retains complete native reports, including every unrelated finding.
// Status is set only after all source, advisory and paired-control gates pass.
type Proof struct {
	AdvisoryID          string
	Module              string
	Version             string
	Owner               string
	PURL                string
	Status              string
	Justification       string
	GOOS                string
	GOARCH              string
	CGOEnabled          string
	GoVersion           string
	ScannerVersion      string
	ScannerModuleSum    string
	ScannerSHA256       string
	ModuleSourceSum     string
	ScannerSourceSums   map[string]string
	Head                string
	InputReceiptSHA256  string
	AdvisorySHA256      string
	DatabaseIndexSHA256 string
	DatabaseModified    string
	SourceFiles         map[string]string
	Symbols             []string
	Aliases             []string
	Roots               []string
	CompiledGoFiles     []string
	OtherFindingIDs     []string
	OriginalJSON        []byte
	OriginalStderr      []byte
	PairedJSON          []byte
	PairedStderr        []byte
	GoListJSON          []byte
	GoListStderr        []byte
	PairedGoListJSON    []byte
	PairedGoListStderr  []byte
	ToolInstallStderr   []byte
	CompilerStderr      []byte
	AdvisoryJSON        []byte
	DatabaseIndexJSON   []byte
	PairedControlSource []byte
	OriginalExit        int
	PairedExit          int
}

func cleanEnvironment(analysis bool, extra ...string) []string {
	var result []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GO") || strings.HasPrefix(name, "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	result = append(result, "GOENV=off", "GODEBUG=netdns=go", "GOTOOLCHAIN=go1.27.3", "GOWORK=off", "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOPRIVATE=", "GONOSUMDB=", "GONOPROXY=", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if analysis {
		result = append(result, "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOVERSION=go1.27.3")
	}
	return append(result, extra...)
}

type boundedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<20 {
		b.exceeded = true
		return 0, fmt.Errorf("native evidence exceeded its bound")
	}
	return b.Buffer.Write(p)
}

func run(ctx context.Context, dir string, env []string, budget time.Duration, program string, args ...string) ([]byte, []byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = env
	var out, errout boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	status := 0
	if exit, ok := err.(*exec.ExitError); ok {
		status = exit.ExitCode()
	}
	if ctx.Err() != nil {
		return out.Bytes(), errout.Bytes(), status, fmt.Errorf("native ACR analysis command exceeded its budget or was cancelled")
	}
	if err != nil || out.exceeded || errout.exceeded {
		return out.Bytes(), errout.Bytes(), status, fmt.Errorf("native ACR analysis command did not complete (exit %d)", status)
	}
	return out.Bytes(), errout.Bytes(), status, nil
}

func git(ctx context.Context, root string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-C", root}, args...)
	out, _, _, err := run(ctx, root, cleanEnvironment(false), 30*time.Second, "git", args...)
	return out, err
}

func fetch(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	dialer := &net.Dialer{Timeout: 30 * time.Second, Resolver: &net.Resolver{PreferGo: true}}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("canonical advisory redirect refused") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://vuln.go.dev/"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("canonical Go advisory fetch failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("canonical Go advisory fetch returned HTTP %d", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(b) == 0 || len(b) > 1<<20 {
		return nil, fmt.Errorf("canonical advisory response is incomplete or oversized")
	}
	return b, nil
}

// Prove reads an already-created scansnapshot.Root. It never clones or writes to
// that root; compiler artifacts and the sole added control test live privately.
// The claim covers only Linux/amd64 with CGO disabled and the exact owner/version.
// The caller must bind Head to its snapshot and verify the complete snapshot
// receipt around all scanners; SourceFiles is this proof's narrower input receipt.
func Prove(ctx context.Context, root string) (proof Proof, err error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	proof = Proof{AdvisoryID: Advisory, Module: Module, Version: Version, Owner: Owner, PURL: "pkg:golang/" + Module + "@" + Version, GOOS: "linux", GOARCH: "amd64", CGOEnabled: "0", GoVersion: "go1.27.3", ScannerVersion: "v1.8.0", ScannerModuleSum: scannerSum, Symbols: []string{"ACRCredHelper.Get", "isACRRegistry"}}
	root, err = filepath.Abs(root)
	if err != nil {
		return proof, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return proof, err
	}
	if strings.Contains(os.Getenv("GOFLAGS"), "overlay") || strings.Contains(os.Getenv("GOFLAGS"), "modfile") || (os.Getenv("GOWORK") != "" && os.Getenv("GOWORK") != "off") || os.Getenv("GOCACHEPROG") != "" || os.Getenv("GOEXPERIMENT") != "" {
		return proof, fmt.Errorf("workspace, overlay or noncanonical compiler configuration refused")
	}
	if _, err = git(ctx, root, "diff", "--quiet", "HEAD", "--"); err != nil {
		return proof, fmt.Errorf("ACR proof requires an unchanged tracked snapshot")
	}
	head, err := git(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return proof, err
	}
	proof.Head = strings.TrimSpace(string(head))
	listing, err := git(ctx, root, "ls-files", "-z")
	if err != nil {
		return proof, err
	}
	files := strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00")
	inputs, err := inspectInputs(root, files)
	if err != nil {
		return proof, err
	}
	proof.SourceFiles = inputs.Hashes
	proof.CompiledGoFiles = slices.Clone(inputs.GoFiles)
	receipt, err := json.Marshal(inputs.Hashes)
	if err != nil {
		return proof, err
	}
	proof.InputReceiptSHA256 = digest(receipt)
	defer func() {
		if e := inputs.verify(root); e != nil {
			proof.Status = ""
			proof.Justification = ""
			err = e
		}
	}()
	proof.DatabaseIndexJSON, err = fetch(ctx, "index/db.json")
	if err != nil {
		return proof, err
	}
	var index struct{ Modified string }
	if err = json.Unmarshal(proof.DatabaseIndexJSON, &index); err != nil || index.Modified == "" {
		return proof, fmt.Errorf("canonical database index is invalid")
	}
	proof.DatabaseModified = index.Modified
	proof.DatabaseIndexSHA256 = digest(proof.DatabaseIndexJSON)
	proof.AdvisoryJSON, err = fetch(ctx, "ID/"+Advisory+".json")
	if err != nil {
		return proof, err
	}
	advisory, err := advisoryScope(proof.AdvisoryJSON)
	if err != nil {
		return proof, err
	}
	proof.AdvisorySHA256 = digest(proof.AdvisoryJSON)
	for _, a := range array(advisory["aliases"]) {
		value, ok := a.(string)
		if !ok {
			return proof, fmt.Errorf("invalid canonical advisory alias")
		}
		proof.Aliases = append(proof.Aliases, value)
	}
	work, err := os.MkdirTemp("", "platform-acr-applicability-")
	if err != nil {
		return proof, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	goBin, err := exec.LookPath("go")
	if err != nil {
		return proof, err
	}
	goVersion, compilerStderr, _, err := run(ctx, root, cleanEnvironment(false), time.Minute, goBin, "env", "GOVERSION")
	proof.CompilerStderr = compilerStderr
	if err != nil || strings.TrimSpace(string(goVersion)) != "go1.27.3" {
		return proof, fmt.Errorf("the root Go1.27.3 compiler is required")
	}
	bin := filepath.Join(work, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		return proof, err
	}
	if _, proof.ToolInstallStderr, _, err = run(ctx, work, cleanEnvironment(false, "GOBIN="+bin, "CGO_ENABLED=0"), 2*time.Minute, goBin, "install", "golang.org/x/vuln/cmd/govulncheck@v1.8.0"); err != nil {
		return proof, fmt.Errorf("install native govulncheck: %w", err)
	}
	scanner := filepath.Join(bin, "govulncheck")
	build, err := buildinfo.ReadFile(scanner)
	if err != nil || build.GoVersion != "go1.27.3" || build.Main.Path != "golang.org/x/vuln" || build.Main.Version != "v1.8.0" || build.Main.Sum != scannerSum || build.Main.Replace != nil {
		return proof, fmt.Errorf("native govulncheck compiler/module identity mismatch")
	}
	scannerBytes, err := os.ReadFile(scanner)
	if err != nil {
		return proof, err
	}
	proof.ScannerSHA256 = digest(scannerBytes)
	proof.ScannerSourceSums = map[string]string{}
	for _, mod := range append([]*debug.Module{&build.Main}, build.Deps...) {
		if mod.Replace != nil || mod.Sum == "" {
			return proof, fmt.Errorf("native scanner has an unverified module source")
		}
		sum, e := verifyModule(ctx, work, goBin, mod.Path, mod.Version, mod.Sum)
		if e != nil {
			return proof, e
		}
		proof.ScannerSourceSums[mod.Path+"@"+mod.Version] = sum
	}
	proof.ModuleSourceSum, err = verifyModule(ctx, work, goBin, Module, Version, moduleSum)
	if err != nil {
		return proof, err
	}
	moduleDir := filepath.Join(root, filepath.Dir(Owner))
	env := cleanEnvironment(true)
	proof.GoListJSON, proof.GoListStderr, _, err = run(ctx, moduleDir, env, 2*time.Minute, goBin, "list", "-test", "-json", "./...")
	if err != nil {
		return proof, err
	}
	proof.Roots, err = compiledInputs(root, inputs.GoFiles, proof.GoListJSON)
	if err != nil {
		return proof, err
	}
	args := []string{"-test", "-json", "-mode=source", "-scan=symbol", "-db=https://vuln.go.dev", "./..."}
	proof.OriginalJSON, proof.OriginalStderr, proof.OriginalExit, err = run(ctx, moduleDir, env, 4*time.Minute, scanner, args...)
	if err != nil {
		return proof, err
	}
	original, err := nativeReport(proof.OriginalJSON, proof.AdvisoryJSON, index.Modified, false, proof.Roots)
	if err != nil {
		return proof, err
	}
	proof.OtherFindingIDs = original.OtherIDs
	controlRoot := filepath.Join(work, "control")
	controlDir := filepath.Join(controlRoot, filepath.Dir(Owner))
	if err = os.CopyFS(controlDir, os.DirFS(moduleDir)); err != nil {
		return proof, err
	}
	controlFile := filepath.Join(controlDir, "acr_reachability_control_test.go")
	f, err := os.OpenFile(controlFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return proof, fmt.Errorf("paired ACR control input already exists")
	}
	_, writeErr := f.WriteString(pairedSource)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return proof, fmt.Errorf("paired control could not be written")
	}
	proof.PairedControlSource = []byte(pairedSource)
	pairedFiles := append(slices.Clone(inputs.GoFiles), filepath.Join(filepath.Dir(Owner), "acr_reachability_control_test.go"))
	proof.PairedGoListJSON, proof.PairedGoListStderr, _, err = run(ctx, controlDir, env, 2*time.Minute, goBin, "list", "-test", "-json", "./...")
	if err != nil {
		return proof, err
	}
	pairedRoots, err := compiledInputs(controlRoot, pairedFiles, proof.PairedGoListJSON)
	if err != nil {
		return proof, err
	}
	proof.PairedJSON, proof.PairedStderr, proof.PairedExit, err = run(ctx, controlDir, env, 4*time.Minute, scanner, args...)
	if err != nil {
		return proof, err
	}
	if _, err = nativeReport(proof.PairedJSON, proof.AdvisoryJSON, index.Modified, true, pairedRoots); err != nil {
		return proof, err
	}
	for path, want := range inputs.Hashes {
		if !strings.HasPrefix(path, filepath.Dir(Owner)+"/") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(controlRoot, path))
		if e != nil || digest(b) != want {
			return proof, fmt.Errorf("paired control changed an original module input")
		}
	}
	after, err := fetch(ctx, "index/db.json")
	if err != nil || !bytes.Equal(after, proof.DatabaseIndexJSON) {
		return proof, fmt.Errorf("canonical database changed during the ACR proof")
	}
	if _, err = git(ctx, root, "diff", "--quiet", "HEAD", "--"); err != nil {
		return proof, fmt.Errorf("tracked source changed during ACR proof")
	}
	finalHead, e := git(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if e != nil || strings.TrimSpace(string(finalHead)) != proof.Head {
		return proof, fmt.Errorf("snapshot HEAD changed during ACR proof")
	}
	if _, err = verifyModule(ctx, work, goBin, Module, Version, moduleSum); err != nil {
		return proof, err
	}
	proof.Status = "not_affected"
	proof.Justification = "vulnerable_code_not_in_execute_path"
	return proof, nil
}
