package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

// The spam filter imports its rules from this file at a normal start that
// finds no stored rule. The image derives it at build time from the pinned
// stalwartlabs/spam-filter v3.0.2 release, the last in the 0.16 rule format,
// so the server never fetches the unpinned upstream URL that egress blocks.
const (
	// upstreamSpamRulesURL is the server's own default rules source, in both
	// v0.16.27 and v1.0.3 (registry/src/schema/structs_impl.rs).
	upstreamSpamRulesURL = "https://github.com/stalwartlabs/spam-filter/releases/latest/download/spam-filter-rules.json.gz"
	spamRulesPath        = "/usr/local/share/stalwart/spam-filter-rules-v3.0.2.json"
	spamRulesSHA256      = "1d3a66f6fa9a32f711c5b3a73853663eff27c994a46e8f6a0db64a9b2a580f52"
)

// The reviewed object counts of the pinned release, per type and per lookup.
var (
	spamRulesCounts     = map[string]int{"SpamRule": 66, "SpamTag": 403, "SpamFileExtension": 132, "SpamDnsblServer": 18, "HttpLookup": 4, "MemoryLookupKey": 2084}
	spamRulesNamespaces = map[string]int{"url-redirectors": 1076, "trusted-domains": 828, "surbl-hashbl": 180}
)

// The shortened-link check requests links found in received mail, and runs
// only for a host in this lookup, so leaving its keys out turns the check off.
const shortenedLinkNamespace = "url-redirectors"

// deriveSpamRules accepts only the pinned release bytes.
func deriveSpamRules(release []byte) ([]byte, error) {
	digest := sha256.Sum256(release)
	if hex.EncodeToString(digest[:]) != spamRulesSHA256 {
		return nil, errConfiguration
	}
	reader, err := gzip.NewReader(bytes.NewReader(release))
	if err != nil {
		return nil, errConfiguration
	}
	content, err := io.ReadAll(io.LimitReader(reader, 4<<20+1))
	if err != nil || len(content) > 4<<20 || reader.Close() != nil {
		return nil, errConfiguration
	}
	return filterSpamRules(content)
}

// filterSpamRules refuses content other than the reviewed release and leaves
// out only the shortened-link lookup keys.
func filterSpamRules(content []byte) ([]byte, error) {
	var objects map[string][]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(content))
	if decoder.Decode(&objects) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errConfiguration
	}
	counts := map[string]int{}
	for kind, values := range objects {
		counts[kind] = len(values)
	}
	if !reflect.DeepEqual(counts, spamRulesCounts) {
		return nil, errConfiguration
	}
	namespaces := map[string]int{}
	kept := []json.RawMessage{}
	for _, raw := range objects["MemoryLookupKey"] {
		var key struct {
			Namespace string `json:"namespace"`
		}
		if json.Unmarshal(raw, &key) != nil {
			return nil, errConfiguration
		}
		namespaces[key.Namespace]++
		if key.Namespace != shortenedLinkNamespace {
			kept = append(kept, raw)
		}
	}
	if !reflect.DeepEqual(namespaces, spamRulesNamespaces) {
		return nil, errConfiguration
	}
	objects["MemoryLookupKey"] = kept
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(objects) != nil {
		return nil, errConfiguration
	}
	return output.Bytes(), nil
}

// spamRulesCommand writes the derived rules once, read-only, at image build.
func spamRulesCommand(args []string, log io.Writer) int {
	f := flag.NewFlagSet("spam-rules", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "pinned spam filter release")
	output := f.String("output", "", "new derived rules file")
	fail := func() int { _, _ = fmt.Fprintln(log, "Spam filter rules refused."); return 2 }
	if f.Parse(args) != nil || f.NArg() != 0 || !filepath.IsAbs(*input) || !filepath.IsAbs(*output) {
		return fail()
	}
	source, err := os.OpenFile(*input, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fail()
	}
	release, err := io.ReadAll(io.LimitReader(source, 1<<20+1))
	if source.Close() != nil || err != nil || len(release) > 1<<20 {
		return fail()
	}
	derived, err := deriveSpamRules(release)
	if err != nil {
		return fail()
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
	if err != nil {
		return fail()
	}
	_, writeErr := file.Write(derived)
	chmodErr := file.Chmod(0444)
	if closeErr := file.Close(); writeErr != nil || chmodErr != nil || closeErr != nil {
		_ = os.Remove(*output)
		return fail()
	}
	if _, err := fmt.Fprintln(log, "Spam filter rules derived from the pinned release."); err != nil {
		return 2
	}
	return 0
}
