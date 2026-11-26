package main

import (
	"errors"
	"net/url"
	"reflect"
	"regexp"
)

type cnpgRecoverySource struct {
	Server, Database, BackupUID, BackupID                        string
	Namespace, Cluster, Store, Credential, Destination, Endpoint string
	Image, Size, ClusterUID, StoreUID                            string
}

type cnpgArchiveBinding struct {
	ClusterUID, StoreUID, Endpoint, Destination string
	ClusterGeneration, StoreGeneration          int64
}

var recoveryEndpoint = regexp.MustCompile(`^https://[0-9a-f]{32}\.r2\.cloudflarestorage\.com$`)
var recoveryBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var recoveryBackupID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`)
var recoveryImage = regexp.MustCompile(`^ghcr\.io/cloudnative-pg/postgresql:18\.[0-9]+-[a-z0-9-]+$`)
var recoverySize = regexp.MustCompile(`^[1-9][0-9]*(Mi|Gi|Ti)$`)

// bindCNPGArchiveSource validates the source before a fresh Backup exists.
// Its expected continuity binding is persisted before the later backup write.
func bindCNPGArchiveSource(c, store object, expected cnpgArchiveBinding) (cnpgRecoverySource, error) {
	name := text(c, "metadata", "name")
	config, ok := map[string]struct{ namespace, database, store, server, credential, originalClass string }{
		"umami-db":                {"umami", "umami", "umami-db", "umami-db", "umami-db-backup-r2", "longhorn-wffc"},
		"coroot-db":               {"observability", "coroot", "coroot-db", "coroot-db", "coroot-db-backup-r2", "longhorn-wffc"},
		"wedding-db":              {"wedding-app", "wedding", "wedding-db-dedicated", "wedding-db-20260909", "wedding-db-backup-r2-dedicated", "longhorn-wffc"},
		"customer-identity-db":    {"customer-identity", "zitadel", "customer-identity-db", "customer-identity-db", "customer-identity-db-backup-r2", "hcloud"},
		"business-site-portal-db": {"business-site", "portal", "business-site-portal-db", "business-site-portal-db", "business-site-portal-db-backup-r2", "hcloud"},
	}[name]
	profile, knownProfile := hostDatabases[config.namespace+"/"+name]
	refused := errors.New("database recovery source, archive or fresh backup binding is incomplete")
	if expected.ClusterUID == "" || expected.StoreUID == "" || expected.ClusterGeneration < 1 || expected.StoreGeneration < 1 || expected.ClusterGeneration > 2147483647 || expected.StoreGeneration > 2147483647 ||
		text(c, "metadata", "uid") != expected.ClusterUID || text(store, "metadata", "uid") != expected.StoreUID || value(c, "metadata", "generation") != float64(expected.ClusterGeneration) || value(store, "metadata", "generation") != float64(expected.StoreGeneration) ||
		text(store, "spec", "configuration", "endpointURL") != expected.Endpoint || text(store, "spec", "configuration", "destinationPath") != expected.Destination {
		return cnpgRecoverySource{}, refused
	}
	if !ok || !knownProfile || text(c, "metadata", "namespace") != config.namespace || text(c, "metadata", "uid") == "" || text(c, "metadata", "resourceVersion") == "" || value(c, "metadata", "deletionTimestamp") != nil ||
		value(c, "spec", "instances") != profile.instances || value(c, "status", "readyInstances") != profile.instances || !cnpgCondition(c, "Ready") || !cnpgCondition(c, "ContinuousArchiving") || text(c, "spec", "bootstrap", "initdb", "database") != config.database {
		return cnpgRecoverySource{}, refused
	}
	plugins, _ := value(c, "spec", "plugins").([]any)
	if len(plugins) != 1 {
		return cnpgRecoverySource{}, refused
	}
	plugin, _ := plugins[0].(object)
	parameters, _ := value(plugin, "parameters").(object)
	server := name
	if raw, present := parameters["serverName"]; present {
		var valid bool
		server, valid = raw.(string)
		if !valid || server == "" {
			return cnpgRecoverySource{}, refused
		}
	}
	if text(plugin, "name") != "barman-cloud.cloudnative-pg.io" || value(plugin, "enabled") != true || value(plugin, "isWALArchiver") != true || text(plugin, "parameters", "barmanObjectName") != config.store || server != config.server ||
		text(store, "metadata", "namespace") != config.namespace || text(store, "metadata", "name") != config.store || text(store, "metadata", "uid") == "" || text(store, "metadata", "resourceVersion") == "" || value(store, "metadata", "deletionTimestamp") != nil {
		return cnpgRecoverySource{}, refused
	}
	credentials := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		credentials[field] = object{"name": config.credential, "key": key}
	}
	destination := text(store, "spec", "configuration", "destinationPath")
	parsed, err := url.Parse(destination)
	if err != nil || parsed.Scheme != "s3" || !recoveryBucket.MatchString(parsed.Host) || parsed.User != nil || parsed.Path != "/cnpg/"+name || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		!recoveryEndpoint.MatchString(text(store, "spec", "configuration", "endpointURL")) || !reflect.DeepEqual(value(store, "spec", "configuration", "s3Credentials"), credentials) {
		return cnpgRecoverySource{}, refused
	}
	if name == "wedding-db" && destination != "s3://wedding-db-backups/cnpg/wedding-db" {
		return cnpgRecoverySource{}, refused
	}
	if text(store, "spec", "configuration", "wal", "compression") != "gzip" || text(store, "spec", "configuration", "data", "compression") != "gzip" {
		return cnpgRecoverySource{}, refused
	}
	storageClass := text(c, "spec", "storage", "storageClass")
	if (storageClass != config.originalClass && storageClass != "longhorn-v2") || !recoveryImage.MatchString(text(c, "spec", "imageName")) || !recoverySize.MatchString(text(c, "spec", "storage", "size")) {
		return cnpgRecoverySource{}, refused
	}
	return cnpgRecoverySource{Server: server, Database: config.database, Namespace: config.namespace, Cluster: name,
		Store: config.store, Credential: config.credential, Destination: destination, Endpoint: text(store, "spec", "configuration", "endpointURL"), Image: text(c, "spec", "imageName"), Size: text(c, "spec", "storage", "size"), ClusterUID: text(c, "metadata", "uid"), StoreUID: text(store, "metadata", "uid")}, nil
}

// bindCNPGRecoverySource adds the recorded completed Backup to its original
// pre-backup archive binding. This does not establish physical recovery or
// production replacement clearance.
func bindCNPGRecoverySource(c, store, backup object, freshBackupUID string, expected cnpgArchiveBinding) (cnpgRecoverySource, error) {
	source, err := bindCNPGArchiveSource(c, store, expected)
	if err != nil {
		return cnpgRecoverySource{}, err
	}
	if freshBackupUID == "" || text(backup, "metadata", "uid") != freshBackupUID || text(backup, "metadata", "namespace") != source.Namespace || value(backup, "metadata", "deletionTimestamp") != nil || text(backup, "spec", "cluster", "name") != source.Cluster || text(backup, "spec", "method") != "plugin" || text(backup, "spec", "pluginConfiguration", "name") != "barman-cloud.cloudnative-pg.io" || text(backup, "status", "phase") != "completed" || text(backup, "status", "pluginMetadata", "clusterUID") != source.ClusterUID || text(backup, "status", "pluginMetadata", "name") != "barman-cloud.cloudnative-pg.io" || !recoveryBackupID.MatchString(text(backup, "status", "backupId")) {
		return cnpgRecoverySource{}, errors.New("fresh native backup differs from the recorded source binding")
	}
	source.BackupUID = freshBackupUID
	source.BackupID = text(backup, "status", "backupId")
	return source, nil
}
