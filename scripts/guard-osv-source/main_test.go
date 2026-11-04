package main

import (
	"testing"
)

func TestOSVArgumentsKeepCompleteNativeScope(t *testing.T) {
	valid := []string{"scan", "source", "--experimental-exclude", ".agents", "--experimental-exclude", "megalinter-reports", "--recursive", "."}
	if !validArguments(valid) {
		t.Fatal("rejected actual MegaLinter source command")
	}
	for _, changed := range [][]string{{"scan", "source", "--recursive", "scripts"}, {"scan", "source", "--no-call-analysis=go", "--recursive", "."}, append(valid[:len(valid):len(valid)], "--config", "arbitrary.toml")} {
		if validArguments(changed) {
			t.Fatal("accepted reduced source or analysis scope")
		}
	}
}

func TestNativeOSVReportMustExamineEveryModuleAndKeepOperationalErrorsBlocking(t *testing.T) {
	files := map[string]string{"go.mod": "receipt", "nested/go.mod": "receipt"}
	valid := []byte(`{"results":[{"source":{"path":"/snapshot/go.mod"},"packages":[{"package":{"name":"stdlib","version":"1.27.3","ecosystem":"Go"}}]},{"source":{"path":"/snapshot/nested/go.mod"},"packages":[{"package":{"name":"stdlib","version":"1.26.10","ecosystem":"Go"}}]}]}`)
	log := []byte("Scanned /snapshot/go.mod file and found 124 packages\nScanned /snapshot/nested/go.mod file and found 78 packages\n")
	if err := validateReport(valid, log, "/snapshot", files); err != nil {
		t.Fatal(err)
	}
	if err := validateReport([]byte(`{"results":[]}`), log, "/snapshot", files); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte(`{"results":[]}`), []byte(`{"results":[{"source":{"path":"/snapshot/go.mod"},"packages":[]}]}`), []byte(`{"results":[{"source":{"path":"/snapshot/go.mod"},"packages":[{}]}]}`)} {
		if validateReport(data, nil, "/snapshot", files) == nil {
			t.Fatal("accepted incomplete or unexamined source")
		}
	}
	for _, failure := range []string{"Failed to run code analysis (govulncheck)", "panic: internal failure", "[ERROR] query failed"} {
		if validateReport(valid, append(append([]byte{}, log...), []byte(failure)...), "/snapshot", files) == nil {
			t.Fatal("accepted failed native analysis")
		}
	}
}
