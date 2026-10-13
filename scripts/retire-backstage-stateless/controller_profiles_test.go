package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// These certificates and keys are generated fixtures, never production data.
func certificateFixture(t *testing.T, key *ecdsa.PrivateKey, serial int64, ca bool, subject string, authorities ...map[string]any) (string, string) {
	t.Helper()
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: subject}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature}
	if ca {
		cert.KeyUsage |= x509.KeyUsageCertSign
	} else {
		cert.DNSNames = []string{"backstage-db-rw.backstage.svc"}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	parent, signer := cert, key
	if !ca && len(authorities) == 1 {
		public, err := base64.StdEncoding.DecodeString(text(field(authorities[0], "data", "ca.crt")))
		if err != nil {
			t.Fatal("invalid fixture authority")
		}
		block, _ := pem.Decode(public)
		parent, err = x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal("invalid fixture authority")
		}
		private, err := base64.StdEncoding.DecodeString(text(field(authorities[0], "data", "ca.key")))
		if err != nil {
			t.Fatal("invalid fixture authority key")
		}
		block, _ = pem.Decode(private)
		signer, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			t.Fatal("invalid fixture authority key")
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
}

func TestPinnedCNPGSecretProducerProfiles(t *testing.T) {
	t.Run("credential", func(t *testing.T) {
		f := retirementFixture(t)
		secret := item(f.inventory, 0)
		s := snapshot{live: f.live}
		if profile, err := controllerProfile(s, secret); err != nil || profile != "cnpg-credential" {
			t.Fatalf("default basic-auth credential rejected: profile=%q err=%v", profile, err)
		}
		secret["type"] = "Opaque"
		if _, err := controllerProfile(s, secret); err == nil {
			t.Fatal("uncharacterized credential type accepted")
		}
	})
	t.Run("certificate", func(t *testing.T) {
		f := retirementFixture(t)
		_, certificate := addCertificateFixture(t, f, "backstage-db-ca", "ca")
		s := snapshot{live: f.live}
		if profile, err := controllerProfile(s, certificate); err != nil || profile != "ca" {
			t.Fatalf("constructor certificate metadata rejected: profile=%q err=%v", profile, err)
		}
		object(field(certificate, "metadata", "labels"))["app.kubernetes.io/managed-by"] = "foreign-producer"
		if _, err := controllerProfile(s, certificate); err == nil {
			t.Fatal("foreign certificate producer accepted")
		}
	})
}

func TestRetirementPublicControllerRefreshAndRoleCatchup(t *testing.T) {
	for _, profile := range []string{"root-ca", "trust-bundle", "cnpg-monitoring", "cnpg-role"} {
		modes := []string{"refresh", "extra data", "unrelated annotation", "writer replaced"}
		if profile == "root-ca" {
			modes = append(modes, "missing description")
		}
		for _, mode := range modes {
			t.Run(profile+"/"+mode, func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				oldCert, _ := certificateFixture(t, key, 1, true, "public-ca")
				newCert, _ := certificateFixture(t, key, 2, true, "public-ca")
				oldPEM, _ := base64.StdEncoding.DecodeString(oldCert)
				newPEM, _ := base64.StdEncoding.DecodeString(newCert)
				resource := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata("kube-root-ca.crt", "public-ca-uid"), "data": map[string]any{"ca.crt": string(oldPEM)}}
				object(resource["metadata"])["annotations"] = map[string]any{"kubernetes.io/description": rootCADescription}
				if mode == "missing description" {
					delete(object(resource["metadata"]), "annotations")
				}
				bundle := map[string]any{"apiVersion": "trust.cert-manager.io/v1alpha1", "kind": "Bundle", "metadata": map[string]any{"name": "cloudflare-origin-ca", "uid": "bundle-uid", "resourceVersion": "1"}, "spec": map[string]any{"target": map[string]any{"configMap": map[string]any{"key": "ca-certificates.crt"}}}}
				monitoring := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cnpg-default-monitoring", "namespace": "cnpg-system", "uid": "monitoring-source-uid", "resourceVersion": "1"}, "data": map[string]any{"queries": "SELECT 1"}}
				switch profile {
				case "trust-bundle":
					object(resource["metadata"])["name"] = "cloudflare-origin-ca"
					object(resource["metadata"])["labels"] = map[string]any{"trust.cert-manager.io/bundle": "cloudflare-origin-ca"}
					object(resource["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "trust.cert-manager.io/v1alpha1", "kind": "Bundle", "name": "cloudflare-origin-ca", "uid": "bundle-uid", "controller": true}}
					object(resource["metadata"])["annotations"] = map[string]any{"trust.cert-manager.io/hash": fmt.Sprintf("%x", sha256.Sum256(oldPEM))}
					resource["data"] = map[string]any{"ca-certificates.crt": string(oldPEM)}
				case "cnpg-monitoring":
					object(resource["metadata"])["name"] = "cnpg-default-monitoring"
					object(resource["metadata"])["annotations"] = map[string]any{"cnpg.io/operatorVersion": "1.30.1"}
					resource["data"] = map[string]any{"queries": "SELECT 1"}
				case "cnpg-role":
					resource = item(f.live, 3)
					// Exercise a real two-instance primary transition, not a
					// status naming a primary absent from the healthy inventory.
					object(item(f.live, 2)["spec"])["instances"] = 2
					object(item(f.live, 2)["status"])["readyInstances"] = 2
					pod, _ := decode(encode(t, item(f.live, 4)))
					claim, _ := decode(encode(t, resource))
					volume, _ := decode(encode(t, item(f.volumes, 0)))
					for _, r := range []map[string]any{pod, claim} {
						object(r["metadata"])["name"] = "backstage-db-4"
						object(r["metadata"])["uid"] = "second-" + text(r["kind"]) + "-uid"
						object(field(r, "metadata", "labels"))["role"] = "replica"
						object(field(r, "metadata", "labels"))["cnpg.io/instanceRole"] = "replica"
					}
					object(claim["spec"])["volumeName"] = "second-retained-volume"
					object(volume["metadata"])["name"] = "second-retained-volume"
					object(volume["metadata"])["uid"] = "second-volume-uid"
					object(volume["spec"])["claimRef"] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": "backstage-db-4", "namespace": "backstage", "uid": field(claim, "metadata", "uid")}
					f.live["items"] = append(array(f.live["items"]), pod, claim)
					f.volumes["items"] = append(array(f.volumes["items"]), volume)
				}
				if profile != "cnpg-role" {
					f.inventory["items"] = append(array(f.inventory["items"]), resource)
				}
				changed := false
				execute := func(c context.Context, p string, a ...string) ([]byte, error) {
					args := strings.Join(a, " ")
					if f.fenceInstalled && !changed && strings.Contains(args, "get deployment,pod,persistentvolumeclaim") {
						changed = true
						switch profile {
						case "root-ca":
							object(resource["data"])["ca.crt"] = string(newPEM)
							if mode == "missing description" {
								object(resource["metadata"])["annotations"] = map[string]any{"kubernetes.io/description": rootCADescription}
							}
						case "trust-bundle":
							object(resource["data"])["ca-certificates.crt"] = string(newPEM)
							object(field(resource, "metadata", "annotations"))["trust.cert-manager.io/hash"] = fmt.Sprintf("%x", sha256.Sum256(newPEM))
						case "cnpg-monitoring":
							resource["data"] = map[string]any{"queries": "SELECT 2"}
							monitoring["data"] = map[string]any{"queries": "SELECT 2"}
						case "cnpg-role":
							object(item(f.live, 2)["status"])["currentPrimary"] = "backstage-db-4"
							object(item(f.live, 2)["status"])["targetPrimary"] = "backstage-db-4"
							for _, r := range arrayObjects(f.live) {
								if r["kind"] != "Pod" && r["kind"] != "PersistentVolumeClaim" || field(r, "metadata", "labels", "cnpg.io/cluster") != "backstage-db" {
									continue
								}
								role := "replica"
								if field(r, "metadata", "name") == "backstage-db-4" {
									role = "primary"
								}
								object(field(r, "metadata", "labels"))["role"] = role
								object(field(r, "metadata", "labels"))["cnpg.io/instanceRole"] = role
							}
						}
						if mode == "extra data" {
							resource["data"] = map[string]any{"password": "PRIVATE_ADDED"}
						}
						if mode == "unrelated annotation" {
							object(field(resource, "metadata", "annotations"))["unrelated"] = "PRIVATE_CHANGED"
						}
					}
					for _, account := range []struct{ name, namespace string }{{"root-ca-cert-publisher", "kube-system"}, {"trust-manager", "cert-manager"}} {
						if strings.Contains(args, "get serviceaccount "+account.name) {
							uid := account.name + "-uid"
							if f.fenceInstalled && mode == "writer replaced" {
								uid = "replaced-writer"
							}
							return encode(t, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": account.name, "namespace": account.namespace, "uid": uid, "resourceVersion": "1"}}), nil
						}
					}
					if strings.Contains(args, "get deployment trust-manager") {
						return encode(t, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "trust-manager", "namespace": "cert-manager", "uid": "trust-deployment-uid", "resourceVersion": "1", "generation": 1}, "spec": map[string]any{"replicas": 2, "template": map[string]any{"spec": map[string]any{"serviceAccountName": "trust-manager", "containers": []any{map[string]any{"image": trustControllerImage}}}}}, "status": map[string]any{"observedGeneration": 1, "readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2}}), nil
					}
					if strings.Contains(args, "get bundle cloudflare-origin-ca") {
						return encode(t, bundle), nil
					}
					if strings.Contains(args, "--namespace=cnpg-system get configmap cnpg-default-monitoring") {
						return encode(t, monitoring), nil
					}
					b, err := f.execute(c, p, a...)
					if strings.Contains(args, "get serviceaccount cloudnative-pg") && f.fenceInstalled && mode == "writer replaced" && (profile == "cnpg-role" || profile == "cnpg-monitoring") && err == nil {
						account, _ := decode(b)
						object(account["metadata"])["uid"] = "replaced-writer"
						return encode(t, account), nil
					}
					return b, err
				}
				var out bytes.Buffer
				code := run([]string{"--execute"}, &out, execute)
				if mode == "refresh" || mode == "missing description" {
					if code != 0 || !changed || !f.deleted {
						t.Fatalf("legitimate generated refresh blocked: code=%d %s", code, &out)
					}
				} else if code != 2 || f.deleted || strings.Contains(out.String(), "PRIVATE") {
					t.Fatalf("unsafe generated refresh accepted: code=%d %s", code, &out)
				}
			})
		}
	}
}

func TestPublicPEMRejectsNonCertificateText(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := certificateFixture(t, key, 1, true, "public-ca")
	encoded, _ := base64.StdEncoding.DecodeString(cert)
	for _, value := range []string{"PRIVATE_PREFIX\n" + string(encoded), string(encoded) + "PRIVATE_SUFFIX", string(encoded) + "PRIVATE_MIDDLE\n" + string(encoded), "-----BEGIN CERTIFICATE-----\nPRIVATE_INVALID_BLOCK\n" + string(encoded), "-----BEGIN CERTIFICATE-----\nPRIVATE_INVALID_BLOCK\n-----END CERTIFICATE-----\n" + string(encoded)} {
		if publicPEM(map[string]any{"ca.crt": value}, "ca.crt") {
			t.Fatal("non-certificate text accepted as normalized public material")
		}
	}
	if !publicPEM(map[string]any{"ca.crt": "\n" + string(encoded) + "\n" + string(encoded)}, "ca.crt") {
		t.Fatal("ordinary whitespace-separated CA bundle rejected")
	}
}

func TestUnsettledCredentialMetadataNeverInstallsFence(t *testing.T) {
	for _, mode := range []string{"missing managed label", "missing version", "inherited annotation absent", "uncharacterized configuration", "malformed configuration", "inheritance override", "monitoring config override", "monitoring secret override"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			switch mode {
			case "missing managed label":
				delete(object(field(item(f.inventory, 0), "metadata", "labels")), "app.kubernetes.io/managed-by")
			case "missing version":
				delete(object(field(item(f.inventory, 0), "metadata", "annotations")), "cnpg.io/operatorVersion")
			case "inherited annotation absent":
				object(item(f.live, 2)["spec"])["inheritedMetadata"] = map[string]any{"annotations": map[string]any{"coroot.com/postgres-scrape": "true"}}
			}
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				b, err := f.execute(c, p, a...)
				args := strings.Join(a, " ")
				if err == nil && strings.Contains(args, "get configmap cnpg-controller-manager-config") && (mode == "uncharacterized configuration" || mode == "malformed configuration") {
					r, _ := decode(b)
					r["data"] = map[string]any{"INHERITED_ANNOTATIONS": "*"}
					if mode == "malformed configuration" {
						r["data"] = "invalid"
					}
					return encode(t, r), nil
				}
				if err == nil && strings.Contains(args, "get deployment cloudnative-pg") && (mode == "inheritance override" || mode == "monitoring config override" || mode == "monitoring secret override") {
					r, _ := decode(b)
					name, value := "INHERITED_ANNOTATIONS", "*"
					if mode == "monitoring config override" {
						name, value = "MONITORING_QUERIES_CONFIGMAP", "alternate-queries"
					} else if mode == "monitoring secret override" {
						name, value = "MONITORING_QUERIES_SECRET", "alternate-queries"
					}
					object(array(field(r, "spec", "template", "spec", "containers"))[0])["env"] = []any{map[string]any{"name": name, "value": value}}
					return encode(t, r), nil
				}
				return b, err
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, execute); code != 2 || f.fenceInstalled || f.deleted || f.receiptCreated || !strings.Contains(out.String(), "HOLD_PRECONDITION") {
				t.Fatalf("unsettled producer was durably frozen: code=%d %s", code, &out)
			}
		})
	}
}

func TestMissingSettledPrimaryNeverAuthorizesRetirement(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	object(item(f.live, 2)["status"])["currentPrimary"] = "backstage-db-4"
	object(item(f.live, 2)["status"])["targetPrimary"] = "backstage-db-4"
	for _, r := range []map[string]any{item(f.live, 3), item(f.live, 4)} {
		object(field(r, "metadata", "labels"))["role"] = "replica"
		object(field(r, "metadata", "labels"))["cnpg.io/instanceRole"] = "replica"
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.fenceInstalled || f.deleted {
		t.Fatalf("status naming an absent primary authorized lifecycle writes: code=%d %s", code, &out)
	}
}

func addCertificateFixture(t *testing.T, f *portalFixture, name, profile string) (*ecdsa.PrivateKey, map[string]any) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var authority map[string]any
	if profile != "ca" {
		for _, candidate := range array(f.inventory["items"]) {
			if field(object(candidate), "metadata", "name") == "backstage-db-ca" {
				authority = object(candidate)
			}
		}
		if authority == nil {
			_, authority = addCertificateFixture(t, f, "backstage-db-ca", "ca")
		}
	}
	cert, private := certificateFixture(t, key, 1, profile == "ca", name, authority)
	crtKey, privateKey, secretType := "tls.crt", "tls.key", "kubernetes.io/tls"
	if profile == "ca" {
		crtKey, privateKey, secretType = "ca.crt", "ca.key", "Opaque"
	}
	r := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata(name, name+"-uid"), "type": secretType, "data": map[string]any{crtKey: cert, privateKey: private}}
	// The pinned PKI constructor adds only managed-by; unlike credentials,
	// its ownership path does not copy Cluster inheritance or operatorVersion.
	object(r["metadata"])["labels"] = map[string]any{"app.kubernetes.io/managed-by": "cloudnative-pg"}
	object(r["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "backstage-db", "uid": "database-uid", "controller": true}}
	f.inventory["items"] = append(array(f.inventory["items"]), r)
	object(item(f.live, 2)["status"])["certificates"] = map[string]any{"serverCASecret": "backstage-db-ca", "clientCASecret": "backstage-db-ca", "serverTLSSecret": "backstage-db-server", "replicationTLSSecret": "backstage-db-replication"}
	return key, r
}

func TestNativeRetirementAcceptsOnlyIdentityPreservingCertificateRenewal(t *testing.T) {
	for _, profile := range []string{"ca", "server", "replication"} {
		for _, change := range []string{"renew", "private key", "subject", "signature", "extra data", "owner"} {
			t.Run(profile+"/"+change, func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				key, secret := addCertificateFixture(t, f, "backstage-db-"+profile, profile)
				crtKey, keyKey := "tls.crt", "tls.key"
				if profile == "ca" {
					crtKey, keyKey = "ca.crt", "ca.key"
				}
				changed := false
				execute := func(c context.Context, p string, a ...string) ([]byte, error) {
					if f.fenceInstalled && !changed && strings.Contains(strings.Join(a, " "), "get service,configmap,secret") {
						changed = true
						subject := "backstage-db-" + profile
						if change == "subject" {
							subject = "foreign-identity"
						}
						var authority map[string]any
						for _, candidate := range arrayObjects(f.inventory) {
							if field(candidate, "metadata", "name") == "backstage-db-ca" {
								authority = candidate
							}
						}
						cert, _ := certificateFixture(t, key, 2, profile == "ca", subject, authority)
						if change == "signature" {
							encoded, _ := base64.StdEncoding.DecodeString(cert)
							block, _ := pem.Decode(encoded)
							block.Bytes[len(block.Bytes)-1] ^= 1
							cert = base64.StdEncoding.EncodeToString(pem.EncodeToMemory(block))
						}
						object(secret["data"])[crtKey] = cert
						switch change {
						case "private key":
							object(secret["data"])[keyKey] = "PRIVATE_CHANGED"
						case "extra data":
							object(secret["data"])["password"] = "PRIVATE_ADDED"
						case "owner":
							object(array(field(secret, "metadata", "ownerReferences"))[0])["uid"] = "foreign-database"
						}
					}
					// The controller account's immutable identity is independent of
					// the certificate's owner. The production path must read it.
					if strings.Contains(strings.Join(a, " "), "get serviceaccount cloudnative-pg") {
						return encode(t, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "cloudnative-pg", "namespace": "cnpg-system", "uid": "cnpg-controller-uid", "resourceVersion": "1"}}), nil
					}
					if strings.Contains(strings.Join(a, " "), "get deployment cloudnative-pg") {
						return encode(t, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "cloudnative-pg", "namespace": "cnpg-system", "uid": "cnpg-deployment-uid", "resourceVersion": "1", "generation": 1}, "spec": map[string]any{"replicas": 2, "template": map[string]any{"spec": map[string]any{"serviceAccountName": "cloudnative-pg", "containers": []any{map[string]any{"image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"}}}}}, "status": map[string]any{"observedGeneration": 1, "readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2}}), nil
					}
					return f.execute(c, p, a...)
				}
				var out bytes.Buffer
				code := run([]string{"--execute"}, &out, execute)
				if change == "renew" {
					if code != 0 || !changed || !f.deleted {
						t.Fatalf("legitimate certificate renewal blocked retirement: code=%d %s", code, &out)
					}
				} else if code != 2 || f.deleted || strings.Contains(out.String(), "PRIVATE") {
					t.Fatalf("unsafe renewal reached deletion or disclosed content: code=%d %s", code, &out)
				}
			})
		}
	}
}
