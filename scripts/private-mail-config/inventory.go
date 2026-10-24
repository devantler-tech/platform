package main

import (
	"encoding/json"
	"reflect"
)

type listener struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Protocol      string          `json:"protocol"`
	Bind          map[string]bool `json:"bind"`
	UseTLS        *bool           `json:"useTls"`
	ImplicitTLS   *bool           `json:"tlsImplicit"`
	ProxyNetworks map[string]bool `json:"overrideProxyTrustedNetworks"`
	DisabledTLS   map[string]bool `json:"tlsDisableProtocols"`
}

// The preparation process must make this bounded authenticated query/get in
// the same exclusive recovery session. A CLI's empty NDJSON is insufficient:
// it omits query totals, missing IDs and coverage of the get response.
func validateInventory(raw []byte) error {
	var response struct {
		Methods [][]json.RawMessage `json:"methodResponses"`
	}
	if len(raw) > 65536 || json.Unmarshal(raw, &response) != nil || len(response.Methods) != 5 {
		return errConfiguration
	}
	for offset, object := range []string{"MtaHook", "MtaMilter", "WebHook"} {
		method := response.Methods[offset+2]
		if len(method) != 3 {
			return errConfiguration
		}
		var name, tag string
		var query struct {
			IDs      []string `json:"ids"`
			Total    *int     `json:"total"`
			Position *int     `json:"position"`
		}
		if json.Unmarshal(method[0], &name) != nil || name != "x:"+object+"/query" || json.Unmarshal(method[2], &tag) != nil || tag != object || json.Unmarshal(method[1], &query) != nil || query.IDs == nil || query.Total == nil || query.Position == nil || *query.Position != 0 || *query.Total != 0 || len(query.IDs) != 0 {
			return errConfiguration
		}
	}
	var query struct {
		IDs      []string `json:"ids"`
		Total    *int     `json:"total"`
		Position *int     `json:"position"`
	}
	var get struct {
		List    []listener `json:"list"`
		Missing []string   `json:"notFound"`
	}
	for i, expected := range []string{"x:NetworkListener/query", "x:NetworkListener/get"} {
		if len(response.Methods[i]) != 3 {
			return errConfiguration
		}
		var method, tag string
		if json.Unmarshal(response.Methods[i][0], &method) != nil || method != expected || json.Unmarshal(response.Methods[i][2], &tag) != nil || tag != []string{"listeners-query", "listeners-get"}[i] {
			return errConfiguration
		}
	}
	if json.Unmarshal(response.Methods[0][1], &query) != nil || json.Unmarshal(response.Methods[1][1], &get) != nil || query.Total == nil || query.Position == nil || *query.Position != 0 || query.IDs == nil || *query.Total != len(query.IDs) || len(query.IDs) > 4 || get.List == nil || get.Missing == nil || len(get.Missing) != 0 || len(get.List) != len(query.IDs) {
		return errConfiguration
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, id := range query.IDs {
		if id == "" || ids[id] {
			return errConfiguration
		}
		ids[id] = true
	}
	for _, item := range get.List {
		if !ids[item.ID] || names[item.Name] || item.UseTLS == nil || item.ImplicitTLS == nil || item.ProxyNetworks == nil || item.DisabledTLS == nil || len(item.ProxyNetworks) != 0 || len(item.DisabledTLS) != 0 {
			return errConfiguration
		}
		delete(ids, item.ID)
		names[item.Name] = true
		protocol, bind, tls, implicit := "", "", true, false
		switch item.Name {
		case "management":
			protocol, bind, tls = "http", "0.0.0.0:8080", false
		case "smtp-inbound":
			protocol, bind = "smtp", "0.0.0.0:1025"
		case "submission":
			protocol, bind = "smtp", "0.0.0.0:1587"
		case "imaps":
			protocol, bind, implicit = "imap", "0.0.0.0:1993", true
		default:
			return errConfiguration
		}
		if item.Protocol != protocol || *item.UseTLS != tls || *item.ImplicitTLS != implicit || !reflect.DeepEqual(item.Bind, map[string]bool{bind: true}) {
			return errConfiguration
		}
	}
	if len(ids) != 0 {
		return errConfiguration
	}
	return nil
}
