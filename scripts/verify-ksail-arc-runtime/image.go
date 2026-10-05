package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

func runtimeDigest(data []byte, digest string) (string, error) {
	if !validDigest(digest) || len(data) > 4<<20 {
		return "", errors.New("invalid image evidence")
	}
	// Buildx --raw may append one display newline. Accept only bytes whose hash
	// equals the immutable signed descriptor, never reserialized JSON or a tag.
	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
		if len(data) == 0 || data[len(data)-1] != '\n' || fmt.Sprintf("sha256:%x", sha256.Sum256(data[:len(data)-1])) != digest {
			return "", errors.New("image descriptor mismatch")
		}
		data = data[:len(data)-1]
	}
	var manifest struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Manifests     []struct {
			Digest   string                                     `json:"digest"`
			Platform struct{ OS, Architecture, Variant string } `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 {
		return "", errors.New("invalid image manifest")
	}
	switch manifest.MediaType {
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
		return digest, nil
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		runtime := ""
		for _, item := range manifest.Manifests {
			if item.Platform.OS != "linux" || item.Platform.Architecture != "amd64" {
				continue
			}
			if runtime != "" || item.Platform.Variant != "" || !validDigest(item.Digest) {
				return "", errors.New("ambiguous amd64 runtime")
			}
			runtime = item.Digest
		}
		if runtime == "" {
			return "", errors.New("missing amd64 runtime")
		}
		return runtime, nil
	default:
		return "", errors.New("unsupported image manifest")
	}
}

func validDigest(digest string) bool {
	if len(digest) != 71 || digest[:7] != "sha256:" {
		return false
	}
	data, err := hex.DecodeString(digest[7:])
	return err == nil && len(data) == 32
}
