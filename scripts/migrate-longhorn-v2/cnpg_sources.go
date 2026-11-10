package main

type cnpgSource struct{ Namespace, Name string }

// cnpgSources enumerates the database owners covered by this migration.
func cnpgSources() [5]cnpgSource {
	return [5]cnpgSource{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"},
		{"ate-system", "substrate-db"}, {"kagent", "kagent-db"}}
}
