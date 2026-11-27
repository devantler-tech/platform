package main

type cnpgSource struct{ Namespace, Name string }

const (
	previewCNPGNamespace = "business-site-dev"
	previewCNPGName      = "portal-preview-db"
)

// cnpgSources enumerates the databases that the read-only workflow can inspect.
func cnpgSources() [8]cnpgSource {
	return [8]cnpgSource{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"},
		{"ate-system", "substrate-db"}, {"kagent", "kagent-db"}, {"customer-identity", "customer-identity-db"},
		{"business-site", "business-site-portal-db"}, {previewCNPGNamespace, previewCNPGName}}
}

// cnpgArchiveSources contains only sources with a reviewed Barman archive.
func cnpgArchiveSources() [5]cnpgSource {
	return [5]cnpgSource{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"},
		{"customer-identity", "customer-identity-db"}, {"business-site", "business-site-portal-db"}}
}

func knownCNPGArchiveSource(namespace, name string) bool {
	for _, source := range cnpgArchiveSources() {
		if namespace == source.Namespace && name == source.Name {
			return true
		}
	}
	return false
}
