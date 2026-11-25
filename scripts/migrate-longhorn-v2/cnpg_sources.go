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
