package main

import (
	"encoding/json"
	"strings"
)

// managedAccountNames lists every account local part the plan owns. The
// voucher-collection mailbox is owned only once its private setting is chosen.
func managedAccountNames(v settings) []string {
	names := []string{v.Maintainer, v.Agent, v.Administrator}
	if v.Bookkeeping != "" {
		names = append(names, v.Bookkeeping)
	}
	return names
}

// managedAccountIDs binds password updates to the complete non-secret census.
// A missing account is created explicitly; an upsert must never race into a replacement.
func managedAccountIDs(raw []byte, v inputs) (map[string]string, error) {
	profile, err := schemaFor(v.RuntimeVersion)
	if err != nil {
		return nil, err
	}
	var response struct {
		Methods [][]json.RawMessage `json:"methodResponses"`
	}
	if len(raw) > 65536 || json.Unmarshal(raw, &response) != nil || len(response.Methods) != 13 || validateDomains(response.Methods[5:7]) != nil {
		return nil, errConfiguration
	}
	var domains struct {
		List []struct {
			ID, Name string
			Aliases  map[string]bool
		} `json:"list"`
	}
	if json.Unmarshal(response.Methods[6][1], &domains) != nil {
		return nil, errConfiguration
	}
	knownDomains := map[string]bool{}
	target := ""
	for _, domain := range domains.List {
		knownDomains[domain.ID] = true
		if strings.EqualFold(domain.Name, v.Domain) {
			if target != "" || domain.Name != v.Domain {
				return nil, errConfiguration
			}
			target = domain.ID
		} else {
			for alias := range domain.Aliases {
				if strings.EqualFold(alias, v.Domain) {
					return nil, errConfiguration
				}
			}
		}
	}
	accounts, err := receivingObjects(response.Methods[9:11], "Account", "accounts")
	if err != nil {
		return nil, errConfiguration
	}
	wanted := map[string]bool{}
	for _, name := range managedAccountNames(v.settings) {
		wanted[name] = true
	}
	ids := map[string]string{}
	for _, account := range accounts {
		name, nameOK := account["name"].(string)
		id, idOK := account["id"].(string)
		domainValue, domainPresent := account["domainId"]
		domain, domainOK := domainValue.(string)
		if !nameOK || name == "" || !idOK || id == "" || len(id) > 128 || strings.HasPrefix(id, "#") || !domainPresent || domainValue != nil && (!domainOK || !knownDomains[domain]) {
			return nil, errConfiguration
		}
		managed := wanted[strings.ToLower(name)]
		if managed {
			if !wanted[name] || target == "" || domain != target || account["@type"] != "User" || ids[name] != "" {
				return nil, errConfiguration
			}
			ids[name] = id
		}
		aliases, err := canonicalReceivingAliasesFor(account["aliases"], profile.aliasEnabled)
		if err != nil {
			return nil, errConfiguration
		}
		for _, alias := range aliases {
			if !knownDomains[alias.Domain] || alias.Domain == target && wanted[alias.Name] && (!managed || alias.Name != name) {
				return nil, errConfiguration
			}
		}
	}
	return ids, nil
}
