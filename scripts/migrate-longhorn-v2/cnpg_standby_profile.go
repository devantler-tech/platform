package main

// Each transition has an independently reviewed backup source and a fixed
// physical storage template. A matching name outside its namespace is foreign.
type cnpgStandbyProfile struct {
	Source    cnpgSource
	Instances int
	Size      string
	SizeBytes string
}

func cnpgStandbyTransition(cluster object) (cnpgStandbyProfile, bool) {
	source := cnpgSource{Namespace: text(cluster, "metadata", "namespace"), Name: text(cluster, "metadata", "name")}
	profile := cnpgStandbyProfile{Source: source, Instances: 4}
	switch source {
	case cnpgSource{Namespace: "umami", Name: "umami-db"}:
		profile.Size, profile.SizeBytes = "5Gi", "5368709120"
	case cnpgSource{Namespace: "wedding-app", Name: "wedding-db"}:
		profile.Size, profile.SizeBytes = "2Gi", "2147483648"
	default:
		return cnpgStandbyProfile{}, false
	}
	if value(cluster, "spec", "instances") != float64(profile.Instances) ||
		text(cluster, "spec", "storage", "storageClass") != "longhorn-v2" ||
		text(cluster, "spec", "storage", "size") != profile.Size ||
		value(cluster, "spec", "storage", "resizeInUseVolumes") != false ||
		value(cluster, "spec", "walStorage") != nil || value(cluster, "spec", "tablespaces") != nil {
		return cnpgStandbyProfile{}, false
	}
	return profile, true
}
