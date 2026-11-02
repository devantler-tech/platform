package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

func registryCredential(raw []byte) (string, string, error) {
	fail := errors.New("authoritative registry credential is unavailable or ambiguous")
	var secret struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Type       string `json:"type"`
		Metadata   struct {
			Name              string  `json:"name"`
			Namespace         string  `json:"namespace"`
			UID               string  `json:"uid"`
			ResourceVersion   string  `json:"resourceVersion"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	fields := map[string][]string{"": {"apiVersion", "kind", "type", "metadata", "data"}, "metadata": {"name", "namespace", "uid", "resourceVersion", "deletionTimestamp"}}
	if strictjson.Validate(raw, fields) != nil || json.Unmarshal(raw, &secret) != nil || secret.APIVersion != "v1" || secret.Kind != "Secret" || secret.Type != "kubernetes.io/dockerconfigjson" || secret.Metadata.Name != "ghcr-auth" || secret.Metadata.Namespace != "kyverno" || secret.Metadata.UID == "" || secret.Metadata.ResourceVersion == "" || secret.Metadata.DeletionTimestamp != nil || len(secret.Data) != 1 || len(secret.Data[".dockerconfigjson"]) > 65536 {
		return "", "", fail
	}
	configRaw, err := base64.StdEncoding.DecodeString(secret.Data[".dockerconfigjson"])
	if err != nil {
		return "", "", fail
	}
	var config struct {
		Auths map[string]struct {
			Username      string `json:"username"`
			Password      string `json:"password"`
			Auth          string `json:"auth"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if strictjson.Validate(configRaw, map[string][]string{"": {"auths"}, "auths.ghcr.io": {"username", "password", "auth", "identitytoken"}}) != nil || json.Unmarshal(configRaw, &config) != nil || len(config.Auths) != 1 {
		return "", "", fail
	}
	a, ok := config.Auths["ghcr.io"]
	if !ok || a.IdentityToken != "" {
		return "", "", fail
	}
	username, password := a.Username, a.Password
	if a.Auth != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(a.Auth)
		u, p, found := strings.Cut(string(decoded), ":")
		if decodeErr != nil || !found || (username != "" && username != u) || (password != "" && password != p) {
			return "", "", fail
		}
		username, password = u, p
	}
	if username == "" || password == "" || strings.ContainsAny(username+password, "\r\n") {
		return "", "", fail
	}
	return username, password, nil
}
