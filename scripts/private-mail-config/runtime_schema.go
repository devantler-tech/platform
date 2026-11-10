package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

type runtimeSchema struct {
	version, domainEnabled, implicitTLS, applicationEnabled, applicationInterval, aliasEnabled string
}

// schemaFor selects an audited schema. Omission preserves the deployed baseline.
func schemaFor(version string) (runtimeSchema, error) {
	switch version {
	case "", "0.16.27":
		return runtimeSchema{"0.16.27", "isEnabled", "tlsImplicit", "enabled", "autoUpdateFrequency", "enabled"}, nil
	case "1.0.2":
		return runtimeSchema{"1.0.2", "enable", "implicitTls", "enable", "autoUpdateInterval", "enable"}, nil
	default:
		return runtimeSchema{}, errConfiguration
	}
}

// nativeSchemaBool refuses omitted, null, foreign and mixed schema properties.
func nativeSchemaBool(chosen, foreign json.RawMessage) (bool, error) {
	var value bool
	if len(foreign) != 0 || len(chosen) == 0 || bytes.Equal(bytes.TrimSpace(chosen), []byte("null")) || json.Unmarshal(chosen, &value) != nil {
		return false, errConfiguration
	}
	return value, nil
}

// verifyRuntimeTools refuses an incompatible migration toolchain before store access.
func verifyRuntimeTools(ctx context.Context, runtime bootstrapRuntime) error {
	for _, tool := range []struct{ path, version string }{
		{runtime.server, "1.0.2"},
		{runtime.cli, "stalwart-cli 1.0.14"},
	} {
		versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		command := exec.CommandContext(versionCtx, tool.path, "--version")
		var output boundedOutput
		command.Stdout, command.Stderr, command.WaitDelay = &output, &output, time.Second
		err := command.Run()
		cancel()
		if err != nil || string(bytes.TrimSpace(output.Bytes())) != tool.version {
			return errConfiguration
		}
	}
	return nil
}

// httpSettings keeps transport identity explicit in both native representations.
func httpSettings(v inputs, profile runtimeSchema) map[string]any {
	value := map[string]any{
		"allowedEndpoints":  map[string]any{"match": map[string]any{"0": map[string]any{"if": "starts_with(path, '/jmap/upload/')", "then": "403"}}, "else": "200"},
		"usePermissiveCors": false,
	}
	if profile.version == "1.0.2" {
		source := "peer"
		if v.UseForwardedClientIP {
			source = "xForwardedFor"
		}
		value["clientIpSource"], value["forwardedHops"] = source, 1
	} else {
		value["useXForwarded"] = v.UseForwardedClientIP
	}
	return value
}

// inventoryRequestFor requests fields of the selected native API, never both.
func inventoryRequestFor(version string) ([]byte, error) {
	profile, err := schemaFor(version)
	if err != nil {
		return nil, err
	}
	request := bytes.Clone(inventoryRequest)
	if profile.version == "1.0.2" {
		request = bytes.ReplaceAll(request, []byte(`"tlsImplicit"`), []byte(`"implicitTls"`))
		request = bytes.ReplaceAll(request, []byte(`"enabled"`), []byte(`"enable"`))
	}
	return request, nil
}

// mailboxPermissionsFor applies the audited normal-user permission delta only.
func mailboxPermissionsFor(profile runtimeSchema) map[string]bool {
	permissions := normalMailboxPermissions()
	if profile.version == "1.0.2" {
		for _, name := range []string{"jmapPrincipalCreate", "jmapPrincipalDestroy", "jmapPrincipalUpdate", "sysArchivedItemCreate", "sysSpamTrainingSampleUpdate"} {
			delete(permissions, name)
		}
		for _, name := range []string{"imapMetadataGet", "imapMetadataPrivate", "imapMetadataSet", "jmapMetadataGet", "jmapMetadataPrivate", "jmapMetadataSet", "sysOAuthGrantDestroy", "sysOAuthGrantGet", "sysOAuthGrantQuery"} {
			permissions[name] = true
		}
	}
	return permissions
}
