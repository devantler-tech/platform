package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// privateInput reads a bounded input without including its contents in errors.
func privateInput(dir, name string, limit int64) (data []byte, result error) {
	// Projected Kubernetes Secrets use symlinks; require the resolved target to
	// remain under the private input directory rather than rejecting projections.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, errConfiguration
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	if err != nil {
		return nil, errConfiguration
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || len(rel) > 3 && rel[:3] == "../" {
		return nil, errConfiguration
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errConfiguration
	}
	defer func() {
		if f.Close() != nil {
			data = nil
			result = errConfiguration
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0027 != 0 {
		return nil, errConfiguration
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errConfiguration
	}
	return b, nil
}

// loadInputs validates the settings and credential files needed for configuration.
func loadInputs(dir string) (inputs, error) {
	var v inputs
	fail := func() (inputs, error) { return inputs{}, errConfiguration }
	b, err := privateInput(dir, "settings.json", 4096)
	if err != nil {
		return fail()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return fail()
	}
	seen := map[string]bool{}
	for d.More() {
		key, keyErr := d.Token()
		name, ok := key.(string)
		if keyErr != nil || !ok || seen[name] {
			return fail()
		}
		seen[name] = true
		if name == "useForwardedClientIP" || name == "useProxyClientIP" {
			value, decodeErr := d.Token()
			forwarded, valid := value.(bool)
			if decodeErr != nil || !valid {
				return fail()
			}
			if name == "useForwardedClientIP" {
				v.UseForwardedClientIP = forwarded
			} else {
				v.UseProxyClientIP = forwarded
			}
			continue
		}
		if name == "maintainerAliases" {
			var value json.RawMessage
			if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &v.MaintainerAliases) != nil || v.MaintainerAliases == nil {
				return fail()
			}
			continue
		}
		var target *string
		switch name {
		case "runtimeVersion":
			target = &v.RuntimeVersion
		case "domain":
			target = &v.Domain
		case "hostname":
			target = &v.Hostname
		case "maintainer":
			target = &v.Maintainer
		case "agent":
			target = &v.Agent
		case "administrator":
			target = &v.Administrator
		default:
			return fail()
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || string(value) == "null" || json.Unmarshal(value, target) != nil {
			return fail()
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return fail()
	}
	for _, target := range []struct {
		name  string
		value *string
	}{
		{"administrator-password", &v.administratorPassword}, {"maintainer-password", &v.maintainerPassword}, {"agent-password", &v.agentPassword},
		{"relay-username", &v.relayUsername}, {"relay-password", &v.relayPassword},
	} {
		b, err = privateInput(dir, target.name, 128)
		if err != nil {
			return fail()
		}
		*target.value = string(b)
	}
	if seen["runtimeVersion"] && v.RuntimeVersion == "" {
		return fail()
	}
	aliases, present, err := maintainerAddressAliases(dir, v.Domain, v.Maintainer)
	if err != nil {
		return fail()
	}
	if present {
		// Aliases already approved in the settings are kept; the inventory
		// adds every other address. An address in both is one alias, so
		// adopting the inventory can neither drop nor duplicate an alias.
		v.MaintainerAliases = unionAliases(v.MaintainerAliases, aliases)
	}
	if _, err = render(v); err != nil {
		return fail()
	}
	return v, nil
}

// unionAliases returns the settings' aliases in their order, followed by each
// inventory alias not already present.
func unionAliases(settings, inventory []string) []string {
	union := make([]string, 0, len(settings)+len(inventory))
	seen := make(map[string]bool, len(settings)+len(inventory))
	for _, name := range append(append([]string{}, settings...), inventory...) {
		if !seen[name] {
			seen[name] = true
			union = append(union, name)
		}
	}
	return union
}

// maintainerAddressAliases reads the maintainer's complete address inventory
// from the optional `maintainer-addresses` input: the same comma-separated
// list the mailbox history copy uses (#5481), so the maintainer keeps it in
// one place. Every other address on the domain becomes a receiving alias of
// the maintainer's mailbox, alongside any aliases the settings already
// approve. The inventory must name the maintainer's own address exactly once
// and no address twice, so a wrong, truncated or hand-merged file cannot pass
// as complete. Alias names are validated with the settings' own rules when
// rendered.
func maintainerAddressAliases(dir, domain, maintainer string) ([]string, bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "maintainer-addresses")); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	b, err := privateInput(dir, "maintainer-addresses", 4096)
	line := strings.TrimSuffix(string(b), "\n")
	if err != nil || line == "" || strings.ContainsAny(line, "\r\n\x00") {
		return nil, true, errConfiguration
	}
	aliases := []string{}
	own := 0
	listed := map[string]bool{}
	for _, item := range strings.Split(line, ",") {
		local, host, ok := strings.Cut(strings.ToLower(strings.TrimSpace(item)), "@")
		if !ok || local == "" || host != domain || strings.Contains(host, "@") || listed[local] {
			return nil, true, errConfiguration
		}
		listed[local] = true
		if local == maintainer {
			own++
			continue
		}
		aliases = append(aliases, local)
	}
	if own != 1 {
		return nil, true, errConfiguration
	}
	return aliases, true, nil
}

// command handles the Email configuration CLI without echoing private input values.
func command(args []string, log io.Writer) int {
	if len(args) > 0 && args[0] == "history" {
		return historyCommand(args[1:], log)
	}
	if len(args) > 0 && args[0] == "bootstrap" {
		return bootstrapCommand(args[1:], log)
	}
	if len(args) > 0 && args[0] == "check" {
		return checkCommand(args[1:], log)
	}
	f := flag.NewFlagSet("private-mail-config", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("input-dir", "", "private mounted configuration")
	output := f.String("output-file", "", "new private plan in memory-backed storage")
	var proxy proxyOption
	f.Var(&proxy, "proxy-client-ip", "explicit deployment transport override")
	var mailProxy mailProxySourceOption
	f.Var(&mailProxy, "mail-proxy-source", "private IPv4 address of the mail load balancer")
	fail := func() int { _, _ = fmt.Fprintln(log, errConfiguration); return 2 }
	if f.Parse(args) != nil || f.NArg() != 0 || !filepath.IsAbs(*dir) || !filepath.IsAbs(*output) {
		return fail()
	}
	v, err := loadInputs(*dir)
	if err != nil {
		return fail()
	}
	if proxy.value != nil {
		v.UseProxyClientIP = *proxy.value
	}
	v.mailProxySource = mailProxy.value
	inventory, err := privateInput(*dir, "listener-inventory.json", 65536)
	if err != nil || validateInventoryFor(inventory, v.RuntimeVersion) != nil || validateRecipientOwnership(inventory, v) != nil {
		return fail()
	}
	v.accountIDs, err = managedAccountIDs(inventory, v)
	if err != nil {
		return fail()
	}
	b, err := render(v)
	if err != nil {
		return fail()
	}
	parent, err := os.Stat(filepath.Dir(*output))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return fail()
	}
	plan, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail()
	}
	_, writeErr := plan.Write(b)
	closeErr := plan.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(*output)
		return fail()
	}
	if _, err = fmt.Fprintln(log, "Email plan prepared; activation is separate."); err != nil {
		return 2
	}
	return 0
}

// main preserves the configuration command's exit status.
func main() { os.Exit(command(os.Args[1:], os.Stderr)) }
