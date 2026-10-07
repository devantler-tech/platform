package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Validate each bounded native document before typed decoding can overwrite
// duplicate members or accept case aliases. Unrelated native fields retain
// their spelling and values; only fields used by this proof have canonical names.
func requireTransportJSON(data []byte, health bool) error {
	fields := map[string][]string{
		"":                  {"flow", "lost_events", "lostEvents", "node_status", "nodeStatus"},
		"flow":              {"verdict", "drop_reason_desc", "traffic_direction", "source", "destination", "IP", "l4", "node_name", "time"},
		"flow.source":       {"namespace", "pod_name"},
		"flow.destination":  {"namespace", "pod_name"},
		"flow.IP":           {"source", "destination"},
		"flow.l4":           {"TCP"},
		"flow.l4.TCP":       {"destination_port", "flags"},
		"flow.l4.TCP.flags": {"SYN"},
	}
	if health {
		fields = map[string][]string{"": {"initialized", "sealed", "version", "cluster_id"}}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(string, int) error
	walk = func(path string, depth int) error {
		if depth > 64 {
			return errors.New("native evidence nesting exceeds bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("incomplete native evidence")
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return errors.New("ambiguous native evidence fields")
				}
				seen[key] = true
				for _, canonical := range fields[path] {
					if key != canonical && strings.EqualFold(key, canonical) {
						return errors.New("case-aliased native evidence field")
					}
				}
				child := key
				if path != "" {
					child = path + "." + key
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(path+"[]", depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected native evidence delimiter")
		}
		end, err := decoder.Token()
		if err != nil || (delimiter == '{' && end != json.Delim('}')) || (delimiter == '[' && end != json.Delim(']')) {
			return errors.New("incomplete native evidence")
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing native evidence")
	}
	return nil
}
