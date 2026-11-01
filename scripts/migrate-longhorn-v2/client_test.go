package main

import "testing"

func TestListRequiresCompleteDistinctResourceIdentities(t *testing.T) {
	good := []byte(`{"metadata":{},"items":[{"metadata":{"name":"volume","uid":"uid","resourceVersion":"1"}}]}`)
	if list, err := decodeList(good); err != nil || len(list) != 1 {
		t.Fatalf("complete list rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"no items":       `{}`,
		"null items":     `{"items":null}`,
		"invalid JSON":   `{"items":[`,
		"continuation":   `{"metadata":{"continue":"next"},"items":[]}`,
		"missing UID":    `{"items":[{"metadata":{"name":"volume"}}]}`,
		"duplicate name": `{"items":[{"metadata":{"name":"volume","uid":"a"}},{"metadata":{"name":"volume","uid":"b"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeList([]byte(raw)); err == nil {
				t.Fatal("incomplete resource inventory accepted")
			}
		})
	}
}
