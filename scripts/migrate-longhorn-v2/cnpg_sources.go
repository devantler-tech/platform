package main

type cnpgSource struct{ Namespace, Name string }

// cnpgSources enumerates the database owners covered by this migration.
func cnpgSources() [7]cnpgSource {
	return [7]cnpgSource{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"},
		{"ate-system", "substrate-db"}, {"kagent", "kagent-db"}, {"customer-identity", "customer-identity-db"},
		{"business-site", "business-site-portal-db"}}
}
