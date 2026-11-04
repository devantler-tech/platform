package main

import "testing"

func TestNativeCheckovScopeCannotDropAFrameworkOrSourceTree(t *testing.T) {
	good := []string{"--skip-path", "tests/", "--config-file", "/native/.checkov.yml", "--skip-path", ".agents", "--skip-path", "megalinter-reports", "--directory", ".", "--skip-framework", "secrets"}
	if _, err := arguments(good); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 5, 7, 9, 11} {
		changed := append([]string{}, good...)
		changed[index] = "other"
		if _, err := arguments(changed); err == nil {
			t.Fatal("accepted a changed native scope")
		}
	}
}

func TestNativeCheckovReportRequiresAllExaminedFrameworks(t *testing.T) {
	good := []byte(`[{"check_type":"cloudformation","summary":{"passed":0,"failed":0,"skipped":0,"parsing_errors":0}},{"check_type":"kubernetes","summary":{"passed":4000,"failed":0,"skipped":50,"parsing_errors":0}},{"check_type":"dockerfile","summary":{"passed":600,"failed":0,"skipped":1,"parsing_errors":0}},{"check_type":"github_actions","summary":{"passed":900,"failed":0,"skipped":4,"parsing_errors":0}}]`)
	if err := validateReport(good, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, []byte(`[]`), []byte(`[{"check_type":"kubernetes","summary":{"passed":4000}}]`)} {
		if validateReport(bad, nil) == nil {
			t.Fatal("accepted a partial native scan")
		}
	}
	if validateReport(good, []byte("YAML error parsing ./unknown.yaml: invalid")) == nil {
		t.Fatal("accepted an unknown parser omission")
	}
}
