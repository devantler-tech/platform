package main

import (
	"net"
	"os"
	"strings"
	"testing"
)

func TestNativeEndpointsStayReservedUntilHandoff(t *testing.T) {
	listeners, err := reserveEndpoints(2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	if len(listeners) != 2 || listeners[0].Addr().String() == listeners[1].Addr().String() {
		t.Fatal("native HTTPS and gRPC endpoints must be distinct")
	}
	for _, listener := range listeners {
		other, err := net.Listen("tcp", listener.Addr().String())
		if err == nil {
			_ = other.Close()
			t.Fatal("endpoint released before native process handoff")
		}
	}
}

func TestNativeTLSFloorControls(t *testing.T) {
	binary, web := os.Getenv("DEX_NATIVE_BINARY"), os.Getenv("DEX_NATIVE_WEB")
	if binary == "" || web == "" {
		t.Skip("native release fixture runs in the Dex image workflow")
	}
	if err := nativeTLSFloor(binary, web, "1.2"); err == nil || err.Error() != "dex discarded its configured TLS floor" {
		t.Fatalf("weak-floor control must reach the actual protocol check, got %v", err)
	}
	if err := nativeTLSFloor(binary, web, "1.3"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAdmissionRefusesAStaleCertificate(t *testing.T) {
	binary, web := os.Getenv("DEX_STALE_CERTIFICATE_BINARY"), os.Getenv("DEX_NATIVE_WEB")
	if binary == "" || web == "" {
		t.Skip("native reloader mutation runs in the Dex image workflow")
	}
	if err := nativeTLSFloor(binary, web, "1.3"); err == nil || err.Error() != "renewed certificate was not observed on both endpoints" {
		t.Fatalf("stale-certificate control must fail the actual renewal observation, got %v", err)
	}
}

func TestJudgeReportRefusesUnexaminedOrNewFindings(t *testing.T) {
	const sbom = `{"SBOM":{"go_version":"go1.27.3","modules":[{"path":"github.com/dexidp/dex","version":"v0.0.0-20261021142137-75eb19238e10"}]}}`
	const historical = `{"finding":{"osv":"GO-2024-2476","trace":[{"module":"github.com/dexidp/dex","function":"loadTLSConfig"}]}}`
	for _, tc := range []struct {
		name, report               string
		wantError, wantDisposition bool
	}{
		{"empty is not a scan", "", true, false},
		{"progress is not a scan", `{"progress":{"message":"scanning"}}`, true, false},
		{"empty module coverage", `{"SBOM":{"go_version":"go1.27.3","modules":[]}}`, true, false},
		{"different executable", `{"SBOM":{"go_version":"go1.27.3","modules":[{"path":"example.com/other"}]}}`, true, false},
		{"missing compiler coverage", strings.ReplaceAll(sbom, `"go1.27.3"`, `""`), true, false},
		{"malformed suffix", sbom + "{", true, false},
		{"new reachable finding", sbom + `{"finding":{"osv":"GO-2099-0001","trace":[{"module":"stdlib","function":"Serve"}]}}`, true, false},
		{"missing finding identity", sbom + `{"finding":{"trace":[{"module":"stdlib","function":"Serve"}]}}`, true, false},
		{"same id in another module", sbom + strings.ReplaceAll(historical, "github.com/dexidp/dex", "example.com/other"), true, false},
		{"historical requires native disposition", sbom + historical, false, true},
		{"local api replacement requires same disposition", sbom + strings.ReplaceAll(historical, "github.com/dexidp/dex", "./api/v2"), false, true},
		{"module only remains recorded", sbom + `{"finding":{"osv":"GO-2099-0002","trace":[{"module":"example.com/unused"}]}}`, false, false},
		{"examined clean executable", sbom, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disposition, err := judgeReport(strings.NewReader(tc.report), "go1.27.3")
			if (err != nil) != tc.wantError || disposition != tc.wantDisposition {
				t.Fatalf("disposition=%v err=%v; want disposition=%v error=%v", disposition, err, tc.wantDisposition, tc.wantError)
			}
		})
	}
}

func TestHistoricalDispositionRefusesChangedPrimaryAdvisory(t *testing.T) {
	const advisory = `{"id":"GO-2024-2476","affected":[{"package":{"name":"github.com/dexidp/dex","ecosystem":"Go"},"ranges":[{"type":"SEMVER","events":[{"introduced":"0"}]}],"ecosystem_specific":{"custom_ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"2.37.0"},{"fixed":"2.38.0"}]}]}}]}`
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"primary fixed range", advisory, false},
		{"missing", `{}`, true},
		{"wrong advisory", strings.ReplaceAll(advisory, "GO-2024-2476", "GO-2099-0001"), true},
		{"wrong package", strings.ReplaceAll(advisory, "github.com/dexidp/dex", "example.com/other"), true},
		{"revised affected range", strings.ReplaceAll(advisory, "2.38.0", "2.47.0"), true},
		{"changed generic mapping", strings.ReplaceAll(advisory, `"introduced":"0"`, `"introduced":"2.46.0"`), true},
		{"no fixed range", strings.ReplaceAll(advisory, `{"fixed":"2.38.0"}`, `{}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkHistoricalAdvisory([]byte(tc.body)); (err != nil) != tc.wantError {
				t.Fatalf("err=%v; want error=%v", err, tc.wantError)
			}
		})
	}
}
