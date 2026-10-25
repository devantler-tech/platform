package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

//go:embed inventory-request.json
var inventoryRequest []byte

type operation struct {
	Type   string         `json:"@type"`
	Object string         `json:"object"`
	ID     string         `json:"id"`
	Value  map[string]any `json:"value"`
}

func planOperations(plan []byte) ([]operation, error) {
	var ops []operation
	s := bufio.NewScanner(bytes.NewReader(plan))
	for s.Scan() {
		var op operation
		if json.Unmarshal(s.Bytes(), &op) != nil {
			return nil, errConfiguration
		}
		ops = append(ops, op)
	}
	if s.Err() != nil || len(ops) == 0 {
		return nil, errConfiguration
	}
	return ops, nil
}

func validateApply(plan, output []byte) error {
	ops, err := planOperations(plan)
	if err != nil {
		return err
	}
	seen := make(map[int]bool)
	summary := false
	d := json.NewDecoder(bytes.NewReader(output))
	for {
		var receipt struct {
			Index              *int `json:"index"`
			Op, Object, Status string
			Done               *struct {
				Failed *int `json:"failed"`
			} `json:"done"`
		}
		err = d.Decode(&receipt)
		if err == io.EOF {
			break
		}
		if err != nil || summary {
			return errConfiguration
		}
		if receipt.Op == "summary" {
			if receipt.Done == nil || receipt.Done.Failed == nil || *receipt.Done.Failed != 0 {
				return errConfiguration
			}
			summary = true
			continue
		}
		if receipt.Index == nil {
			return errConfiguration
		}
		i := *receipt.Index
		if i < 0 || i >= len(ops) || seen[i] || receipt.Status != "ok" || receipt.Op != ops[i].Type || receipt.Object != ops[i].Object {
			return errConfiguration
		}
		seen[i] = true
	}
	if !summary || len(seen) != len(ops) {
		return errConfiguration
	}
	return nil
}

// Structs may contain native defaults. Security maps are replacements, so an
// extra permission, socket or expression branch must never compare equal.
func policyMatches(want, actual any, key string) bool {
	switch key {
	case "match", "enabledPermissions", "disabledPermissions", "bind":
		return reflect.DeepEqual(want, actual)
	}
	if wm, ok := want.(map[string]any); ok {
		am, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range wm {
			av, found := am[k]
			if !found || !policyMatches(v, av, k) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(want, actual)
}

type management struct {
	client        *http.Client
	url, password string
}

func (m management) request(ctx context.Context, path string, body []byte) ([]byte, error) {
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, m.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, errConfiguration
	}
	req.SetBasicAuth("bootstrap", m.password)
	req.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(req)
	if err != nil {
		return nil, errConfiguration
	}
	defer func() { _ = response.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(response.Body, 262145))
	if err != nil || len(b) > 262144 || response.StatusCode != http.StatusOK {
		return nil, errConfiguration
	}
	return b, nil
}

func (m management) call(ctx context.Context, object, method string, args any) (map[string]any, error) {
	name := "x:" + object + "/" + method
	b, err := json.Marshal(map[string]any{"using": []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"}, "methodCalls": []any{[]any{name, args, "verify"}}})
	if err != nil {
		return nil, errConfiguration
	}
	b, err = m.request(ctx, "/jmap", b)
	if err != nil {
		return nil, err
	}
	var response struct {
		Methods [][]json.RawMessage `json:"methodResponses"`
	}
	var returned, tag string
	var result map[string]any
	if json.Unmarshal(b, &response) != nil || len(response.Methods) != 1 || len(response.Methods[0]) != 3 || json.Unmarshal(response.Methods[0][0], &returned) != nil || returned != name || json.Unmarshal(response.Methods[0][2], &tag) != nil || tag != "verify" || json.Unmarshal(response.Methods[0][1], &result) != nil || result == nil {
		return nil, errConfiguration
	}
	return result, nil
}

func (m management) objects(ctx context.Context, object string, properties []string, singleton bool) ([]map[string]any, error) {
	ids := []any{"singleton"}
	if !singleton {
		query, err := m.call(ctx, object, "query", map[string]any{"limit": 16, "calculateTotal": true})
		if err != nil {
			return nil, err
		}
		var ok bool
		ids, ok = query["ids"].([]any)
		if !ok || len(ids) > 16 || query["total"] != float64(len(ids)) || query["position"] != float64(0) {
			return nil, errConfiguration
		}
	}
	result, err := m.call(ctx, object, "get", map[string]any{"ids": ids, "properties": properties})
	if err != nil {
		return nil, err
	}
	list, ok := result["list"].([]any)
	missing, missingOK := result["notFound"].([]any)
	if !ok || !missingOK || len(missing) != 0 || len(list) != len(ids) {
		return nil, errConfiguration
	}
	expected := map[string]bool{}
	for _, raw := range ids {
		id, ok := raw.(string)
		if !ok || id == "" || expected[id] {
			return nil, errConfiguration
		}
		expected[id] = true
	}
	objects := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		v, ok := raw.(map[string]any)
		id, idOK := v["id"].(string)
		if !ok || !idOK || !expected[id] {
			return nil, errConfiguration
		}
		delete(expected, id)
		objects = append(objects, v)
	}
	if len(expected) != 0 {
		return nil, errConfiguration
	}
	return objects, nil
}

func resolved(v any, ids map[string]string) any {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "#") {
			return ids[strings.TrimPrefix(x, "#")]
		}
	case map[string]any:
		r := make(map[string]any, len(x))
		for k, v := range x {
			r[k] = resolved(v, ids)
		}
		return r
	}
	return v
}

func (m management) verify(ctx context.Context, plan []byte) error {
	ops, err := planOperations(plan)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, op := range ops {
		if op.Type == "upsert" {
			properties := map[string]bool{"id": true}
			for _, value := range op.Value {
				fields, ok := value.(map[string]any)
				if !ok {
					return errConfiguration
				}
				for key := range fields {
					if key != "credentials" && key != "privateKey" {
						properties[key] = true
					}
				}
			}
			keys := make([]string, 0, len(properties))
			for key := range properties {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			objects, err := m.objects(ctx, op.Object, keys, false)
			if err != nil {
				return err
			}
			for ref, raw := range op.Value {
				fields := resolved(raw, ids).(map[string]any)
				delete(fields, "credentials")
				delete(fields, "privateKey")
				var match map[string]any
				for _, actual := range objects {
					identity := fields["name"] != nil && reflect.DeepEqual(fields["name"], actual["name"]) && (fields["domainId"] == nil || fields["domainId"] == actual["domainId"])
					if op.Object == "Certificate" {
						identity = policyMatches(fields["certificate"], actual["certificate"], "")
					}
					if identity {
						if match != nil {
							return errConfiguration
						}
						match = actual
					}
				}
				if match == nil || !policyMatches(fields, match, "") {
					return errConfiguration
				}
				id, ok := match["id"].(string)
				if !ok || id == "" {
					return errConfiguration
				}
				ids[ref] = id
			}
		} else {
			properties := []string{"id"}
			for key := range op.Value {
				properties = append(properties, key)
			}
			sort.Strings(properties)
			if op.ID == "" {
				objects, err := m.objects(ctx, op.Object, properties, true)
				if err != nil || len(objects) != 1 || !policyMatches(resolved(op.Value, ids), objects[0], "") {
					return errConfiguration
				}
			} else {
				id, ok := resolved(op.ID, ids).(string)
				if !ok || id == "" {
					return errConfiguration
				}
				result, err := m.call(ctx, op.Object, "get", map[string]any{"ids": []string{id}, "properties": properties})
				if err != nil {
					return err
				}
				list, ok := result["list"].([]any)
				missing, mok := result["notFound"].([]any)
				if !ok || !mok || len(missing) != 0 || len(list) != 1 || !policyMatches(resolved(op.Value, ids), list[0], "") {
					return errConfiguration
				}
				actual, ok := list[0].(map[string]any)
				if !ok || actual["id"] != id {
					return errConfiguration
				}
			}
		}
	}
	return nil
}
