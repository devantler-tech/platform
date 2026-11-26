package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// These are native Parse -> RulesToPolicyEntries results, not a second
// implementation of Cilium's Kubernetes policy normalization.
//
//go:embed testdata/cnpg-isolation-input-cilium-1.20.5.json
var recoveryIsolationInput []byte

//go:embed testdata/cnpg-isolation-entries-cilium-1.20.5.json
var recoveryIsolationEntries []byte

type recoveryCiliumConfig struct {
	Version, Cluster, Enforcement                         string
	DefaultLocalCluster, NonDefaultDeny, L7, RestoreState bool
}

func supportedRecoveryCiliumConfig(cfg recoveryCiliumConfig) bool {
	return cfg == (recoveryCiliumConfig{Version: "1.20.5", Cluster: "default", Enforcement: "default", DefaultLocalCluster: true, NonDefaultDeny: true, L7: true, RestoreState: true})
}

var isolationNamespace = regexp.MustCompile(`^longhorn-db-proof-(umami-db|coroot-db|wedding-db|customer-identity-db|business-site-portal-db)-[1-9][0-9]{0,19}$`)
var isolationUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func decodeIsolationJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing isolation observation")
	}
	return nil
}

func replaceIsolationFixture(v any, replacements map[string]string) any {
	switch x := v.(type) {
	case string:
		if replacement, ok := replacements[x]; ok {
			return replacement
		}
	case map[string]any:
		for key, child := range x {
			x[key] = replaceIsolationFixture(child, replacements)
		}
	case []any:
		for i, child := range x {
			x[i] = replaceIsolationFixture(child, replacements)
		}
	}
	return v
}

func expectedCNPGIsolationEntries(policy object, endpoint string, cfg recoveryCiliumConfig) ([]object, error) {
	// A changed release or normalization setting requires regenerating and
	// reviewing the native fixture. Zero values are never runtime evidence.
	if !supportedRecoveryCiliumConfig(cfg) {
		return nil, errors.New("recovery isolation lacks the reviewed native Cilium configuration")
	}
	ns, uid := text(policy, "metadata", "namespace"), text(policy, "metadata", "uid")
	generation, ok := value(policy, "metadata", "generation").(float64)
	if text(policy, "apiVersion") != "cilium.io/v2" || text(policy, "kind") != "CiliumNetworkPolicy" || text(policy, "metadata", "name") != "restore-isolation" || !isolationNamespace.MatchString(ns) || !isolationUID.MatchString(uid) || !ok || generation < 1 || generation != float64(int64(generation)) || value(policy, "metadata", "deletionTimestamp") != nil || !recoveryEndpoint.MatchString(endpoint) {
		return nil, errors.New("recovery isolation CNP identity or archive binding is incomplete")
	}
	var template object
	if err := json.Unmarshal(recoveryIsolationInput, &template); err != nil {
		return nil, err
	}
	fixtureNS, fixtureUID := text(template, "metadata", "namespace"), text(template, "metadata", "uid")
	fqdn := value(template, "spec", "egress").([]any)[2].(object)["toFQDNs"].([]any)[0].(object)
	fixtureHost := text(fqdn, "matchName")
	host := strings.TrimPrefix(endpoint, "https://")
	fqdn["matchName"] = host
	if !reflect.DeepEqual(value(policy, "spec"), value(template, "spec")) {
		return nil, errors.New("recovery CNP differs from the native-reviewed isolation rules")
	}
	var rawEntries []any
	if err := json.Unmarshal(recoveryIsolationEntries, &rawEntries); err != nil {
		return nil, err
	}
	replaced := replaceIsolationFixture(rawEntries, map[string]string{
		fixtureNS: ns, fixtureUID: uid,
		"MatchName: " + fixtureHost + ", MatchPattern: ": "MatchName: " + host + ", MatchPattern: ",
	}).([]any)
	entries := make([]object, 0, len(replaced))
	for _, entry := range replaced {
		entries = append(entries, entry.(object))
	}
	return entries, nil
}

// This establishes only one part of isolation: the exact identified CNP's
// imported native multiset and that response's repository revision. The caller
// must bind each actual agent/config/process, re-read the unchanged CNP before
// Pod creation, and later prove the owned endpoint realized enforcement and the
// positive/negative network paths. It grants no mutation clearance on its own.
func decodeRecoveryPolicyRepository(raw []byte) (uint64, []object, error) {
	if len(raw) == 0 || len(raw) > 32<<20 {
		return 0, nil, errors.New("native policy repository observation is empty or exceeds its bound")
	}
	var response struct {
		Revision uint64 `json:"revision"`
		Policy   string `json:"policy"`
	}
	if err := decodeIsolationJSON(raw, &response); err != nil || response.Revision == 0 || response.Revision > 1<<63-1 || response.Policy == "" {
		return 0, nil, errors.New("native policy repository revision or encoded entries are malformed")
	}
	var entries []object
	if err := decodeIsolationJSON([]byte(response.Policy), &entries); err != nil || len(entries) == 0 || len(entries) > 16384 {
		return 0, nil, errors.New("native flattened policy entries are malformed or exceed their bound")
	}
	return response.Revision, entries, nil
}

func verifyCNPGIsolationImport(raw []byte, policy object, endpoint string, cfg recoveryCiliumConfig) (uint64, error) {
	expected, err := expectedCNPGIsolationEntries(policy, endpoint, cfg)
	if err != nil {
		return 0, err
	}
	revision, entries, err := decodeRecoveryPolicyRepository(raw)
	if err != nil {
		return 0, err
	}
	if err := verifyRecoveryInitEntries(entries); err != nil {
		return 0, err
	}
	ns, uid := text(policy, "metadata", "namespace"), text(policy, "metadata", "uid")
	if err := verifyRecoveryPolicyAllowances(entries, expected, ns); err != nil {
		return 0, err
	}
	selected := []object{}
	for _, entry := range entries {
		labels, ok := entry["Labels"].([]any)
		if entry["Labels"] == nil {
			continue
		}
		if !ok {
			return 0, errors.New("native policy identity labels are malformed")
		}
		identities := map[string]string{}
		duplicate := false
		for _, label := range labels {
			item, ok := label.(object)
			if !ok {
				return 0, errors.New("native policy identity label is malformed")
			}
			if text(item, "source") != "k8s" || !strings.HasPrefix(text(item, "key"), "io.cilium.k8s.policy.") {
				continue
			}
			key := text(item, "key")
			if _, seen := identities[key]; seen {
				duplicate = true
			}
			identities[key] = text(item, "value")
		}
		sameUID := identities["io.cilium.k8s.policy.uid"] == uid
		samePolicy := identities["io.cilium.k8s.policy.name"] == "restore-isolation" && identities["io.cilium.k8s.policy.namespace"] == ns
		if !sameUID && !samePolicy {
			continue
		}
		if duplicate || len(identities) != 4 || identities["io.cilium.k8s.policy.uid"] != uid || identities["io.cilium.k8s.policy.name"] != "restore-isolation" || identities["io.cilium.k8s.policy.namespace"] != ns || identities["io.cilium.k8s.policy.derived-from"] != "CiliumNetworkPolicy" {
			return 0, errors.New("native recovery policy identity is stale or ambiguous")
		}
		selected = append(selected, entry)
	}
	canonical := func(rules []object) ([]string, error) {
		result := make([]string, 0, len(rules))
		for _, rule := range rules {
			encoded, e := json.Marshal(rule)
			if e != nil {
				return nil, e
			}
			result = append(result, string(encoded))
		}
		sort.Strings(result)
		return result, nil
	}
	want, err := canonical(expected)
	if err != nil {
		return 0, err
	}
	got, err := canonical(selected)
	if err != nil {
		return 0, err
	}
	if !reflect.DeepEqual(want, got) {
		return 0, errors.New("native recovery policy import is incomplete or changed")
	}
	return revision, nil
}
