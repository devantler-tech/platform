// Synthetic native Cilium v1.20.5 endpoint configuration. No agent connection
// or production state is used. Run from the verified source's vendor tree.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/option"
)

func main() {
	library := option.GetEndpointMutableOptionLibrary()
	opts := option.NewIntOptions(&library)
	opts.SetBool(option.DropNotify, true)
	opts.SetBool(option.PolicyAuditMode, false)
	cfg := &models.EndpointConfigurationStatus{
		Realized:  &models.EndpointConfigurationSpec{LabelConfiguration: &models.LabelConfigurationSpec{}, Options: *opts.GetMutableModel()},
		Immutable: *opts.GetImmutableModel(),
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(raw))
}
