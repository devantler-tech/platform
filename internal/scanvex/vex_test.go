package scanvex

import "testing"

func TestNativeAdjudicationCannotEscapeItsAdvisoryVersionAndCompleteLocations(t *testing.T) {
	scopes := []Scope{{IDs: []string{"GHSA-example", "GO-example"}, Module: "example.test/library", Version: "v1.0.0", Paths: []string{"fixture/go.mod"}, Justification: "vulnerable_code_not_in_execute_path"}}
	for _, candidate := range []struct {
		id, module, version string
		paths               []string
		want                bool
	}{
		{"GO-example", "example.test/library", "v1.0.0", []string{"fixture/go.mod"}, true},
		{"GO-other", "example.test/library", "v1.0.0", []string{"fixture/go.mod"}, false},
		{"GO-example", "example.test/library", "v1.1.0", []string{"fixture/go.mod"}, false},
		{"GO-example", "another/library", "v1.0.0", []string{"fixture/go.mod"}, false},
		{"GO-example", "example.test/library", "v1.0.0", nil, false},
		{"GO-example", "example.test/library", "v1.0.0", []string{"fixture/go.mod", "new/go.mod"}, false},
	} {
		if allowed(scopes, candidate.id, candidate.module, candidate.version, candidate.paths) != candidate.want {
			t.Fatalf("wrong native boundary for %+v", candidate)
		}
	}
}

func TestUnexpectedSuppressionRemainsBlocking(t *testing.T) {
	scopes := []Scope{{IDs: []string{"GO-example"}, Module: "example.test/library", Version: "v1.0.0", Paths: []string{"fixture/go.mod"}, Justification: "vulnerable_code_not_in_execute_path"}}
	data := []byte(`{"Results":[{"Target":"fixture/go.mod","ExperimentalModifiedFindings":[{"Type":"vulnerability","Status":"not_affected","Statement":"vulnerable_code_not_in_execute_path","Source":"/proof/vex.json","Finding":{"VulnerabilityID":"GO-example","PkgName":"example.test/library","InstalledVersion":"v1.0.0"}}]}]}`)
	if err := ValidateTrivy(data, "/proof/vex.json", scopes); err != nil {
		t.Fatal(err)
	}
	if ValidateTrivy(data, "/other/vex.json", scopes) == nil {
		t.Fatal("accepted an unbound VEX source")
	}
	scopes[0].Paths = []string{"different/go.mod"}
	if ValidateTrivy(data, "/proof/vex.json", scopes) == nil {
		t.Fatal("accepted an extra source occurrence")
	}
	if ValidateTrivy([]byte(`{}`), "/proof/vex.json", scopes) == nil {
		t.Fatal("accepted an empty native scan")
	}
}

func TestGrypeRetainsUnknownFindingsAndRejectsUnprovedSuppression(t *testing.T) {
	active := []byte(`{"descriptor":{"name":"grype","version":"0.118.0"},"source":{"type":"directory"},"matches":[{"vulnerability":{"id":"GO-new","severity":"Unknown"}}],"ignoredMatches":[]}`)
	count, err := ValidateGrype(active, nil)
	if err != nil || count != 1 {
		t.Fatalf("lost unknown finding: %d %v", count, err)
	}
	ignored := []byte(`{"descriptor":{"name":"grype","version":"0.118.0"},"source":{"type":"directory"},"matches":[],"ignoredMatches":[{"vulnerability":{"id":"GO-example"},"artifact":{"name":"example.test/library","version":"v1.0.0","locations":[{"path":"/fixture/go.mod","accessPath":"/other/go.mod"}]},"appliedIgnoreRules":[{"namespace":"vex","vex-status":"not_affected"}]}]}`)
	scopes := []Scope{{IDs: []string{"GO-example"}, Module: "example.test/library", Version: "v1.0.0", Paths: []string{"fixture/go.mod"}, Justification: "vulnerable_code_not_in_execute_path"}}
	if _, err := ValidateGrype(ignored, scopes); err == nil {
		t.Fatal("accepted unexpected complete artifact location")
	}
}
