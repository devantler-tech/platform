package main

import "github.com/devantler-tech/platform/internal/strictjson"

// Validate each bounded native document before typed decoding can overwrite
// duplicate members or accept case aliases. Unrelated native fields retain
// their spelling and values; only fields used by this proof have canonical names.
func requireHealthJSON(data []byte) error {
	return strictjson.Validate(data, map[string][]string{"": {"initialized", "sealed", "version", "cluster_id"}})
}
