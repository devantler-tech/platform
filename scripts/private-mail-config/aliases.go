package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

func receivingAliases(names []string, domain string) map[string]any {
	aliases := make(map[string]any, len(names))
	for i, name := range names {
		aliases[strconv.Itoa(i)] = map[string]any{"name": name, "domainId": domain, "enabled": true, "description": nil}
	}
	return aliases
}

type receivingAlias struct {
	Name, Domain string
	Enabled      bool
	Description  any
}

// Native List<EmailAlias> is a numeric-keyed object. Its ordering is not an
// address identity, but every entry and native field must be accounted for.
func canonicalReceivingAliases(raw any) ([]receivingAlias, error) {
	entries, ok := raw.(map[string]any)
	if !ok || len(entries) > 16 {
		return nil, errConfiguration
	}
	aliases := make([]receivingAlias, 0, len(entries))
	seen := map[string]bool{}
	for i := 0; i < len(entries); i++ {
		entry, ok := entries[strconv.Itoa(i)].(map[string]any)
		if !ok || len(entry) != 4 {
			return nil, errConfiguration
		}
		name, nok := entry["name"].(string)
		domain, dok := entry["domainId"].(string)
		enabled, eok := entry["enabled"].(bool)
		description, present := entry["description"]
		if !nok || !localPattern.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".") || !dok || domain == "" || !eok || !present {
			return nil, errConfiguration
		}
		if description != nil {
			if _, ok := description.(string); !ok {
				return nil, errConfiguration
			}
		}
		key := domain + "\x00" + name
		if seen[key] {
			return nil, errConfiguration
		}
		seen[key] = true
		aliases = append(aliases, receivingAlias{Name: name, Domain: domain, Enabled: enabled, Description: description})
	}
	sort.Slice(aliases, func(i, j int) bool {
		if aliases[i].Domain == aliases[j].Domain {
			return aliases[i].Name < aliases[j].Name
		}
		return aliases[i].Domain < aliases[j].Domain
	})
	return aliases, nil
}

func receivingAliasesMatch(want, actual any) bool {
	// Domain aliases are a string-set, whereas Account aliases are EmailAlias
	// objects. Neither collection may silently accept additional identities.
	if entries, ok := want.(map[string]any); ok {
		stringSet := true
		for _, entry := range entries {
			if _, ok := entry.(bool); !ok {
				stringSet = false
				break
			}
		}
		if stringSet {
			return reflect.DeepEqual(want, actual)
		}
	}
	w, err := canonicalReceivingAliases(want)
	if err != nil {
		return false
	}
	a, err := canonicalReceivingAliases(actual)
	return err == nil && reflect.DeepEqual(w, a)
}

func receivingObjects(methods [][]json.RawMessage, object, tag string) ([]map[string]any, error) {
	if len(methods) != 2 {
		return nil, errConfiguration
	}
	for i, verb := range []string{"query", "get"} {
		var name, callTag string
		if len(methods[i]) != 3 || json.Unmarshal(methods[i][0], &name) != nil || name != "x:"+object+"/"+verb || json.Unmarshal(methods[i][2], &callTag) != nil || callTag != tag+"-"+verb {
			return nil, errConfiguration
		}
	}
	var query struct {
		IDs      []string `json:"ids"`
		Total    *int     `json:"total"`
		Position *int     `json:"position"`
	}
	var get struct {
		List    []map[string]any `json:"list"`
		Missing []string         `json:"notFound"`
	}
	if json.Unmarshal(methods[0][1], &query) != nil || json.Unmarshal(methods[1][1], &get) != nil || query.IDs == nil || query.Total == nil || query.Position == nil || *query.Position != 0 || *query.Total != len(query.IDs) || len(query.IDs) > 16 || get.List == nil || get.Missing == nil || len(get.Missing) != 0 || len(get.List) != len(query.IDs) {
		return nil, errConfiguration
	}
	ids := map[string]bool{}
	for _, id := range query.IDs {
		if id == "" || ids[id] {
			return nil, errConfiguration
		}
		ids[id] = true
	}
	for _, entry := range get.List {
		id, ok := entry["id"].(string)
		if !ok || !ids[id] {
			return nil, errConfiguration
		}
		delete(ids, id)
	}
	if len(ids) != 0 {
		return nil, errConfiguration
	}
	return get.List, nil
}

// This is called on the fresh census from the exclusively owned recovery
// child, before the first native apply. A standalone plan is only a snapshot;
// the native CLI offers no plan-level revision/ifInState precondition.
func validateRecipientOwnership(raw []byte, v inputs) error {
	if v.MaintainerAliases == nil {
		return nil
	}
	var response struct {
		Methods [][]json.RawMessage `json:"methodResponses"`
	}
	if len(raw) > 65536 || json.Unmarshal(raw, &response) != nil || len(response.Methods) != 13 {
		return errConfiguration
	}
	var domains struct {
		List []struct {
			ID, Name string
			Aliases  map[string]bool
		} `json:"list"`
	}
	if json.Unmarshal(response.Methods[6][1], &domains) != nil {
		return errConfiguration
	}
	domainIDs := map[string]bool{}
	target := ""
	for _, domain := range domains.List {
		domainIDs[domain.ID] = true
		if strings.EqualFold(domain.Name, v.Domain) {
			if target != "" || domain.Name != v.Domain {
				return errConfiguration
			}
			target = domain.ID
		} else {
			for alias := range domain.Aliases {
				if strings.EqualFold(alias, v.Domain) {
					return errConfiguration
				}
			}
		}
	}
	wanted := map[string]bool{v.Maintainer: true}
	for _, name := range v.MaintainerAliases {
		wanted[name] = true
	}
	maintainerSeen := false
	for i, object := range []string{"Account", "MailingList"} {
		objects, err := receivingObjects(response.Methods[9+i*2:11+i*2], object, []string{"accounts", "mailing-lists"}[i])
		if err != nil {
			return errConfiguration
		}
		for _, owner := range objects {
			name, ok := owner["name"].(string)
			if !ok || name == "" {
				return errConfiguration
			}
			domainRaw, present := owner["domainId"]
			if !present {
				return errConfiguration
			}
			domain := ""
			if domainRaw != nil {
				var ok bool
				domain, ok = domainRaw.(string)
				if !ok || !domainIDs[domain] {
					return errConfiguration
				}
			}
			retained := target != "" && domain == target && strings.EqualFold(name, v.Maintainer)
			if retained && (object != "Account" || name != v.Maintainer || owner["@type"] != "User" || maintainerSeen) {
				return errConfiguration
			}
			if target != "" && domain == target && wanted[strings.ToLower(name)] && !retained {
				return errConfiguration
			}
			aliases, err := canonicalReceivingAliases(owner["aliases"])
			if err != nil {
				return errConfiguration
			}
			if retained {
				maintainerSeen = true
				if len(aliases) > 0 && !receivingAliasesMatch(receivingAliases(v.MaintainerAliases, target), owner["aliases"]) {
					return errConfiguration
				}
			} else {
				for _, alias := range aliases {
					if !domainIDs[alias.Domain] || target != "" && alias.Domain == target && wanted[alias.Name] {
						return errConfiguration
					}
				}
			}
		}
	}
	return nil
}
