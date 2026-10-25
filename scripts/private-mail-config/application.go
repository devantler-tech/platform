package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"syscall"
)

const applicationAssetPath = "/usr/local/share/stalwart/webui.zip"

//go:embed webui-pins.json
var applicationReleaseJSON []byte

func pinnedApplicationDigest() (string, error) {
	var release struct {
		Version       string `json:"version"`
		URL           string `json:"url"`
		SHA256        string `json:"sha256"`
		LicenseSHA256 string `json:"license_sha256"`
	}
	d := json.NewDecoder(bytes.NewReader(applicationReleaseJSON))
	d.DisallowUnknownFields()
	if d.Decode(&release) != nil || d.Decode(new(any)) != io.EOF {
		return "", errConfiguration
	}
	digest, err := hex.DecodeString(release.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", errConfiguration
	}
	return release.SHA256, nil
}

func verifyApplicationAsset(path, digest string) (result error) {
	expected, err := hex.DecodeString(digest)
	if err != nil || len(expected) != sha256.Size {
		return errConfiguration
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errConfiguration
	}
	defer func() {
		if f.Close() != nil {
			result = errConfiguration
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1048576 {
		return errConfiguration
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 1048577))
	if err != nil || n != info.Size() || n > 1048576 || !bytes.Equal(h.Sum(nil), expected) {
		return errConfiguration
	}
	return nil
}

// A green listener inventory does not cover applications sharing HTTP. Accept
// only the pinned server's default interface or this image's verified copy.
func validateApplications(methods [][]json.RawMessage) error {
	var query struct {
		IDs      []string `json:"ids"`
		Total    *int     `json:"total"`
		Position *int     `json:"position"`
	}
	var get struct {
		List []struct {
			ID          string          `json:"id"`
			Description string          `json:"description"`
			Enabled     *bool           `json:"enabled"`
			Resource    string          `json:"resourceUrl"`
			Prefixes    map[string]bool `json:"urlPrefix"`
			Unpack      json.RawMessage `json:"unpackDirectory"`
			OAuth       json.RawMessage `json:"oauthClientId"`
		} `json:"list"`
		Missing []string `json:"notFound"`
	}
	for i, expected := range []string{"x:Application/query", "x:Application/get"} {
		if len(methods[i]) != 3 {
			return errConfiguration
		}
		var name, tag string
		if json.Unmarshal(methods[i][0], &name) != nil || name != expected || json.Unmarshal(methods[i][2], &tag) != nil || tag != []string{"applications-query", "applications-get"}[i] {
			return errConfiguration
		}
	}
	if json.Unmarshal(methods[0][1], &query) != nil || json.Unmarshal(methods[1][1], &get) != nil || query.IDs == nil || query.Total == nil || query.Position == nil || *query.Position != 0 || *query.Total != len(query.IDs) || len(query.IDs) > 1 || get.List == nil || get.Missing == nil || len(get.Missing) != 0 || len(get.List) != len(query.IDs) {
		return errConfiguration
	}
	if len(get.List) == 0 {
		return nil
	}
	app := get.List[0]
	if app.ID == "" || app.ID != query.IDs[0] || app.Description != "Stalwart Web Interface" || app.Enabled == nil || !*app.Enabled || !reflect.DeepEqual(app.Prefixes, map[string]bool{"/admin": true, "/account": true}) || !bytes.Equal(bytes.TrimSpace(app.Unpack), []byte("null")) || !bytes.Equal(bytes.TrimSpace(app.OAuth), []byte("null")) {
		return errConfiguration
	}
	if app.Resource != "https://github.com/stalwartlabs/webui/releases/download/v1.0.11/webui.zip" && app.Resource != "file://"+applicationAssetPath {
		return errConfiguration
	}
	return nil
}
