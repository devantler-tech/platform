package compiler_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reuse verified API-server assets; no scheduler, kubelet, workload process,
// production client or ambient kubeconfig participates in this protocol proof.
func TestMailPodDeletionPreconditions(t *testing.T) {
	root := os.Getenv("PLATFORM_TEST_ROOT")
	if root == "" {
		t.Fatal("explicit fixture source root required")
	}
	environment := newCompilerEnvironment(t, nil)
	cfg, err := startCompilerEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err = corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "private-mail"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "private-mail"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "private-mail-data", Namespace: "private-mail"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Mi")}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "private-mail-0", Namespace: "private-mail"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "private-mail", Image: "example.invalid/not-executed:v1", VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}}, Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "private-mail-data"}}}}}},
	} {
		if err = api.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	config := clientcmdapi.NewConfig()
	config.Clusters["fixture"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	config.AuthInfos["fixture"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	config.Contexts["admin@prod"] = &clientcmdapi.Context{Cluster: "fixture", AuthInfo: "fixture"}
	config.CurrentContext = "admin@prod"
	data, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "go", "test", "-count=1", "-run", "^TestNativeCorePodDeletePreconditions$", "-v", "./scripts/recover-private-mail-probes")
	command.Dir = root
	command.Env = append(os.Environ(), "MAIL_NATIVE_API_CONFIG="+path, "MAIL_NATIVE_API_SERVER="+cfg.Host, "KUBECONFIG="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native Pod deletion acceptance failed: %v\n%s", err, output)
	}
	t.Log(string(output))
}
