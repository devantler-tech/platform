package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"time"
)

func observe(ctx context.Context, client *http.Client, token string, readNodes func(context.Context) ([]byte, error), requireV2 bool) (int, error) {
	if err := active(ctx); err != nil {
		return 0, err
	}
	beforeRaw, err := readNodes(ctx)
	if err != nil {
		return 0, errors.New("native node inventory is unavailable")
	}
	before, err := decodeNodes(beforeRaw)
	if err != nil {
		return 0, err
	}
	servers, err := collectServers(ctx, client, token)
	if err != nil {
		return 0, err
	}
	if err = qualify(servers, before, requireV2); err != nil {
		return 0, err
	}
	afterRaw, err := readNodes(ctx)
	if err != nil {
		return 0, errors.New("final native node inventory is unavailable")
	}
	after, err := decodeNodes(afterRaw)
	if err != nil {
		return 0, err
	}
	if !reflect.DeepEqual(before, after) {
		return 0, errors.New("node identities or availability changed during the provider read")
	}
	if err = active(ctx); err != nil {
		return 0, err
	}
	return len(servers), nil
}

func active(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}

type boundedOutput struct{ data bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxInventoryBytes-b.data.Len() {
		return 0, errors.New("native inventory exceeded its read bound")
	}
	return b.data.Write(p)
}

func readNativeNodes(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--context", "admin@prod", "--request-timeout=15s", "get", "--raw", "/api/v1/nodes?limit=1000")
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, errors.New("native node inventory read failed")
	}
	return output.data.Bytes(), nil
}
