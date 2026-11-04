package celapplicability

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeOutputBoundSurvivesStreamingCopy(t *testing.T) {
	output := boundedOutput{limit: 3}
	if _, err := io.Copy(&output, strings.NewReader("metadata beyond its bound")); err == nil {
		t.Fatal("native output exceeded its bound")
	}
	if output.buffer.Len() > 3 {
		t.Fatal("native output was retained beyond its bound")
	}
}

func TestModuleEvidenceRefusesVersionReplacementAndIdentityDrift(t *testing.T) {
	cache := t.TempDir()
	good := []nativeModule{
		{Path: "example.test/owner", Main: true},
		{Path: modulePath, Version: moduleVersion, Sum: celSum, GoModSum: celModSum, Dir: filepath.Join(cache, "cel")},
	}
	for _, mutation := range []string{"version", "sum", "mod sum", "replacement", "missing CEL", "duplicate CEL", "wrong main", "outside cache"} {
		t.Run(mutation, func(t *testing.T) {
			modules := append([]nativeModule(nil), good...)
			switch mutation {
			case "version":
				modules[1].Version = "v0.29.0"
			case "sum":
				modules[1].Sum = "h1:unverified"
			case "mod sum":
				modules[1].GoModSum = "h1:unverified"
			case "replacement":
				modules[1].Replace = &nativeModule{Dir: "/elsewhere"}
			case "missing CEL":
				modules = modules[:1]
			case "duplicate CEL":
				modules = append(modules, modules[1])
			case "wrong main":
				modules[0].Path = "example.test/other"
			case "outside cache":
				modules[1].Dir = filepath.Dir(cache)
			}
			if err := validateModules(modules, "example.test/owner", cache); err == nil {
				t.Fatal("accepted module evidence with changed source identity")
			}
		})
	}
	if err := validateModules(good, "example.test/owner", cache); err != nil {
		t.Fatal(err)
	}
}
