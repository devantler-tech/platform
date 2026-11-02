package main

import (
	"encoding/json"
	"io"
	"os"
	"syscall"

	"github.com/devantler-tech/platform/internal/strictjson"
)

func privateR2JSON(path string, target any, fieldSets ...[]string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errStore
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0027 != 0 || info.Size() > 8192 {
		_ = f.Close()
		return errStore
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, 8193))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) == 0 || len(raw) > 8192 {
		return errStore
	}
	for _, fields := range fieldSets {
		if strictjson.Validate(raw, map[string][]string{"": fields}) == nil && exactJSONFields(raw, fields...) && json.Unmarshal(raw, target) == nil {
			return nil
		}
	}
	return errStore
}

func loadR2Facts(configPath, credentialPath string) (r2Settings, r2Credentials, error) {
	var facts r2Settings
	var credential r2Credentials
	var fields map[string]json.RawMessage
	if privateR2JSON(configPath, &facts, []string{"accountId", "bucket", "key", "instance"}) != nil || privateR2JSON(credentialPath, &fields, []string{"accessKeyId", "secretAccessKey"}, []string{"accessKeyId", "secretAccessKey", "sessionToken"}) != nil || json.Unmarshal(fields["accessKeyId"], &credential.AccessKeyID) != nil || json.Unmarshal(fields["secretAccessKey"], &credential.SecretAccessKey) != nil {
		return r2Settings{}, r2Credentials{}, errStore
	}
	if raw, present := fields["sessionToken"]; present {
		if json.Unmarshal(raw, &credential.SessionToken) != nil || credential.SessionToken == "" {
			return r2Settings{}, r2Credentials{}, errStore
		}
	}
	return facts, credential, nil
}
