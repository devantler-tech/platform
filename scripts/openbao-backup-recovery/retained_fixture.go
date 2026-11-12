package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// retainedInputFixture supplies only synthetic data for the explicitly gated isolated fixture mode.
func retainedInputFixture(seed string) (map[string]any, error) {
	namespace := "email-data-recovery-41561215415"
	refs := [][3]string{{"Pod", "reader", namespace}, {"PersistentVolumeClaim", "data", namespace}, {"Namespace", namespace, ""}, {"StorageClass", namespace, ""}, {"CiliumNetworkPolicy", "isolation", namespace}, {"ConfigMap", "datastore", namespace}, {"Secret", "storage-key", namespace}, {"Secret", "test-authentication", namespace}}
	var inputs []any
	for i, ref := range refs {
		values := map[string]any{"synthetic": seed, "input_index": i}
		raw, err := json.Marshal(values)
		if err != nil {
			return nil, errEvidence
		}
		digest := sha256.Sum256(raw)
		inputs = append(inputs, map[string]any{"Kind": ref[0], "Name": ref[1], "Namespace": ref[2], "UID": fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), "Digest": hex.EncodeToString(digest[:]), "Values": values})
	}
	return map[string]any{"schema": "email-reader-inputs/v1", "trial_run": "41561215415", "trial_revision": "7e4486cad02c326004ac9a1a637bb5205dbfe76a", "bundle": map[string]any{"ReaderUID": "00000000-0000-4000-8000-000000000001", "ClaimUID": "00000000-0000-4000-8000-000000000002", "Inputs": inputs}}, nil
}
