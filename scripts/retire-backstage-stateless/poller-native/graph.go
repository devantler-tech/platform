package main

import (
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
)

// These complete locks are the reviewed graph of helm-controller 1.5.5,
// including its intentional Kustomize replacements. Move them only with the
// producer and renewed conformance/security evidence, never a dependency bump.
const nativeModSHA = "57247105c347d4018764df19f36646b0ec0c66302d930111c04b1d386dc03c36"
const nativeSumSHA = "6335830768e87322301ef84ad756f4de07f680148a3bd65d2b397a52681fe14c"

//go:embed go.mod
var nativeMod []byte

//go:embed go.sum
var nativeSum []byte

func validateNativeGraph(info *debug.BuildInfo, mod, sums []byte) error {
	if info == nil || fmt.Sprintf("%x", sha256.Sum256(mod)) != nativeModSHA || fmt.Sprintf("%x", sha256.Sum256(sums)) != nativeSumSHA {
		return errors.New("native dependency lock drift")
	}
	required := map[string]string{}
	replaced := map[string]debug.Module{}
	for _, line := range strings.Split(string(mod), "\n") {
		f := strings.Fields(strings.SplitN(line, "//", 2)[0])
		if len(f) == 2 && strings.HasPrefix(f[1], "v") {
			required[f[0]] = f[1]
		} else if len(f) == 4 && f[1] == "=>" {
			replaced[f[0]] = debug.Module{Path: f[2], Version: f[3]}
		}
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 {
			checksums[f[0]+" "+f[1]] = f[2]
		}
	}
	producer := map[string]bool{"github.com/fluxcd/helm-controller": true, "github.com/fluxcd/helm-controller/api": true, "github.com/fluxcd/pkg/ssa": true, "helm.sh/helm/v4": true}
	seen := map[string]bool{}
	for _, dep := range info.Deps {
		if dep == nil || seen[dep.Path] || required[dep.Path] == "" || dep.Version != required[dep.Path] {
			return errors.New("linked native dependency drift")
		}
		seen[dep.Path] = true
		delete(producer, dep.Path)
		actual := dep
		if replacement, ok := replaced[dep.Path]; ok {
			if dep.Replace == nil || dep.Replace.Path != replacement.Path || dep.Replace.Version != replacement.Version || dep.Replace.Replace != nil {
				return errors.New("native producer replacement drift")
			}
			actual = dep.Replace
		} else if dep.Replace != nil {
			return errors.New("unexpected native dependency replacement")
		}
		if checksum := checksums[actual.Path+" "+actual.Version]; checksum == "" || actual.Sum != checksum {
			return errors.New("linked native checksum drift")
		}
	}
	if len(producer) != 0 {
		return errors.New("missing native producer identity")
	}
	return nil
}
