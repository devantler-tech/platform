package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"

	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/option"
	policyapi "github.com/cilium/cilium/pkg/policy/api"
	policyutils "github.com/cilium/cilium/pkg/policy/utils"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func main() {
	if len(os.Args) != 5 || os.Args[1] == "" {
		panic("four explicit agent configuration inputs required")
	}
	cluster := os.Args[1]
	local := must(strconv.ParseBool(os.Args[2]))
	option.Config.EnableNonDefaultDenyPolicies = must(strconv.ParseBool(os.Args[3]))
	option.Config.EnableL7Proxy = must(strconv.ParseBool(os.Args[4]))
	policyapi.InitEntities(cluster)
	var p v2.CiliumNetworkPolicy
	dec := json.NewDecoder(os.Stdin)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		panic(err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		panic("trailing input")
	}
	if p.APIVersion != "cilium.io/v2" || p.Kind != "CiliumNetworkPolicy" || p.Name == "" || p.Namespace == "" || p.UID == "" || p.Spec == nil || p.Specs != nil {
		panic("one identified namespaced CNP spec required")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rules := must(p.Parse(logger, cmtypes.LocalClusterNameForPolicies(cmtypes.PolicyConfig{PolicyDefaultLocalCluster: local}, cluster)))
	if err := json.NewEncoder(os.Stdout).Encode(policyutils.RulesToPolicyEntries(rules)); err != nil {
		panic(err)
	}
}
