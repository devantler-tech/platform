package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schema/*.json
var schemas embed.FS

const schemaURL = "https://flagd.dev/schema/v0/flags.json"

type offlineLoader struct{}

func (offlineLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("unregistered schema %q; network loading is disabled", url)
}

func compileSchema() (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineLoader{})
	for _, name := range []string{"flags.json", "targeting.json"} {
		data, err := schemas.ReadFile("schema/" + name)
		if err != nil {
			return nil, err
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := compiler.AddResource("https://flagd.dev/schema/v0/"+name, document); err != nil {
			return nil, err
		}
	}
	return compiler.Compile(schemaURL)
}

func inspect(document map[string]any, schema *jsonschema.Schema, count *int) error {
	kind, _ := document["kind"].(string)
	api, _ := document["apiVersion"].(string)
	if kind == "List" || kind == "FeatureFlagList" {
		if kind == "FeatureFlagList" && api != "core.openfeature.dev/v1beta1" {
			return fmt.Errorf("unsupported FeatureFlagList API %q", api)
		}
		items, ok := document["items"].([]any)
		if !ok {
			return fmt.Errorf("%s must contain an items list", kind)
		}
		for _, item := range items {
			child, ok := item.(map[string]any)
			if !ok {
				return errors.New("resource list item must be an object")
			}
			if kind == "FeatureFlagList" {
				if child["kind"] == nil {
					child["kind"] = "FeatureFlag"
				}
				if child["apiVersion"] == nil {
					child["apiVersion"] = api
				}
				if child["kind"] != "FeatureFlag" {
					return errors.New("FeatureFlagList contains a different resource kind")
				}
			}
			if err := inspect(child, schema, count); err != nil {
				return err
			}
		}
		return nil
	}
	if kind != "FeatureFlag" {
		return nil
	}
	if api != "core.openfeature.dev/v1beta1" {
		return fmt.Errorf("unsupported FeatureFlag API %q", api)
	}
	spec, _ := document["spec"].(map[string]any)
	definition, ok := spec["flagSpec"].(map[string]any)
	if !ok {
		return errors.New("FeatureFlag spec.flagSpec must be an object")
	}
	data, err := json.Marshal(definition)
	if err != nil {
		return fmt.Errorf("flag definition is not JSON-compatible: %w", err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("flagd schema: %w", err)
	}
	flags, _ := definition["flags"].(map[string]any)
	keys := make([]string, 0, len(flags))
	for key := range flags {
		keys = append(keys, key)
	}
	// Stable diagnostics when several flags have invalid defaults.
	sort.Strings(keys)
	for _, key := range keys {
		flag, _ := flags[key].(map[string]any)
		variant, ok := flag["defaultVariant"].(string)
		if !ok {
			return fmt.Errorf("flag %q requires a string defaultVariant in the operator CR", key)
		}
		variants, _ := flag["variants"].(map[string]any)
		if _, exists := variants[variant]; !exists {
			return fmt.Errorf("flag %q defaultVariant does not name a variant", key)
		}
	}
	(*count)++
	return nil
}

func validate(root string, output io.Writer) error {
	schema, err := compileSchema()
	if err != nil {
		return err
	}
	count := 0
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing YAML symlink: %s", path)
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var node yaml.Node
			err := decoder.Decode(&node)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			// Standalone JSON6902 patch arrays and scalar overlay inputs are not CRs.
			if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
				continue
			}
			var document map[string]any
			if err := node.Decode(&document); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := inspect(document, schema, &count); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
	})
	if err != nil {
		return err
	}
	if count == 0 {
		_, err = fmt.Fprintln(output, "No FeatureFlag resources found; flag coverage is empty.")
	} else {
		_, err = fmt.Fprintf(output, "Validated %d FeatureFlag resources against flagd schema 0.2.15.\n", count)
	}
	return err
}

func main() {
	root := "k8s"
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: validate-feature-flags [ROOT]")
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		root = os.Args[1]
	}
	if strings.TrimSpace(root) == "" {
		fmt.Fprintln(os.Stderr, "ROOT must not be empty")
		os.Exit(2)
	}
	if err := validate(root, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
