package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout)) }

func run(args []string, input io.Reader, out io.Writer) int {
	// Never echo input, field keys, parser excerpts, or filesystem error text.
	report := func(message string, status int) int {
		if _, err := fmt.Fprintln(out, message); err != nil {
			return 2
		}
		return status
	}
	flags := flag.NewFlagSet("validate-break-glass", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	field := flags.String("field", "", "named field to extract")
	output := flags.String("output", "", "new private file")
	if flags.Parse(args) != nil || flags.NArg() != 0 || ((*field == "") != (*output == "")) ||
		(*field != "" && *field != "kubeconfig" && *field != "talosconfig") {
		return report("usage: validate-break-glass [--field kubeconfig|talosconfig --output NEW_FILE] < KV_V2_JSON", 2)
	}
	const maximum = 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(input, maximum+1))
	if err != nil || len(data) > maximum {
		return report("cannot completely read bounded recovery export", 2)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := jsonValue(decoder, 0)
	_, trailing := decoder.Token()
	if !utf8.Valid(data) || err != nil || !errors.Is(trailing, io.EOF) {
		return report("invalid or ambiguous recovery export", 1)
	}
	outer, _ := value.(map[string]any)
	dataObject, _ := outer["data"].(map[string]any)
	fields, _ := dataObject["data"].(map[string]any)
	kube, kubeOK := fields["kubeconfig"].(string)
	talos, talosOK := fields["talosconfig"].(string)
	if len(fields) != 2 || !kubeOK || !talosOK || !configShape(kube, "kubeconfig") || !configShape(talos, "talosconfig") {
		return report("invalid named recovery fields; operator repair required", 1)
	}
	if *output == "" {
		return report("named recovery fields valid", 0)
	}
	parent, err := os.Lstat(filepath.Dir(*output))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return report("output requires an existing private directory", 2)
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return report("cannot create new recovery file; existing paths are refused", 2)
	}
	selected := kube
	if *field == "talosconfig" {
		selected = talos
	}
	err = file.Chmod(0600)
	if err == nil {
		_, err = io.WriteString(file, selected)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		// This path was exclusively created by this invocation in a private directory.
		if os.Remove(*output) != nil {
			return report("recovery write failed; private partial output needs operator cleanup", 2)
		}
		return report("recovery write failed; partial output removed", 2)
	}
	return report("private recovery file created", 0)
}

// Walk tokens instead of unmarshalling into a map: duplicate keys otherwise
// silently replace earlier values. Depth and input size are bounded.
func jsonValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("key")
			}
			if _, exists := object[name]; exists {
				return nil, errors.New("duplicate")
			}
			value, err := jsonValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("object")
		}
		return object, nil
	case '[':
		var array []any
		for decoder.More() {
			value, err := jsonValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("array")
		}
		return array, nil
	default:
		return nil, errors.New("delimiter")
	}
}

func configShape(body, kind string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	decoder := yaml.NewDecoder(strings.NewReader(body))
	var config map[string]any
	if decoder.Decode(&config) != nil || config == nil {
		return false
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return false
	}
	if kind == "kubeconfig" {
		context, ok := config["current-context"].(string)
		if !ok || context == "" || config["apiVersion"] != "v1" || config["kind"] != "Config" {
			return false
		}
		clusters, clustersOK := namedEntries(config["clusters"], "cluster")
		users, usersOK := namedEntries(config["users"], "user")
		contexts, contextsOK := namedEntries(config["contexts"], "context")
		selected, selectedOK := contexts[context]
		cluster, clusterOK := selected["cluster"].(string)
		user, userOK := selected["user"].(string)
		_, clusterExists := clusters[cluster]
		_, userExists := users[user]
		return clustersOK && usersOK && contextsOK && selectedOK && clusterOK && userOK && clusterExists && userExists
	}
	context, ok := config["context"].(string)
	contexts, mapOK := config["contexts"].(map[string]any)
	selected, selectedOK := contexts[context].(map[string]any)
	if !ok || !mapOK || !selectedOK || context == "" {
		return false
	}
	for _, key := range []string{"ca", "crt", "key"} {
		value, ok := selected[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return false
		}
	}
	endpoints, ok := selected["endpoints"].([]any)
	if !ok || len(endpoints) == 0 {
		return false
	}
	for _, endpoint := range endpoints {
		value, ok := endpoint.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func namedEntries(value any, field string) (map[string]map[string]any, bool) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	entries := make(map[string]map[string]any)
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		name, nameOK := item["name"].(string)
		body, bodyOK := item[field].(map[string]any)
		if !ok || !nameOK || name == "" || !bodyOK || len(body) == 0 {
			return nil, false
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, false
		}
		entries[name] = body
	}
	return entries, true
}
