package main

import (
	"encoding/json"
	"strings"
)

// managedAccountIDs binds password updates to the complete non-secret census.
// A missing account is created explicitly; an upsert must never race into a replacement.
func managedAccountIDs(raw []byte, v inputs) (map[string]string, error) {
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
	wanted := map[string]bool{v.Administrator: true, v.Maintainer: true, v.Agent: true}
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
		aliases, err := canonicalReceivingAliases(account["aliases"])
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
