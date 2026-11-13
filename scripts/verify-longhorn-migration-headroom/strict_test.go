package main

import (
	"context"
	"strings"
	"testing"
)

func TestAmbiguousInventoryCannotHideMembers(t *testing.T) {
	valid := providerResponse(t, nil)
	for _, body := range []string{
		strings.Replace(valid, `"servers":`, `"servers":[{"id":999}],"servers":`, 1),
		strings.Replace(valid, `"servers":`, `"Servers":`, 1),
		strings.Replace(valid, `"total_entries":8`, `"total_entries":9,"total_entries":8`, 1),
		strings.Replace(valid, `"id":100`, `"ID":100`, 1),
	} {
		if _, err := collectServers(context.Background(), providerClient(t, body, 200), "private-test-token"); err == nil {
			t.Fatal("ambiguous provider inventory accepted")
		}
	}
	_, nodes := roster(t, false)
	valid = string(nodeResponse(t, nodes))
	for _, body := range []string{
		strings.Replace(valid, `"items":`, `"items":[{"metadata":{"name":"hidden"}}],"items":`, 1),
		strings.Replace(valid, `"items":`, `"Items":`, 1),
		strings.Replace(valid, `"providerID":`, `"ProviderID":`, 1),
		strings.Replace(valid, `"resourceVersion":"123"`, `"resourceVersion":"123","remainingItemCount":1`, 1),
	} {
		if _, err := decodeNodes([]byte(body)); err == nil {
			t.Fatal("ambiguous node inventory accepted")
		}
	}
}
