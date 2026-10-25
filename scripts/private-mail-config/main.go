package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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
		var target *string
		switch name {
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
	} {
		b, err = privateInput(dir, target.name, 128)
		if err != nil {
			return fail()
		}
		*target.value = string(b)
	}
	if _, err = render(v); err != nil {
		return fail()
	}
	return v, nil
}

func command(args []string, log io.Writer) int {
	if len(args) > 0 && args[0] == "bootstrap" {
		return bootstrapCommand(args[1:], log)
	}
	f := flag.NewFlagSet("private-mail-config", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("input-dir", "", "private mounted configuration")
	output := f.String("output-file", "", "new private plan in memory-backed storage")
	fail := func() int { _, _ = fmt.Fprintln(log, errConfiguration); return 2 }
	if f.Parse(args) != nil || f.NArg() != 0 || !filepath.IsAbs(*dir) || !filepath.IsAbs(*output) {
		return fail()
	}
	v, err := loadInputs(*dir)
	if err != nil {
		return fail()
	}
	b, err := render(v)
	if err != nil {
		return fail()
	}
	inventory, err := privateInput(*dir, "listener-inventory.json", 65536)
	if err != nil || validateInventory(inventory) != nil {
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
	if _, err = fmt.Fprintln(log, "Private mail plan prepared; activation is separate."); err != nil {
		return 2
	}
	return 0
}

func main() { os.Exit(command(os.Args[1:], os.Stderr)) }
