package worldtrialprofile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const trialDir = "k8s/providers/hetzner/apps/world-at-ruin"

func TestUnknownProfileRefuses(t *testing.T) {
	if err := Validate(t.TempDir()); err == nil {
		t.Fatal("empty input was accepted as an audited private trial")
	}
}

func TestUnsafeArchiveRefusesBeforeExtraction(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		link string
	}{
		{"../outside.yaml", tar.TypeReg, ""},
		{"/absolute.yaml", tar.TypeReg, ""},
		{"deployment.yaml", tar.TypeSymlink, "../../outside"},
		{"deployment.yaml", tar.TypeLink, "outside"},
		{"extra.yaml", tar.TypeReg, ""},
	} {
		t.Run(tc.name+string(tc.kind), func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Linkname: tc.link, Mode: 0600}); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectLayer(buf.Bytes()); err == nil {
				t.Fatal("unsafe or unreviewed archive accepted")
			}
		})
	}
}

func TestOfflineReviewedTrialAndAdverseChanges(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(repo); err != nil {
		t.Fatalf("reviewed baseline: %v", err)
	}
	for _, tc := range []struct{ name, path, old, replacement string }{
		{"mutable-release", trialDir + "/oci-repository.yaml", "digest: sha256:", "semver: sha256:"},
		{"different-artifact", trialDir + "/oci-repository.yaml", "41734486", "51734486"},
		{"different-owner", trialDir + "/role-binding-operator.yaml", "oidc:${admin_email}", "oidc:someone-else"},
		{"secret-api", trialDir + "/role-operator.yaml", "pods/portforward, pods/exec", "pods/portforward, pods/exec, secrets"},
		{"persistent-storage", trialDir + "/resource-quota.yaml", "persistentvolumeclaims: \"0\"", "persistentvolumeclaims: \"1\""},
		{"public-network", trialDir + "/network-policy.yaml", "podSelector: {}", "podSelector: {matchLabels: {trial: one}}"},
		{"registry-credential", trialDir + "/service-account.yaml", "automountServiceAccountToken: false", "automountServiceAccountToken: false\nimagePullSecrets: [{name: ghcr-auth}]"},
		{"different-source", trialDir + "/flux-kustomization.yaml", "path: .", "path: ./production"},
		{"lost-retirement", trialDir + "/limit-range.yaml", "prune: disabled", "prune: enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			copyProfile(t, repo, root)
			p := filepath.Join(root, tc.path)
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.old, tc.replacement, 1)
			if changed == string(data) {
				t.Fatalf("ineffective mutation %q", tc.old)
			}
			if err := os.WriteFile(p, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Validate(root); err == nil {
				t.Fatal("changed trial was accepted")
			}
		})
	}
}

func copyProfile(t *testing.T, src, dst string) {
	t.Helper()
	r, err := reviewed()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{auditDir: true}
	for path := range r.HostFiles {
		paths[path] = true
	}
	for path := range r.ReferenceFiles {
		paths[path] = true
	}
	for p := range paths {
		full := filepath.Join(src, p)
		err := filepath.WalkDir(full, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			out := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(out, 0700)
			}
			if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutsideReferencesAndUnrelatedEdits(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		refuse     bool
	}{
		{"novel-role-binding", "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: novel, namespace: world-at-ruin}\nsubjects: [{kind: User, name: stranger}]\n", true},
		{"encoded-namespace", "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: production, namespace: \"world\\u002Dat-ruin\"}\n", true},
		{"production-credential", "apiVersion: v1\nkind: Secret\nmetadata: {name: production, namespace: world-at-ruin}\ndata: {key: eA==}\n", true},
		{"unrelated-app", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: unrelated, namespace: another-app}\ndata: {display: changed}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			copyProfile(t, repo, root)
			p := filepath.Join(root, "k8s/providers/hetzner/apps/other.yaml")
			if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			err := Validate(root)
			if (err != nil) != tc.refuse {
				t.Fatalf("refuse=%v, got %v", tc.refuse, err)
			}
		})
	}
	t.Run("unrelated-vault-policy", func(t *testing.T) {
		root := t.TempDir()
		copyProfile(t, repo, root)
		p := filepath.Join(root, "k8s/bases/infrastructure/vault-config/job.yaml")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(string(b), "policies=app-wedding-app", "policies=app-wedding-app,another-policy", 1)
		if changed == string(b) {
			t.Fatal("ineffective unrelated edit")
		}
		if err := os.WriteFile(p, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Validate(root); err != nil {
			t.Fatalf("unrelated Vault stanza refused: %v", err)
		}
	})
}

func TestCompletePayloadAdverseInputs(t *testing.T) {
	raw, err := os.ReadFile("testdata/audit/layer.tar.gz.golden")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := inspectLayer(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, file, old, replacement string }{
		{"extra-object", "service.yaml", "apiVersion: v1", "apiVersion: v1\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: extra}"},
		{"production-credential", "deployment.yaml", "WAR_ZONE_ADMISSION_SECRET", "PRODUCTION_ZONE_CREDENTIAL"},
		{"authoritative-persistence", "deployment.yaml", "volumes:\n", "volumes:\n        - name: durable\n          persistentVolumeClaim: {claimName: player-data}\n"},
		{"production-agones", "deployment.yaml", "- operator-trial", "- operator-trial\n            - -agones"},
		{"different-image", "deployment.yaml", "23a0bb22", "33a0bb22"},
		{"remote-resource", "kustomization.yaml", "resources:", "resources:\n  - https://untrusted.invalid/production.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string][]byte{}
			for p, b := range approved {
				files[p] = b
			}
			changed := strings.Replace(string(files[tc.file]), tc.old, tc.replacement, 1)
			if changed == string(files[tc.file]) {
				t.Fatal("ineffective mutation")
			}
			files[tc.file] = []byte(changed)
			if _, err := inspectLayer(makeLayer(t, files)); err == nil {
				t.Fatal("changed complete artifact admitted")
			}
		})
	}
	t.Run("duplicate-path", func(t *testing.T) {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		for range 2 {
			data := approved["deployment.yaml"]
			if err := tw.WriteHeader(&tar.Header{Name: "deployment.yaml", Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0600}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectLayer(b.Bytes()); err == nil {
			t.Fatal("duplicate artifact path accepted")
		}
	})
}

func makeLayer(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
		t.Fatal(err)
	}
	for p, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
