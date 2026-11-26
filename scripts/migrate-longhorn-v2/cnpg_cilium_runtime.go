package main

import (
	"errors"
	"regexp"
	"strings"
)

const recoveryCiliumSourceCommit = "150121410c147fad575ac5dcbce6de923be69042"

var recoveryCiliumRevision = regexp.MustCompile(`^[0-9a-f]{8,40}$`)
var recoveryPolicyConfigMarker = regexp.MustCompile(`(?m)^.*⚙️ \(types\.PolicyConfig\).*$`)
var recoveryPolicyConfigBlock = regexp.MustCompile(`(?m)^([\t ]*)⚙️ \(types\.PolicyConfig\) \{\r?\n([\t ]+)PolicyDefaultLocalCluster: \(bool\) (true|false)\r?\n([\t ]*)\}\r?$`)

// Inputs come from fixed read-only native commands in the same bound agent
// process: config --all -o json, version -o json, and shell hive. ConfigMap
// contents and default values cannot establish this runtime observation.
// The caller must still prove agent Pod/container continuity around the reads.
func readRecoveryCiliumRuntime(configJSON, versionJSON, hiveText []byte) (recoveryCiliumConfig, error) {
	var cfg recoveryCiliumConfig
	for _, raw := range [][]byte{configJSON, versionJSON, hiveText} {
		if len(raw) == 0 || len(raw) > 32<<20 {
			return cfg, errors.New("native Cilium runtime evidence is empty or exceeds its bound")
		}
	}
	var config, version object
	if decodeIsolationJSON(configJSON, &config) != nil || decodeIsolationJSON(versionJSON, &version) != nil {
		return cfg, errors.New("native Cilium runtime JSON is malformed")
	}
	cfg.Version = text(version, "Daemon", "Version")
	revision, clientRevision := text(version, "Daemon", "Revision"), text(version, "Client", "Revision")
	// The native version command can return success with an empty Daemon object.
	// A matching CLI version by itself never establishes a running daemon.
	if cfg.Version != "1.20.5" || text(version, "Client", "Version") != cfg.Version || revision != clientRevision || !recoveryCiliumRevision.MatchString(revision) || !strings.HasPrefix(recoveryCiliumSourceCommit, revision) {
		return cfg, errors.New("native Cilium daemon or client does not identify the reviewed build")
	}
	cfg.Cluster = text(config, "ClusterName")
	cfg.Enforcement = text(config, "policy-enforcement")
	var nonDefaultOK, l7OK, restoreOK bool
	cfg.NonDefaultDeny, nonDefaultOK = value(config, "EnableNonDefaultDenyPolicies").(bool)
	cfg.L7, l7OK = value(config, "EnableL7Proxy").(bool)
	// Endpoint creation's startup-cache barrier depends on the actual restore
	// option, exposed as RestoreState by the native running-config API.
	cfg.RestoreState, restoreOK = value(config, "RestoreState").(bool)
	// PolicyDefaultLocalCluster is a separate live Hive cell, absent from the
	// JSON config API. Match its complete native struct, not a similarly named
	// field or an arbitrary occurrence of 'true' elsewhere in the diagnostic.
	markers := recoveryPolicyConfigMarker.FindAll(hiveText, -1)
	blocks := recoveryPolicyConfigBlock.FindAllSubmatch(hiveText, -1)
	if len(markers) != 1 || len(blocks) != 1 || string(blocks[0][1]) != string(blocks[0][4]) || !strings.HasPrefix(string(blocks[0][2]), string(blocks[0][1])) || len(blocks[0][2]) <= len(blocks[0][1]) || string(blocks[0][3]) != "true" || !nonDefaultOK || !l7OK || !restoreOK {
		return cfg, errors.New("native Cilium policy configuration is missing, ambiguous or changed")
	}
	cfg.DefaultLocalCluster = true
	if !supportedRecoveryCiliumConfig(cfg) {
		return cfg, errors.New("native Cilium runtime configuration differs from the reviewed recovery binding")
	}
	return cfg, nil
}
