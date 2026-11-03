package namespaceinventory

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type heldClose struct {
	io.Reader
	entered chan struct{}
	release chan struct{}
}

func (b heldClose) Close() error { close(b.entered); <-b.release; return nil }

func TestUnsignalledDeadlineBoundsHeldResponseClose(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	expires := &atomic.Int64{}
	expires.Store(time.Now().Add(2 * time.Second).UnixNano())
	ctx := unsignalledDeadline{context.Background(), expires}
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: heldClose{strings.NewReader(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"` + testNamespace + `","uid":"namespace-uid","resourceVersion":"1"}}`), entered, release}}, nil
	})}
	endpoint, _ := url.Parse("https://reviewed.invalid")
	done := make(chan error, 1)
	go func() {
		result, err := Observe(ctx, client, endpoint, testNamespace, "namespace-uid")
		if len(result.Bindings()) != 0 {
			t.Error("held cleanup retained evidence")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("response cleanup did not start")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("held response cleanup was accepted")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("held Close outlived its unsignalled deadline")
	}
}
