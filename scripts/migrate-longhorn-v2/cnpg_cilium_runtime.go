package main

import (
	"errors"
	"regexp"
	"strings"
)

const recoveryCiliumSourceCommit = "150121410c147fad575ac5dcbce6de923be69042"

var recoveryCiliumRevision = regexp.MustCompile(`^[0-9a-f]{8,40}$`)

// Bind the fixed native configuration and version reads to the same agent's
// imported, identified policy at one repository revision. The caller must
// prove Pod and container continuity around these reads.
func readRecoveryCiliumRuntime(configJSON, versionJSON, repository []byte, policy object, archiveEndpoint string) (recoveryCiliumConfig, uint64, error) {
	cfg, err := readRecoveryCiliumConfiguration(configJSON, versionJSON)
	if err != nil {
		return recoveryCiliumConfig{}, 0, err
	}
	revision, err := verifyCNPGIsolationImport(repository, policy, archiveEndpoint, cfg)
	if err != nil {
		return recoveryCiliumConfig{}, 0, err
	}
	return cfg, revision, nil
}

func readRecoveryCiliumConfiguration(configJSON, versionJSON []byte) (recoveryCiliumConfig, error) {
	var cfg recoveryCiliumConfig
	for _, raw := range [][]byte{configJSON, versionJSON} {
		if len(raw) == 0 || len(raw) > 32<<20 {
			return recoveryCiliumConfig{}, errors.New("native Cilium runtime evidence is empty or exceeds its bound")
		}
	}
	var config, version object
	if decodeIsolationJSON(configJSON, &config) != nil || decodeIsolationJSON(versionJSON, &version) != nil {
		return recoveryCiliumConfig{}, errors.New("native Cilium runtime JSON is malformed")
	}
	cfg.Version = text(version, "Daemon", "Version")
	daemonRevision, clientRevision := text(version, "Daemon", "Revision"), text(version, "Client", "Revision")
	// The native version command can return success with an empty Daemon object.
	// A matching CLI version by itself never establishes a running daemon.
	if cfg.Version != "1.20.5" || text(version, "Client", "Version") != cfg.Version || daemonRevision != clientRevision || !recoveryCiliumRevision.MatchString(daemonRevision) || !strings.HasPrefix(recoveryCiliumSourceCommit, daemonRevision) {
		return recoveryCiliumConfig{}, errors.New("native Cilium daemon or client does not identify the reviewed build")
	}
	cfg.Cluster = text(config, "ClusterName")
	cfg.Enforcement = text(config, "policy-enforcement")
	var nonDefaultOK, l7OK, restoreOK bool
	cfg.NonDefaultDeny, nonDefaultOK = value(config, "EnableNonDefaultDenyPolicies").(bool)
	cfg.L7, l7OK = value(config, "EnableL7Proxy").(bool)
	// Endpoint creation's startup-cache barrier depends on the actual restore
	// option, exposed as RestoreState by the native running-config API.
	cfg.RestoreState, restoreOK = value(config, "RestoreState").(bool)
	// Cilium's config API omits PolicyDefaultLocalCluster. The policy has no
	// explicit cluster selectors, so Cilium adds the reviewed local-cluster
	// constraints to its peers only when importing with local normalization.
	// The candidate flag becomes evidence after the complete import matches.
	cfg.DefaultLocalCluster = true
	if !nonDefaultOK || !l7OK || !restoreOK || !supportedRecoveryCiliumConfig(cfg) {
		return recoveryCiliumConfig{}, errors.New("native Cilium runtime configuration differs from the reviewed recovery binding")
	}
	return cfg, nil
}
