package celapplicability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const celSum = "h1:DPGjXackMpJWH680oGY4lZhYjIameYmR+/6RBdDGmaI="
const celModSum = "h1:A9O8OU9rdvrK5MQyrqfIxo1a0u4g3sF8KB6PUIaryMM="

type nativeModule struct {
	Path, Version, Dir, Sum, GoModSum string
	Main                              bool
	Replace                           *nativeModule
}

func validateModules(modules []nativeModule, mainPath, cache string) error {
	seen := map[string]bool{}
	main, cel := 0, 0
	for _, module := range modules {
		if module.Path == "" || seen[module.Path] || module.Replace != nil {
			return errors.New("missing, duplicate or replaced module in CEL build list")
		}
		seen[module.Path] = true
		if module.Main {
			main++
			if module.Path != mainPath {
				return errors.New("unexpected main module")
			}
			continue
		}
		if module.Version == "" || module.Sum == "" || module.GoModSum == "" || !within(cache, module.Dir) {
			return fmt.Errorf("unverified module source for %s", module.Path)
		}
		if module.Path == modulePath {
			cel++
			if module.Version != moduleVersion || module.Sum != celSum || module.GoModSum != celModSum {
				return errors.New("CEL version or authenticated source sums changed")
			}
		}
	}
	if main != 1 || cel != 1 {
		return errors.New("expected one owner and one CEL module")
	}
	return nil
}

func within(root, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeEnv() []string {
	remove := map[string]bool{}
	for _, name := range []string{"GOENV", "GOWORK", "GOTOOLCHAIN", "GOFLAGS", "GOEXPERIMENT", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOSUMDB", "GONOPROXY", "GOROOT", "GODEBUG", "GOVCS", "GOAUTH"} {
		remove[name] = true
	}
	var env []string
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !remove[name] && !strings.HasPrefix(name, "GIT_") {
			env = append(env, item)
		}
	}
	return append(env, "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=go1.27.3", "GOFLAGS=", "GOEXPERIMENT=",
		"GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOPRIVATE=", "GONOSUMDB=", "GONOPROXY=", "GOVCS=*:off",
		"GODEBUG=netdns=go", "GOAUTH=off", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

func run(ctx context.Context, directory, program string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir, command.Env = directory, safeEnv()
	command.WaitDelay = 2 * time.Second
	output := boundedOutput{limit: 32 << 20}
	command.Stdout = &output
	err := command.Run()
	if err != nil {
		return nil, fmt.Errorf("%s metadata/verification command failed: %w", program, err)
	}
	return output.buffer.Bytes(), nil
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > output.limit-output.buffer.Len() {
		return 0, errors.New("native metadata output exceeds its bound")
	}
	return output.buffer.Write(data)
}

func moduleList(ctx context.Context, directory string) ([]nativeModule, error) {
	data, err := run(ctx, directory, "go", "list", "-mod=readonly", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var modules []nativeModule
	for {
		var module nativeModule
		if err := decoder.Decode(&module); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		modules = append(modules, module)
		if len(modules) > 512 {
			return nil, errors.New("CEL module inventory exceeds its bound")
		}
	}
	return modules, nil
}

func loadModules(ctx context.Context, owner owner, cache string, files map[string]trackedFile) ([]nativeModule, func(), error) {
	directory, err := os.MkdirTemp("", "cel-applicability-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	for _, name := range []string{"go.mod", "go.sum"} {
		file, ok := files[owner.Directory+"/"+name]
		if !ok {
			cleanup()
			return nil, nil, errors.New("CEL owner is missing a tracked module file")
		}
		if err := os.WriteFile(filepath.Join(directory, name), file.Data, 0600); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	if _, err := run(ctx, directory, "go", "mod", "download", "-json", "all"); err != nil {
		cleanup()
		return nil, nil, err
	}
	mod, err := os.ReadFile(filepath.Join(directory, "go.mod"))
	if err != nil || !bytes.Equal(mod, files[owner.Directory+"/go.mod"].Data) {
		cleanup()
		return nil, nil, errors.New("module download changed the selected dependency requirements")
	}
	modules, err := moduleList(ctx, directory)
	if err == nil {
		err = validateModules(modules, owner.Module, cache)
	}
	if err == nil {
		_, err = run(ctx, directory, "go", "mod", "verify")
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return modules, cleanup, nil
}
