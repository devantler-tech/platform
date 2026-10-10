//go:build apiserverpatch

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

// Exercise the actual marker patch over the fixed mTLS transport with the pinned
// apiserver JSON-patch library, not a mock that merely accepts supplied operations.
func TestOwnedPauseMarkerUsesRealConditionalPatch(t *testing.T) {
	for _, drift := range []string{"", "UID", "version", "primary", "pause", "annotations", "refresh", "refresh annotations"} {
		t.Run(drift, func(t *testing.T) {
			s, _ := completedFixture()
			at(s.cluster, "metadata", "annotations")[pauseKey] = "disabled"
			marker := "123.1"
			if strings.HasPrefix(drift, "refresh") {
				at(s.cluster, "metadata", "annotations")[pauseObservationKey] = marker
				marker += ".2"
			}
			var state object
			if err := json.Unmarshal(transportJSON(t, s.cluster), &state); err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "UID":
				at(state, "metadata")["uid"] = "other"
			case "version":
				at(state, "metadata")["resourceVersion"] = "other"
			case "primary":
				at(state, "status")["currentPrimary"] = "other"
			case "pause":
				delete(at(state, "metadata", "annotations"), pauseKey)
			case "annotations", "refresh annotations":
				at(state, "metadata", "annotations")[pauseObservationKey] = "foreign"
			}
			before := string(transportJSON(t, state))
			requests := 0
			_, config := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPatch || r.URL.Path != clusterAPIPath || r.URL.Query().Get("fieldManager") != fieldManager {
					t.Error("unexpected write")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				patch, err := jsonpatch.DecodePatch(body)
				if err != nil {
					t.Error(err)
					return
				}
				updated, err := patch.Apply(transportJSON(t, state))
				w.Header().Set("Content-Type", "application/json")
				if err != nil {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Invalid","code":422}`))
					return
				}
				if err := json.Unmarshal(updated, &state); err != nil {
					t.Error(err)
				}
				_, _ = w.Write(updated)
			})
			var fallback atomic.Int32
			c := client{command: configuredCommand(t, config, &fallback)}
			err := c.write(context.Background(), patchArgs("cluster", s.cluster), observationPatch(s.cluster, str(s.cluster, "status", "currentPrimary"), marker))
			accepted := drift == "" || drift == "refresh"
			if (err == nil) != accepted || requests != 1 || fallback.Load() != 0 {
				t.Fatalf("drift=%s requests=%d error=%v", drift, requests, err)
			}
			if accepted {
				if str(state, "metadata", "annotations", pauseKey) != "disabled" || str(state, "metadata", "annotations", pauseObservationKey) != marker {
					t.Fatal("accepted observation did not leave reconciliation disabled")
				}
			} else if string(transportJSON(t, state)) != before {
				t.Fatal("rejected observation persisted a partial write")
			}
		})
	}
}

func TestOwnedPauseResumeAtomicallyRemovesOnlyOwnMarkerAndPause(t *testing.T) {
	for _, marker := range []string{"123.1", "foreign"} {
		t.Run(marker, func(t *testing.T) {
			s, _ := completedFixture()
			at(s.cluster, "metadata", "annotations")[pauseKey] = "disabled"
			at(s.cluster, "metadata", "annotations")[pauseObservationKey] = marker
			at(s.cluster, "metadata", "annotations")["unrelated"] = "preserve"
			before := transportJSON(t, s.cluster)
			body, err := json.Marshal(resumePausePatch(s.cluster, str(s.cluster, "status", "currentPrimary"), "123.1"))
			if err != nil {
				t.Fatal(err)
			}
			patch, err := jsonpatch.DecodePatch(body)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := patch.Apply(before)
			if marker == "foreign" {
				if err == nil {
					t.Fatal("foreign marker permitted resume")
				}
				return
			}
			var cluster object
			if err != nil || json.Unmarshal(updated, &cluster) != nil || str(cluster, "metadata", "annotations", "unrelated") != "preserve" || value(cluster, "metadata", "annotations", pauseKey) != nil || value(cluster, "metadata", "annotations", pauseObservationKey) != nil {
				t.Fatalf("resume changed unrelated annotations or retained its controls: %v", err)
			}
		})
	}
}
