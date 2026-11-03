package main

type cnpgSource struct{ Namespace, Name string }

// cnpgSources enumerates the database owners covered by this migration.
func cnpgSources() [3]cnpgSource {
	return [3]cnpgSource{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"}}
}

// knownCNPGSource prevents an arbitrary namespace or database becoming a target.
func knownCNPGSource(namespace, name string) bool {
	for _, source := range cnpgSources() {
		if namespace == source.Namespace && name == source.Name {
			return true
		}
	}
	return false
}
