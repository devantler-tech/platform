package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"regexp"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type recoveryCredentials struct{ Root, Unseal string }

const maxCredentialArchive = 128 << 20
const maxCredentialArchivePlain = 512 << 20

// readCredentialArchive admits only complete independent recovery evidence.
func readCredentialArchive(data []byte) (recoveryCredentials, error) {
	refuse := func() (recoveryCredentials, error) { return recoveryCredentials{}, errEvidence }
	if len(data) == 0 || len(data) > maxCredentialArchive {
		return refuse()
	}
	compressed := bufio.NewReader(bytes.NewReader(data))
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return refuse()
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	plain := &io.LimitedReader{R: gz, N: maxCredentialArchivePlain + 1}
	reader := tar.NewReader(plain)
	seen := map[string]bool{}
	var record []byte
	defer func() { clear(record) }()
	for count := 0; ; count++ {
		if count > 100000 {
			return refuse()
		}
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil || plain.N <= 0 {
			return refuse()
		}
		// Velero writes both the preferred-version object and a legacy copy.
		// Nothing is extracted to the host filesystem, including unrelated entries.
		if header.Name != "resources/secrets/namespaces/openbao/openbao-unseal.json" &&
			header.Name != "resources/secrets/v1-preferredversion/namespaces/openbao/openbao-unseal.json" {
			continue
		}
		if seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 16384 {
			return refuse()
		}
		seen[header.Name] = true
		body, readErr := io.ReadAll(reader)
		if readErr != nil || int64(len(body)) != header.Size {
			clear(body)
			return refuse()
		}
		if record == nil {
			record = body
		} else {
			equal := bytes.Equal(record, body)
			clear(body)
			if !equal {
				return refuse()
			}
		}
	}
	// Consume the gzip trailer to verify its checksum, and reject hidden tar
	// records, trailing compressed streams, truncation and decompression bombs.
	if !completeArchivePadding(plain) || plain.N <= 0 {
		return refuse()
	}
	if _, err := compressed.Peek(1); err != io.EOF {
		return refuse()
	}
	return credentialsFromArchiveObject(record)
}

// completeArchivePadding admits only bounded zero padding after tar's terminator.
func completeArchivePadding(reader io.Reader) bool {
	buffer := make([]byte, 32768)
	for {
		n, err := reader.Read(buffer)
		for _, value := range buffer[:n] {
			if value != 0 {
				return false
			}
		}
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
	}
}

// credentialsFromArchiveObject validates the one fixed resource before releasing its values.
func credentialsFromArchiveObject(raw []byte) (recoveryCredentials, error) {
	refuse := func() (recoveryCredentials, error) { return recoveryCredentials{}, errEvidence }
	canonical := map[string][]string{"": {"apiVersion", "kind", "metadata", "data"}, "metadata": {"name", "namespace", "uid"}, "data": {"root-token", "unseal-key"}}
	if strictjson.Validate(raw, canonical) != nil {
		return refuse()
	}
	var identity struct {
		APIVersion string `json:"apiVersion"`
		Metadata   struct{ UID string }
	}
	if json.Unmarshal(raw, &identity) != nil || identity.APIVersion != "v1" || !regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`).MatchString(identity.Metadata.UID) {
		return refuse()
	}
	values, err := fixedObject(raw, "Secret", "openbao", "openbao-unseal")
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9._-]{20,256}$`).MatchString(values["root-token"]) || !regexp.MustCompile(`^[A-Za-z0-9+/=]{32,256}$`).MatchString(values["unseal-key"]) {
		return refuse()
	}
	return recoveryCredentials{Root: values["root-token"], Unseal: values["unseal-key"]}, nil
}
