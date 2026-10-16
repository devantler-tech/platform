package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/blang/semver/v4"
)

const cnpgControllerImage = "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"
const trustControllerImage = "quay.io/jetstack/trust-manager:v0.25.0"
const rootCADescription = "Contains a CA bundle that can be used to verify the kube-apiserver when using internal endpoints such as the internal service IP or kubernetes.default.svc. No other usage is guaranteed across distributions of Kubernetes clusters."

// Profiles describe generated public material, not a general exemption for an
// operator. Everything outside the named field remains in the stable projection.
func controllerProfile(s snapshot, r map[string]any) (string, error) {
	name := text(field(r, "metadata", "name"))
	if field(r, "metadata", "namespace") != "backstage" {
		return "", nil
	}
	var db map[string]any
	for _, candidate := range array(s.live["items"]) {
		if o := object(candidate); o["kind"] == "Cluster" && field(o, "metadata", "name") == "backstage-db" {
			db = o
		}
	}
	owned := func(kind, ownerName, uid string) bool {
		owners, err := ownerReferences(r)
		return err == nil && len(owners) == 1 && owners[0]["kind"] == kind && owners[0]["name"] == ownerName && owners[0]["uid"] == uid && owners[0]["controller"] == true && uid != ""
	}
	if r["kind"] == "Secret" && (name == "backstage-db-ca" || name == "backstage-db-server" || name == "backstage-db-replication") {
		certs := object(field(db, "status", "certificates"))
		if !emptyObject(field(db, "spec", "certificates")) || !owned("Cluster", "backstage-db", text(field(db, "metadata", "uid"))) || certs["serverCASecret"] != "backstage-db-ca" || certs["clientCASecret"] != "backstage-db-ca" || certs["serverTLSSecret"] != "backstage-db-server" || certs["replicationTLSSecret"] != "backstage-db-replication" {
			return "", errors.New("default certificate ownership unverified")
		}
		// PKI constructors set managed-by and ownership, not credential
		// inheritance or operatorVersion. All existing metadata stays frozen.
		if field(r, "metadata", "labels", "app.kubernetes.io/managed-by") != "cloudnative-pg" {
			return "", errors.New("certificate producer unverified")
		}
		if name == "backstage-db-ca" {
			return "ca", nil
		}
		return "leaf", nil
	}
	if r["kind"] == "Secret" && name == "backstage-db-app" {
		initdb := object(field(db, "spec", "bootstrap", "initdb"))
		if !owned("Cluster", "backstage-db", text(field(db, "metadata", "uid"))) || r["type"] != "kubernetes.io/basic-auth" || len(initdb) == 0 || text(initdb["database"]) == "" || text(initdb["owner"]) == "" || initdb["secret"] != nil || !cnpgMetadataSettled(db, r, true) {
			return "", errors.New("default credential producer metadata is not settled")
		}
		// Existing credential values are never regenerated or normalized.
		return "cnpg-credential", nil
	}
	if (r["kind"] == "PersistentVolumeClaim" || r["kind"] == "Pod") && field(r, "metadata", "labels", "cnpg.io/cluster") == "backstage-db" {
		pattern := `^backstage-db-[1-9][0-9]*$`
		if r["kind"] == "PersistentVolumeClaim" {
			pattern = `^backstage-db-[1-9][0-9]*(-wal)?$`
		}
		if !owned("Cluster", "backstage-db", text(field(db, "metadata", "uid"))) || !regexp.MustCompile(pattern).MatchString(name) {
			return "", errors.New("database claim producer unverified")
		}
		return "cnpg-role", nil
	}
	if r["kind"] == "ConfigMap" {
		switch name {
		case "kube-root-ca.crt":
			return "root-ca", nil
		case "cloudflare-origin-ca":
			if !owned("Bundle", "cloudflare-origin-ca", text(field(s.bundle, "metadata", "uid"))) || field(s.bundle, "spec", "target", "configMap", "key") != "ca-certificates.crt" || !emptyObject(field(s.bundle, "spec", "target", "additionalFormats")) || !emptyObject(field(s.bundle, "spec", "target", "configMap", "metadata")) || field(r, "metadata", "labels", "trust.cert-manager.io/bundle") != "cloudflare-origin-ca" {
				return "", errors.New("public bundle producer unverified")
			}
			return "trust-bundle", nil
		case "cnpg-default-monitoring":
			operatorVersion := text(field(r, "metadata", "annotations", "cnpg.io/operatorVersion"))
			_, versionErr := semver.Parse(operatorVersion)
			if field(r, "metadata", "labels", "app.kubernetes.io/managed-by") != "cloudnative-pg" || field(r, "metadata", "labels", "cnpg.io/reload") != "true" || versionErr != nil {
				return "", errors.New("monitoring producer metadata unverified")
			}
			return "cnpg-monitoring", nil
		}
	}
	return "", nil
}

func (e engine) collectWriters(ctx context.Context, s *snapshot) error {
	s.writers = map[string]map[string]any{}
	resources, err := members(s.inventory)
	if err != nil {
		return err
	}
	resources = append(resources, arrayObjects(s.live)...)
	needed := map[string]bool{"helm": true}
	for _, r := range resources {
		name := field(r, "metadata", "name")
		if r["kind"] == "Secret" && (name == "backstage-db-ca" || name == "backstage-db-server" || name == "backstage-db-replication") || r["kind"] == "PersistentVolumeClaim" && field(r, "metadata", "labels", "cnpg.io/cluster") == "backstage-db" || r["kind"] == "ConfigMap" && name == "cnpg-default-monitoring" {
			needed["cnpg"] = true
		}
		if r["kind"] == "ConfigMap" && name == "kube-root-ca.crt" {
			needed["rootCA"] = true
		}
		if r["kind"] == "ConfigMap" && name == "cloudflare-origin-ca" {
			needed["trust"] = true
		}
	}
	for _, writer := range []struct{ key, namespace, account string }{{"helm", "flux-system", "helm-controller"}, {"cnpg", "cnpg-system", "cloudnative-pg"}, {"rootCA", "kube-system", "root-ca-cert-publisher"}, {"trust", "cert-manager", "trust-manager"}} {
		if !needed[writer.key] {
			continue
		}
		account, err := e.read(ctx, "--namespace="+writer.namespace, "get", "serviceaccount", writer.account, "--output=json")
		if err != nil || !metadataValid(account) || account["apiVersion"] != "v1" || account["kind"] != "ServiceAccount" || field(account, "metadata", "namespace") != writer.namespace || field(account, "metadata", "name") != writer.account {
			return errors.New("retained controller account unverified")
		}
		s.writers[writer.key] = account
	}
	controller, err := e.read(ctx, "--namespace=flux-system", "get", "deployment", "helm-controller", "--output=json")
	if err != nil || !nativeControllerSupported(controller) {
		return errors.New("native history controller unverified")
	}
	s.writers["helmDeployment"] = controller
	if needed["cnpg"] {
		controller, err := e.read(ctx, "--namespace=cnpg-system", "get", "deployment", "cloudnative-pg", "--output=json")
		if err != nil || !retainedControllerSupported(controller, "cnpg-system", "cloudnative-pg", cnpgControllerImage) {
			return errors.New("retained database controller unverified")
		}
		s.writers["cnpgDeployment"] = controller
		for _, env := range array(field(object(array(field(controller, "spec", "template", "spec", "containers"))[0]), "env")) {
			if name := text(field(object(env), "name")); name == "INHERITED_ANNOTATIONS" || name == "INHERITED_LABELS" || name == "CONFIG_MAP_NAME" {
				return errors.New("uncharacterized metadata inheritance override")
			}
			o := object(env)
			if o["name"] == "MONITORING_QUERIES_CONFIGMAP" && (o["value"] != "cnpg-default-monitoring" || o["valueFrom"] != nil) || o["name"] == "MONITORING_QUERIES_SECRET" {
				return errors.New("uncharacterized monitoring source override")
			}
		}
		if len(array(field(object(array(field(controller, "spec", "template", "spec", "containers"))[0]), "envFrom"))) != 0 {
			return errors.New("uncharacterized metadata inheritance source")
		}
		config, err := e.read(ctx, "--namespace=cnpg-system", "get", "configmap", "cnpg-controller-manager-config", "--output=json")
		if err != nil || !metadataValid(config) || config["apiVersion"] != "v1" || config["kind"] != "ConfigMap" || field(config, "metadata", "namespace") != "cnpg-system" || field(config, "metadata", "name") != "cnpg-controller-manager-config" || !emptyObject(config["data"]) || !emptyObject(config["binaryData"]) {
			return errors.New("default metadata inheritance configuration unverified")
		}
		s.writers["cnpgConfig"] = config
		for _, r := range resources {
			if r["kind"] == "ConfigMap" && field(r, "metadata", "name") == "cnpg-default-monitoring" {
				s.monitoring, err = e.read(ctx, "--namespace=cnpg-system", "get", "configmap", "cnpg-default-monitoring", "--output=json")
				if err != nil || !metadataValid(s.monitoring) || s.monitoring["kind"] != "ConfigMap" || field(s.monitoring, "metadata", "namespace") != "cnpg-system" || field(s.monitoring, "metadata", "name") != "cnpg-default-monitoring" {
					return errors.New("monitoring source unverified")
				}
			}
		}
	}
	if needed["trust"] {
		controller, err := e.read(ctx, "--namespace=cert-manager", "get", "deployment", "trust-manager", "--output=json")
		if err != nil || !retainedControllerSupported(controller, "cert-manager", "trust-manager", trustControllerImage) {
			return errors.New("public bundle controller unverified")
		}
		s.writers["trustDeployment"] = controller
		s.bundle, err = e.read(ctx, "get", "bundle", "cloudflare-origin-ca", "--output=json")
		if err != nil || !metadataValid(s.bundle) || s.bundle["apiVersion"] != "trust.cert-manager.io/v1alpha1" || s.bundle["kind"] != "Bundle" || field(s.bundle, "metadata", "name") != "cloudflare-origin-ca" {
			return errors.New("public bundle source unverified")
		}
	}
	return nil
}

// The published constructor merges these entries, leaves unrelated metadata
// alone, and never copies its newly generated password over an existing Secret.
// Prove this merge is already a no-op before installing the durable fence.
func cnpgMetadataSettled(db, r map[string]any, credential bool) bool {
	if !emptyObject(field(db, "spec", "inheritedMetadata", "labels")) {
		return false
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "cloudnative-pg", "cnpg.io/cluster": "backstage-db"}
	if credential {
		labels["cnpg.io/reload"], labels["cnpg.io/userType"] = "true", "app"
	}
	for key, value := range labels {
		if field(r, "metadata", "labels", key) != value {
			return false
		}
	}
	if field(r, "metadata", "annotations", "cnpg.io/operatorVersion") != "1.30.1" {
		return false
	}
	for key, value := range object(field(db, "spec", "inheritedMetadata", "annotations")) {
		if field(r, "metadata", "annotations", key) != value {
			return false
		}
	}
	return true
}

func emptyObject(v any) bool {
	if v == nil {
		return true
	}
	o, ok := v.(map[string]any)
	return ok && len(o) == 0
}

func arrayObjects(list map[string]any) []map[string]any {
	result := []map[string]any{}
	for _, v := range array(list["items"]) {
		result = append(result, object(v))
	}
	return result
}

func retainedControllerSupported(r map[string]any, namespace, name, image string) bool {
	containers := array(field(r, "spec", "template", "spec", "containers"))
	replicas := integer(field(r, "spec", "replicas"))
	generation := integer(field(r, "metadata", "generation"))
	return metadataValid(r) && r["apiVersion"] == "apps/v1" && r["kind"] == "Deployment" && field(r, "metadata", "namespace") == namespace && field(r, "metadata", "name") == name && generation > 0 && replicas > 0 && integer(field(r, "status", "observedGeneration")) == generation && integer(field(r, "status", "readyReplicas")) == replicas && integer(field(r, "status", "updatedReplicas")) == replicas && integer(field(r, "status", "availableReplicas")) == replicas && field(r, "spec", "template", "spec", "serviceAccountName") == name && len(containers) == 1 && object(containers[0])["image"] == image
}

func certificateIdentity(s snapshot, r map[string]any, profile string) (string, error) {
	certKey, privateKey, typ := "tls.crt", "tls.key", "kubernetes.io/tls"
	if profile == "ca" {
		certKey, privateKey, typ = "ca.crt", "ca.key", "Opaque"
	}
	data := object(r["data"])
	crt, err := base64.StdEncoding.DecodeString(text(data[certKey]))
	if err != nil || len(data) != 2 || r["type"] != typ || len(crt) > 64<<10 || !publicPEM(map[string]any{"certificate": string(crt)}, "certificate") || len(object(r["stringData"])) != 0 {
		return "", errors.New("certificate profile incomplete")
	}
	key, err := base64.StdEncoding.DecodeString(text(data[privateKey]))
	if err != nil || len(key) > 64<<10 {
		return "", errors.New("certificate private-key evidence incomplete")
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil || len(pair.Certificate) != 1 {
		return "", errors.New("certificate key binding unverified")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || cert.IsCA != (profile == "ca") || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
		return "", errors.New("certificate validity unverified")
	}
	if profile == "ca" {
		if !bytes.Equal(cert.RawSubject, cert.RawIssuer) || cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) != nil {
			return "", errors.New("CA signature unverified")
		}
	} else {
		var authority *x509.Certificate
		for _, candidate := range arrayObjects(s.inventory) {
			if candidate["kind"] == "Secret" && field(candidate, "metadata", "name") == "backstage-db-ca" {
				if caProfile, err := controllerProfile(s, candidate); err != nil || caProfile != "ca" {
					return "", errors.New("certificate authority ownership unverified")
				}
				if _, err := certificateIdentity(s, candidate, "ca"); err != nil {
					return "", err
				}
				encoded, err := base64.StdEncoding.DecodeString(text(field(candidate, "data", "ca.crt")))
				if err != nil {
					return "", errors.New("authority encoding unverified")
				}
				block, _ := pem.Decode(encoded)
				if block != nil {
					authority, _ = x509.ParseCertificate(block.Bytes)
				}
			}
		}
		if authority == nil || !bytes.Equal(cert.RawIssuer, authority.RawSubject) || cert.CheckSignatureFrom(authority) != nil {
			return "", errors.New("certificate authority signature unverified")
		}
	}
	// Renewal may change serial, validity and signature bytes only. Subject,
	// issuer, public key and all extensions (including SANs) stay identical.
	b, err := json.Marshal([]any{cert.RawSubject, cert.RawIssuer, cert.RawSubjectPublicKeyInfo, cert.SignatureAlgorithm, cert.Extensions})
	if err != nil {
		return "", errors.New("certificate identity incomplete")
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func publicPEM(data map[string]any, key string) bool {
	value, ok := data[key].(string)
	if len(data) != 1 || !ok || value == "" || len(value) > 1<<20 {
		return false
	}
	rest, count := []byte(value), 0
	for len(bytes.TrimSpace(rest)) != 0 {
		rest = bytes.TrimSpace(rest)
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return false
		}
		end := bytes.Index(rest, []byte("-----END CERTIFICATE-----"))
		if end < 0 {
			return false
		}
		end += len("-----END CERTIFICATE-----")
		segment := rest[:end]
		if bytes.Count(segment, []byte("-----BEGIN CERTIFICATE-----")) != 1 {
			return false
		}
		block, trailing := pem.Decode(segment)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(trailing)) != 0 {
			return false
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return false
		}
		rest, count = rest[end:], count+1
	}
	return count > 0
}

func controllerProjection(s snapshot, r map[string]any, normalized bool) (map[string]any, error) {
	p := projection(r)
	profile, err := controllerProfile(s, r)
	if err != nil || profile == "" {
		return p, err
	}
	// Deep-copy before normalizing: snapshots and original Secret data must not
	// be modified by the comparison itself.
	p, err = decode(mustJSON(p))
	if err != nil {
		return nil, errors.New("retained projection incomplete")
	}
	switch profile {
	case "ca", "leaf":
		identity, err := certificateIdentity(s, r, profile)
		if err != nil {
			return nil, err
		}
		if normalized {
			key := "tls.crt"
			if profile == "ca" {
				key = "ca.crt"
			}
			object(p["data"])[key] = identity
		}
	case "cnpg-role":
		role := text(field(r, "metadata", "labels", "role"))
		if role != "primary" && role != "replica" || field(r, "metadata", "labels", "cnpg.io/instanceRole") != role {
			return nil, errors.New("database role metadata unverified")
		}
		// Both data and WAL claims follow the Cluster's settled primary.
		for _, candidate := range arrayObjects(s.live) {
			if candidate["kind"] == "Cluster" && field(candidate, "metadata", "name") == "backstage-db" {
				expected := "replica"
				if strings.TrimSuffix(text(field(r, "metadata", "name")), "-wal") == field(candidate, "status", "currentPrimary") {
					expected = "primary"
				}
				if role != expected {
					return nil, errors.New("claim role does not match the settled primary")
				}
			}
		}
		if normalized {
			delete(object(p["labels"]), "role")
			delete(object(p["labels"]), "cnpg.io/instanceRole")
		}
	case "root-ca", "trust-bundle":
		key := "ca.crt"
		if profile == "trust-bundle" {
			key = "ca-certificates.crt"
			if field(r, "metadata", "annotations", "trust.cert-manager.io/hash") != fmt.Sprintf("%x", sha256.Sum256([]byte(text(field(r, "data", key))))) {
				return nil, errors.New("public bundle bookkeeping incomplete")
			}
		}
		if !publicPEM(object(r["data"]), key) || len(object(r["binaryData"])) != 0 {
			return nil, errors.New("public CA profile incomplete")
		}
		if normalized {
			p["data"] = map[string]any{key: "controller-managed-public-ca"}
			if profile == "trust-bundle" {
				delete(object(p["annotations"]), "trust.cert-manager.io/hash")
			} else {
				delete(object(p["annotations"]), "kubernetes.io/description")
				// The publisher may add its description to an object with
				// no annotations. Only that field is excluded from comparison.
				if len(object(p["annotations"])) == 0 {
					p["annotations"] = nil
				}
			}
		}
	case "cnpg-monitoring":
		data := object(r["data"])
		if len(data) != 1 || text(data["queries"]) == "" || len(object(r["binaryData"])) != 0 || len(object(s.monitoring["binaryData"])) != 0 || !reflect.DeepEqual(data, object(s.monitoring["data"])) {
			return nil, errors.New("generated monitoring data unverified")
		}
		if normalized {
			p["data"] = map[string]any{"queries": "controller-managed-monitoring"}
			// CloudNativePG updates this last-writer annotation only when it
			// patches changed query data, so an operator-only upgrade leaves the
			// prior valid version in place.
			object(p["annotations"])["cnpg.io/operatorVersion"] = "controller-managed-version"
		}
	}
	return p, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v) // Kubernetes JSON has already been decoded.
	return b
}

func normalizePreserved(s snapshot) (map[string]map[string]any, error) {
	// Only our installed, read-back journal enables normal controller changes.
	if s.fence == "" || field(s.namespace, "metadata", "annotations", fenceAnnotation) != s.fence {
		return nil, errors.New("normalization requires the owned fence")
	}
	return preserved(s)
}

func normalizedNamespace(s snapshot, record string) snapshot {
	s.namespace, _ = decode(mustJSON(s.namespace))
	object(field(s.namespace, "metadata", "annotations"))[fenceAnnotation] = record
	s.fence = record
	return s
}
