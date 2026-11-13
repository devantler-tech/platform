package main

import "errors"

// ingressFailure carries a code chosen by reviewed code, never remote output.
type ingressFailure string

func (ingressFailure) Error() string { return "inconclusive" }

func ingressFailureStage(err error) string {
	var stage ingressFailure
	if errors.As(err, &stage) {
		switch stage {
		case "guard", "pod_before", "proxy_before", "node_before", "cilium_before",
			"datastore_before", "settings_before", "credential_before", "authorization",
			"access_before", "access_decode", "ingress_input", "cluster_before", "daemon_before",
			"agent_before", "public_probes", "window", "flow_filter", "flow_read", "flow_empty",
			"flow_decode", "agent_after", "daemon_after", "cluster_after", "access_after",
			"access_compare", "pod_after", "proxy_after", "node_after", "cilium_after",
			"datastore_after", "settings_after", "credential_after":
			return string(stage)
		}
	}
	return "observation"
}
