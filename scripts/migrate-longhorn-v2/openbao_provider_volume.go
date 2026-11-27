package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// The provider's native response contains the authoritative assignee. CLI
// versions that project only the deprecated server field cannot prove detach.
func readOpenBaoProviderVolume(ctx context.Context, transport http.RoundTripper, token, handle string) (object, error) {
	id, err := strconv.ParseInt(handle, 10, 64)
	if token == "" || err != nil || id <= 0 || id >= 1<<53 || strconv.FormatInt(id, 10) != handle {
		return nil, errOpenBaoProviderStorage
	}
	if transport == nil {
		direct := &http.Transport{DisableCompression: true}
		defer direct.CloseIdleConnections()
		transport = direct
	}
	api := &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.hetzner.cloud/v1/volumes/"+handle, nil)
	if err != nil {
		return nil, errOpenBaoProviderStorage
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := api.Do(req)
	if err != nil {
		return nil, errOpenBaoProviderStorage
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, benchmarkProviderReplyLimit+1))
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil ||
		len(raw) > benchmarkProviderReplyLimit || strictjson.Validate(raw, map[string][]string{
		"":              {"volume"},
		"volume":        {"id", "size", "status", "assignee", "server", "labels", "linux_device"},
		"volume.labels": {"managed-by", "pv-name", "pvc-name", "pvc-namespace"},
	}) != nil {
		return nil, errOpenBaoProviderStorage
	}
	var native struct{ Volume object }
	if json.Unmarshal(raw, &native) != nil || native.Volume["id"] != float64(id) {
		return nil, errOpenBaoProviderStorage
	}
	if _, known := native.Volume["assignee"]; !known {
		return nil, errOpenBaoProviderStorage
	}
	return native.Volume, nil
}
