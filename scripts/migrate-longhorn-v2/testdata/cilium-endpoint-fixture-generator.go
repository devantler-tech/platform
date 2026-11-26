// Synthetic Cilium v1.20.5 exported-model fixtures. No live agent, Kubernetes
// client, network connection, or production state is used by this program.
// Run from the verified Cilium checkout with:
// GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go run -mod=vendor /tmp/longhorn-v2-cilium-endpoint-fixture-generator-5582.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-openapi/strfmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cilium/cilium/api/v1/models"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/option"
)

const outputDir = "/tmp/longhorn-v2-cilium-endpoint-synthetic-5582"

func main() {
	const (
		namespace            = "longhorn-db-proof-umami-db-1234"
		podName              = "restore-1"
		podUID               = "11111111-2222-4333-8444-555555555555"
		cepUID               = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		podIP                = "192.0.2.17"
		nodeIP               = "192.0.2.10"
		attachment           = "synthetic-opaque-cni-attachment:eth0"
		endpointID     int64 = 2718
		identityID     int64 = 1234
		policyRevision int64 = 817
	)
	identityLabels := []string{
		"k8s:cnpg.io/cluster=restore",
		"k8s:io.cilium.k8s.policy.cluster=default",
		"k8s:io.kubernetes.pod.namespace=" + namespace,
	}
	makePolicy := func() *models.EndpointPolicy {
		return &models.EndpointPolicy{
			ID:             identityID,
			Build:          policyRevision,
			PolicyRevision: policyRevision,
			PolicyEnabled:  models.EndpointPolicyEnabledBoth,
		}
	}
	library := option.GetEndpointMutableOptionLibrary()
	opts := option.NewIntOptions(&library)
	opts.SetBool(option.DropNotify, true)
	opts.SetBool(option.PolicyAuditMode, false)
	endpoint := &models.Endpoint{
		ID: endpointID,
		Status: &models.EndpointStatus{
			Realized:            &models.EndpointConfigurationSpec{Options: *opts.GetMutableModel()},
			State:               models.EndpointStateReady.Pointer(),
			Identity:            &models.Identity{ID: identityID, Labels: models.Labels(identityLabels)},
			ExternalIdentifiers: &models.EndpointIdentifiers{CniAttachmentID: attachment},
			Networking: &models.EndpointNetworking{
				Addressing:             []*models.AddressPair{{IPv4: podIP}},
				ContainerInterfaceName: "eth0",
			},
			Policy: &models.EndpointPolicyStatus{
				Realized:        makePolicy(),
				Spec:            makePolicy(),
				ProxyStatistics: []*models.ProxyStatistics{},
			},
		},
	}
	if err := endpoint.Validate(strfmt.Default); err != nil {
		panic(err)
	}
	cep := &ciliumv2.CiliumEndpoint{
		TypeMeta: metav1.TypeMeta{APIVersion: "cilium.io/v2", Kind: "CiliumEndpoint"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              podName,
			UID:               types.UID(cepUID),
			ResourceVersion:   "100001",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)),
			Annotations:       map[string]string{"example.com/synthetic-fixture": "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Pod", Name: podName, UID: types.UID(podUID),
				// The native CEP constructor does not set Controller:true.
			}},
		},
		Status: ciliumv2.EndpointStatus{
			ID:       endpointID,
			Identity: &ciliumv2.EndpointIdentity{ID: identityID, Labels: identityLabels},
			Networking: &ciliumv2.EndpointNetworking{
				Addressing: ciliumv2.AddressPairList{&ciliumv2.AddressPair{IPV4: podIP}},
				NodeIP:     nodeIP,
			},
			State: string(models.EndpointStateReady),
			// ExternalIdentifiers is optional in the CEP API and intentionally
			// absent in this fixture. The agent endpoint supplies the opaque CNI ID.
		},
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		panic(err)
	}
	// endpoint get uses an array even when one endpoint identifier is supplied.
	writeJSON("synthetic-endpoint-get-cilium-1.20.5.json", []*models.Endpoint{endpoint})
	writeJSON("synthetic-cep-cilium-1.20.5.json", cep)
}

func writeJSON(name string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(outputDir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		panic(err)
	}
	fmt.Println(path)
}
