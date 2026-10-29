package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"testing"
)

func TestReleasedFrontendEntryPoint(t *testing.T) {
	// Extracted without execution from /app/index.html in the pinned public
	// Hubble UI v0.13.6 image. Its minifier omits the optional opening html tag.
	body, err := os.ReadFile("testdata/frontend-v0.13.6-index.html")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("<html")) {
		t.Fatal("the regression must exercise the released minified entry point")
	}
	for _, fixture := range []struct {
		name, contentType string
		status            int
		body              []byte
		readErr           error
		want              bool
	}{
		{"released", "text/html", http.StatusOK, body, nil, true},
		{"media-type-parameters", "Text/HTML; charset=utf-8", http.StatusOK, body, nil, true},
		{"wrong-status", "text/html", http.StatusNotFound, body, nil, false},
		{"wrong-media-type", "application/json", http.StatusOK, body, nil, false},
		{"ambiguous-media-type", "text/html,application/json", http.StatusOK, body, nil, false},
		{"altered-entry-point", "text/html", http.StatusOK, append(append([]byte(nil), body...), 'x'), nil, false},
		{"truncated-entry-point", "text/html", http.StatusOK, body[:len(body)/2], nil, false},
		{"failed-read", "text/html", http.StatusOK, body, errors.New("fixture read failed"), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if got := validFrontendIndex(fixture.status, fixture.contentType, fixture.body, fixture.readErr); got != fixture.want {
				t.Fatalf("entry point accepted=%t, want %t", got, fixture.want)
			}
		})
	}
}
