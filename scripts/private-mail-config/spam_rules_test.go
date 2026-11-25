package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The pinned stalwartlabs/spam-filter v3.0.2 release asset and the licence
// file of its tag commit, byte for byte as published.
const (
	spamRulesFixture        = "testdata/spam-filter-v3.0.2/spam-filter-rules.json.gz"
	spamRulesLicenseFixture = "testdata/spam-filter-v3.0.2/LICENSE-MIT"
	spamRulesCommit         = "3d2eea7f045ccf9cbb67eea131f627880c7dc983"
	spamRulesReleaseURL     = "https://github.com/stalwartlabs/spam-filter/releases/download/v3.0.2/spam-filter-rules.json.gz"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gunzipFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(reader); err != nil || reader.Close() != nil {
		t.Fatal("pinned release is not a gzip stream")
	}
	return out.Bytes()
}

func rulesByKind(t *testing.T, data []byte) map[string][]any {
	t.Helper()
	var objects map[string][]any
	if err := json.Unmarshal(data, &objects); err != nil {
		t.Fatal(err)
	}
	return objects
}

// TestSpamRulesLeaveOutOnlyTheShortenedLinkLookup derives the image's copy from
// the pinned release. The shortened-link check requests links found in received
// mail and runs only for a host in the url-redirectors lookup, so those keys go.
// Every other object, the 18 DNS blocklists and four HTTP lists included, stays.
func TestSpamRulesLeaveOutOnlyTheShortenedLinkLookup(t *testing.T) {
	upstream := readFixture(t, spamRulesFixture)
	derived, err := deriveSpamRules(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := deriveSpamRules(upstream); err != nil || !bytes.Equal(again, derived) {
		t.Fatal("derivation is not deterministic")
	}
	want, got := rulesByKind(t, gunzipFixture(t, upstream)), rulesByKind(t, derived)
	if len(got) != len(want) {
		t.Fatalf("derived rules hold %d object types, want %d", len(got), len(want))
	}
	for kind, objects := range want {
		if kind == "MemoryLookupKey" {
			kept := []any{}
			for _, object := range objects {
				if object.(map[string]any)["namespace"] != "url-redirectors" {
					kept = append(kept, object)
				}
			}
			if len(kept) != 1008 || !reflect.DeepEqual(got[kind], kept) {
				t.Fatal("lookup keys are not the release's keys without the shortened-link list")
			}
			continue
		}
		if !reflect.DeepEqual(got[kind], objects) {
			t.Fatalf("%s changed", kind)
		}
	}
	if len(got["SpamDnsblServer"]) != 18 || len(got["HttpLookup"]) != 4 || bytes.Contains(derived, []byte(`"url-redirectors"`)) {
		t.Fatal("derived rules keep the shortened-link list or drop a DNS blocklist or HTTP list")
	}
}

// TestSpamRulesRefuseAnotherRelease accepts only the pinned bytes. The same
// rules compressed again pass every count, so only the digest refuses them.
func TestSpamRulesRefuseAnotherRelease(t *testing.T) {
	upstream := readFixture(t, spamRulesFixture)
	var recompressed bytes.Buffer
	writer := gzip.NewWriter(&recompressed)
	if _, err := writer.Write(gunzipFixture(t, upstream)); err != nil || writer.Close() != nil {
		t.Fatal("could not recompress the fixture")
	}
	if bytes.Equal(recompressed.Bytes(), upstream) {
		t.Fatal("recompression reproduced the pinned bytes; the control proves nothing")
	}
	if _, err := filterSpamRules(gunzipFixture(t, recompressed.Bytes())); err != nil {
		t.Fatal("recompressed content no longer passes the count checks; the control proves nothing")
	}
	for name, data := range map[string][]byte{
		"recompressed": recompressed.Bytes(),
		"truncated":    upstream[:len(upstream)-1],
		"empty":        {},
	} {
		if _, err := deriveSpamRules(data); err == nil {
			t.Fatalf("%s release accepted", name)
		}
	}
}

// TestSpamRulesRefuseCountDrift refuses content whose object or lookup counts
// differ from the reviewed release, whatever produced it.
func TestSpamRulesRefuseCountDrift(t *testing.T) {
	content := gunzipFixture(t, readFixture(t, spamRulesFixture))
	mutate := func(change func(map[string][]json.RawMessage)) []byte {
		var objects map[string][]json.RawMessage
		if err := json.Unmarshal(content, &objects); err != nil {
			t.Fatal(err)
		}
		change(objects)
		data, err := json.Marshal(objects)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	key := func(namespace string) json.RawMessage {
		return json.RawMessage(`{"key":"drift.example","namespace":"` + namespace + `","isGlobPattern":false}`)
	}
	if _, err := filterSpamRules(mutate(func(map[string][]json.RawMessage) {})); err != nil {
		t.Fatal("reviewed content refused")
	}
	for name, change := range map[string]func(map[string][]json.RawMessage){
		"DNS blocklist removed": func(o map[string][]json.RawMessage) { o["SpamDnsblServer"] = o["SpamDnsblServer"][1:] },
		"rule added":            func(o map[string][]json.RawMessage) { o["SpamRule"] = append(o["SpamRule"], o["SpamRule"][0]) },
		"object type added":     func(o map[string][]json.RawMessage) { o["SpamPyzor"] = []json.RawMessage{} },
		"object type removed":   func(o map[string][]json.RawMessage) { delete(o, "HttpLookup") },
		"lookup namespace added": func(o map[string][]json.RawMessage) {
			o["MemoryLookupKey"] = append(o["MemoryLookupKey"][1:], key("blocked-domains"))
		},
		"shortened-link key moved": func(o map[string][]json.RawMessage) {
			for i, raw := range o["MemoryLookupKey"] {
				if bytes.Contains(raw, []byte(`"trusted-domains"`)) {
					o["MemoryLookupKey"][i] = key("url-redirectors")
					return
				}
			}
		},
	} {
		if _, err := filterSpamRules(mutate(change)); err == nil {
			t.Fatalf("%s: drifted content accepted", name)
		}
	}
	for name, data := range map[string][]byte{"not JSON": []byte("{"), "trailing value": append(append([]byte{}, content...), []byte(" {}")...)} {
		if _, err := filterSpamRules(data); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// TestSpamRulesCommandWritesOneReadOnlyCopy runs the image build step.
func TestSpamRulesCommandWritesOneReadOnlyCopy(t *testing.T) {
	input, err := filepath.Abs(spamRulesFixture)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "spam-filter-rules.json")
	var log bytes.Buffer
	if command([]string{"spam-rules", "--input", input, "--output", output}, &log) != 0 {
		t.Fatalf("pinned release refused: %s", &log)
	}
	info, err := os.Stat(output)
	want, deriveErr := deriveSpamRules(readFixture(t, spamRulesFixture))
	if err != nil || deriveErr != nil || info.Mode().Perm() != 0444 || !bytes.Equal(readFixture(t, output), want) {
		t.Fatal("build step did not write the derived rules read-only")
	}
	if command([]string{"spam-rules", "--input", input, "--output", output}, &log) == 0 {
		t.Fatal("existing rules file replaced")
	}
	other := filepath.Join(t.TempDir(), "other.json.gz")
	if err := os.WriteFile(other, readFixture(t, spamRulesFixture)[1:], 0600); err != nil {
		t.Fatal(err)
	}
	refused := filepath.Join(t.TempDir(), "refused.json")
	for _, args := range [][]string{
		{"--input", other, "--output", refused},
		{"--input", spamRulesFixture, "--output", refused},
		{"--input", input},
	} {
		if command(append([]string{"spam-rules"}, args...), &log) == 0 {
			t.Fatalf("spam-rules accepted %v", args)
		}
		if _, err := os.Lstat(refused); !os.IsNotExist(err) {
			t.Fatal("refused input left a rules file")
		}
	}
}

// TestSpamRulesLoadFromTheImageCopy binds the plan, the image recipe and the
// pins together: the recipe fetches the pinned release and licence by digest,
// derives exactly the file the plan names, and only the audited schema uses it.
func TestSpamRulesLoadFromTheImageCopy(t *testing.T) {
	release := sha256.Sum256(readFixture(t, spamRulesFixture))
	license := sha256.Sum256(readFixture(t, spamRulesLicenseFixture))
	if hex.EncodeToString(release[:]) != spamRulesSHA256 {
		t.Fatal("pinned digest does not match the release fixture")
	}
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	instructions := dockerfileInstructions(string(data))
	want := []string{
		"ADD --checksum=sha256:" + spamRulesSHA256 + " " + spamRulesReleaseURL + " /spam-filter-rules.json.gz",
		"ADD --chmod=0444 --checksum=sha256:" + hex.EncodeToString(license[:]) + " https://raw.githubusercontent.com/stalwartlabs/spam-filter/" + spamRulesCommit + "/LICENSE-MIT /usr/local/share/licenses/stalwart-spam-filter/LICENSE-MIT",
		"RUN --mount=type=bind,from=spam-filter-release,source=/spam-filter-rules.json.gz,target=/tmp/spam-filter-rules.json.gz /usr/local/bin/private-mail-bootstrap spam-rules --input /tmp/spam-filter-rules.json.gz --output " + spamRulesPath,
	}
	for _, line := range want {
		found := 0
		for _, instruction := range instructions {
			if strings.Join(strings.Fields(instruction), " ") == line {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("image recipe lacks exactly one %q", line)
		}
	}
	if ops := planObjects(t); !reflect.DeepEqual(ops["SpamSettings"].Value, map[string]any{"spamFilterRulesUrl": "file://" + spamRulesPath}) {
		t.Fatal("the audited schema does not load the image copy")
	}
	v := fixture()
	v.RuntimeVersion = "1.0.3"
	plan, err := render(v)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plan, []byte(`"SpamSettings"`)) {
		t.Fatal("the 1.x candidate loads 0.16-format rules")
	}
}

// TestSpamRulesReadbackRefusesTheUpstreamDownload reports a store that still
// names the upstream release URL, which egress blocks.
func TestSpamRulesReadbackRefusesTheUpstreamDownload(t *testing.T) {
	op := planObjects(t)["SpamSettings"]
	for _, tc := range []struct {
		name  string
		url   any
		valid bool
	}{
		{"image copy", "file://" + spamRulesPath, true},
		{"upstream latest release", "https://github.com/stalwartlabs/spam-filter/releases/latest/download/spam-filter-rules.json.gz", false},
		{"no rules source", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := map[string]any{"id": "singleton", "spamFilterRulesUrl": tc.url}
			if (singletonReadback(t, op, stored) == nil) != tc.valid {
				t.Fatal("spam rules readback accepted another source or rejected the image copy")
			}
		})
	}
}
