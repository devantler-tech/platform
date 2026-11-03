package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCSIInventoryPreservesNativeListResourceVersion(t *testing.T) {
	for _, continuation := range []string{"", "unread-page"} {
		calls := 0
		c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
			calls++
			if program != "kubectl" || strings.Join(args, " ") != "--context=admin@prod --request-timeout=20s get --raw=/api/v1/persistentvolumes?limit=500" {
				t.Fatal("watch inventory did not read the fixed native collection")
			}
			return []byte(`{"apiVersion":"v1","kind":"PersistentVolumeList","metadata":{"resourceVersion":"12345","continue":"` + continuation + `"},"items":[]}`), nil
		}}
		items, rv, err := csiInventory(t.Context(), c, "persistentvolumes")
		if continuation == "" && (err != nil || rv != "12345" || len(items) != 0 || calls != 1) {
			t.Fatalf("lost native list version: rv=%s err=%v", rv, err)
		}
		if continuation != "" && err == nil {
			t.Fatal("a partial list was accepted as a gap-free watch origin")
		}
	}
}

func TestCSICaptureKeepsTheOwnedBindingAfterNativeCleanup(t *testing.T) {
	for _, kind := range []string{"DataUpload", "DataDownload"} {
		claim, pv, volume, op, source, class := csiScratchFixture(kind)
		capture := newCSICapture("1234")
		// The backend may arrive before its PV and claim. Native cleanup may
		// remove all three before a backup's completion becomes observable.
		for _, event := range []struct {
			query string
			value object
		}{
			{"volumes.longhorn.io", volume}, {"persistentvolumes", pv}, {"persistentvolumeclaims", claim},
		} {
			if err := capture.observe(event.query, event.value); err != nil {
				t.Fatal(err)
			}
		}
		proof, err := capture.proof(op, source, class)
		if err != nil || proof.Volume.UID != text(volume, "metadata", "uid") {
			t.Fatalf("owned transient binding lost: %v", err)
		}
		encoded, _ := json.Marshal(pv)
		var foreign object
		if err := json.Unmarshal(encoded, &foreign); err != nil {
			t.Fatal(err)
		}
		foreign["metadata"].(object)["uid"] = "replacement-pv"
		foreign["spec"].(object)["csi"].(object)["volumeHandle"] = "unrelated-volume"
		if err := capture.observe("persistentvolumes", foreign); err != nil {
			t.Fatal(err)
		}
		if _, err := capture.proof(op, source, class); err == nil {
			t.Fatal("a replaced or ambiguous PV name was accepted")
		}
	}
}

func TestCSIWatchRefusesGapsAndMalformedEvents(t *testing.T) {
	for _, input := range []string{
		`{"type":"ERROR","object":{"code":410}}`,
		`{"type":"ADDED","object":{"metadata":{"name":"claim"}}}`,
		`{"type":"MODIFIED","object":`,
		`{"type":"UNRECOGNIZED","object":{}}`,
		"",
	} {
		if err := readCSIWatch(strings.NewReader(input), func(object) error { return nil }); err == nil {
			t.Fatalf("incomplete watch accepted: %q", input)
		}
	}
}
