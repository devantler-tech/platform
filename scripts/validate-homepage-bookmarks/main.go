// Command validate-homepage-bookmarks checks embedded YAML and independently
// discovers service groups. It needs no external YAML or JSON executables.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var iconPattern = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*(-#[0-9A-Fa-f]{6})?$`)
var hrefPattern = regexp.MustCompile(`^https://\S+$`)
var groupPattern = regexp.MustCompile(`(?m)^[\t ]*gethomepage\.dev/group:[\t ]*(.*)$`)

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }

func run(args []string, out io.Writer) (status int) {
	buffer := bufio.NewWriter(out)
	out = buffer
	defer func() {
		if buffer.Flush() != nil {
			status = 2
		}
	}()
	if len(args) != 2 {
		fmt.Fprintln(out, "::error::usage: validate-homepage-bookmarks <config-map.yaml> <k8s-root>")
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(out, "::error::missing or unreadable: %s\n", args[0])
		return 2
	}
	info, err := os.Stat(args[1])
	if err != nil || !info.IsDir() {
		fmt.Fprintf(out, "::error::missing k8s root: %s\n", args[1])
		return 2
	}
	var manifest map[string]any
	if err := decode(data, &manifest); err != nil {
		fmt.Fprintf(out, "::error::cannot parse %s: %s\n", args[0], err)
		return 2
	}
	values, ok := manifest["data"].(map[string]any)
	if manifest == nil || !ok {
		fmt.Fprintln(out, "::error::manifest and data must be YAML mappings")
		return 2
	}
	parsed := make(map[string]any)
	for _, key := range []string{"bookmarks.yaml", "settings.yaml", "services.yaml"} {
		body, ok := values[key].(string)
		if !ok || strings.TrimSpace(body) == "" {
			fmt.Fprintf(out, "::error::%s missing from %s\n", key, args[0])
			return 1
		}
		var value any
		if err := decode([]byte(body), &value); err != nil {
			fmt.Fprintf(out, "::error::cannot parse %s: %s\n", key, err)
			return 2
		}
		parsed[key] = value
	}
	services := make(map[string]bool)
	serviceGroups, ok := parsed["services.yaml"].([]any)
	if !ok {
		fmt.Fprintln(out, "::error::unsupported service group structure")
		return 2
	}
	for _, group := range serviceGroups {
		object, ok := group.(map[string]any)
		if !ok || len(object) == 0 {
			fmt.Fprintln(out, "::error::unsupported service group structure")
			return 2
		}
		for name, value := range object {
			if _, ok := value.([]any); !ok || strings.TrimSpace(name) == "" {
				fmt.Fprintln(out, "::error::unsupported service group structure")
				return 2
			}
			services[name] = true
		}
	}
	// Failed observations must not silently erase discovered service groups.
	err = filepath.WalkDir(args[1], func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported service-group input type: %s", path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read service-group input %s: %w", path, err)
		}
		for _, match := range groupPattern.FindAllSubmatch(body, -1) {
			var annotation map[string]any
			if decode([]byte("gethomepage.dev/group: "+string(match[1])), &annotation) != nil {
				return fmt.Errorf("unsupported service annotation: %s", path)
			}
			name, ok := annotation["gethomepage.dev/group"].(string)
			if !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("unsupported service annotation: %s", path)
			}
			services[name] = true
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(out, "::error::cannot read service groups: %s\n", err)
		return 2
	}
	layout := make(map[string]bool)
	if settings, ok := parsed["settings.yaml"].(map[string]any); ok {
		if groups, ok := settings["layout"].(map[string]any); ok {
			for group := range groups {
				layout[group] = true
			}
		}
	}
	problems, entries, groups := validate(parsed["bookmarks.yaml"], services, layout)
	if len(problems) > 0 {
		sort.Strings(problems)
		fmt.Fprintf(out, "::error::%d homepage bookmark violation(s):\n", len(problems))
		for _, problem := range problems {
			fmt.Fprintf(out, "  %s\n", problem)
		}
		return 1
	}
	fmt.Fprintf(out, "✓ %d homepage bookmark(s) in %d group(s) valid.\n", entries, groups)
	return 0
}

func decode(data []byte, target any) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	// YAML-to-JSON readers preserve scalar mapping keys as their original text.
	// Normalize keys before decoding so numeric, boolean and date-like names do
	// not turn otherwise supported mappings into map[any]any.
	if err := stringMappingKeys(&document, make(map[*yaml.Node]bool)); err != nil {
		return err
	}
	if err := document.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("expected exactly one YAML document")
	}
	return nil
}

func stringMappingKeys(node *yaml.Node, seen map[*yaml.Node]bool) error {
	if node == nil || seen[node] {
		return nil
	}
	seen[node] = true
	if node.Kind == yaml.MappingNode {
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind == yaml.AliasNode && key.Alias != nil && key.Alias.Kind == yaml.ScalarNode {
				// Copy the key rather than changing the anchored value's type.
				*key = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key.Alias.Value, Line: key.Line, Column: key.Column}
			}
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("mapping keys must be scalar values")
			}
			if key.Tag != "!!merge" {
				key.Tag = "!!str"
			}
		}
	}
	for _, child := range node.Content {
		if err := stringMappingKeys(child, seen); err != nil {
			return err
		}
	}
	return stringMappingKeys(node.Alias, seen)
}
func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validate(value any, services, layout map[string]bool) ([]string, int, int) {
	var problems []string
	groups := make(map[string]bool)
	entries := 0
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		problems = append(problems, "no bookmark groups parsed — bookmarks.yaml is empty or not a list")
	}
	for index, item := range list {
		group, ok := item.(map[string]any)
		if !ok || len(group) != 1 {
			problems = append(problems, fmt.Sprintf("bookmark group item %d must map exactly one group name", index+1))
			continue
		}
		name := sortedKeys(group)[0]
		if groups[name] {
			problems = append(problems, name+": duplicate bookmark group")
			continue
		}
		groups[name] = true
		bookmarks, ok := group[name].([]any)
		if !ok || len(bookmarks) == 0 {
			problems = append(problems, name+": group has no bookmarks")
			continue
		}
		validItems := true
		for _, item := range bookmarks {
			object, ok := item.(map[string]any)
			if !ok || len(object) != 1 {
				validItems = false
			}
		}
		if !validItems {
			problems = append(problems, name+": every bookmark item must map exactly one bookmark name")
			continue
		}
		counts := make(map[string]int)
		for _, item := range bookmarks {
			bookmark := item.(map[string]any)
			key := sortedKeys(bookmark)[0]
			counts[key]++
			entries++
			fields := make(map[string]any)
			if rows, ok := bookmark[key].([]any); ok {
				validFields := true
				for _, row := range rows {
					if _, ok := row.(map[string]any); !ok {
						validFields = false
					}
				}
				if validFields {
					for _, row := range rows {
						for field, value := range row.(map[string]any) {
							fields[field] = value
						}
					}
				}
			}
			where := name + " -> " + key
			icon, href := "", ""
			if fields["icon"] != nil && fields["icon"] != false {
				icon = fmt.Sprint(fields["icon"])
			}
			if fields["href"] != nil && fields["href"] != false {
				href = fmt.Sprint(fields["href"])
			}
			if icon == "" {
				problems = append(problems, where+": missing icon")
			} else if !iconPattern.MatchString(icon) {
				problems = append(problems, fmt.Sprintf("%s: icon \"%s\" does not match <slug>[-#RRGGBB]", where, icon))
			}
			if href == "" {
				problems = append(problems, where+": missing href")
			} else if !hrefPattern.MatchString(href) {
				problems = append(problems, fmt.Sprintf("%s: href \"%s\" must be an https:// URL", where, href))
			}
		}
		for key, count := range counts {
			if count > 1 {
				problems = append(problems, name+" -> "+key+": duplicate bookmark name in this group")
			}
		}
	}
	if len(services) == 0 {
		problems = append(problems, "no service groups found — check 4 would pass vacuously")
	}
	if len(layout) == 0 {
		problems = append(problems, "settings.yaml has no layout groups — check 5 would pass vacuously")
	}
	for group := range groups {
		if services[group] {
			problems = append(problems, group+": bookmark group reuses a service group name")
		}
		if !layout[group] {
			problems = append(problems, group+": bookmark group is not listed in settings.yaml layout")
		}
	}
	return problems, entries, len(groups)
}
