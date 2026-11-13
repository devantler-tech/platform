package namespaceinventory

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var credentialPath = regexp.MustCompile(`^/dev/shm/longhorn-namespace-inventory\.[A-Za-z0-9]+/kubeconfig$`)
var sourcePattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var runPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// FreshEmailDisposalConfirm selects the fixed fresh Email recovery copy.
const FreshEmailDisposalConfirm = "dispose-fresh-retained-email-copy"

// Dispatch is the verified source commit and run of a protected observation.
type Dispatch struct{ Source, Run string }

// AuthorizeProtectedDispatch accepts only the confirmed first attempt of the
// observation workflow on main, on a GitHub-hosted runner.
func AuthorizeProtectedDispatch(getenv func(string) string) (Dispatch, error) {
	for key, want := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF":        "devantler-tech/platform/.github/workflows/observe-longhorn-namespace-inventory.yaml@refs/heads/main",
		"LONGHORN_NAMESPACE_CONFIRM": "observe-longhorn-namespace-inventory",
	} {
		if getenv(key) != want {
			return Dispatch{}, errors.New("protected namespace observation dispatch is unverified")
		}
	}
	if !sourcePattern.MatchString(getenv("GITHUB_SHA")) || !runPattern.MatchString(getenv("GITHUB_RUN_ID")) {
		return Dispatch{}, errors.New("namespace observation source and run are required")
	}
	return Dispatch{getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")}, nil
}

// LoadProtectedClient reads only the private memory-backed credential created
// by the confirmed first-attempt workflow. It never loads an ambient context,
// credential file named by kubeconfig content, or authentication plugin.
func LoadProtectedClient(getenv func(string) string) (*http.Client, *url.URL, error) {
	if _, err := AuthorizeProtectedDispatch(getenv); err != nil {
		return nil, nil, err
	}
	path := getenv("KUBECONFIG")
	if !credentialPath.MatchString(path) || filepath.Clean(path) != path {
		return nil, nil, errors.New("fixed memory-backed kubeconfig is required")
	}
	raw, err := readPrivateKubeconfig(path)
	if err != nil {
		return nil, nil, err
	}
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, nil, errors.New("protected kubeconfig could not be decoded")
	}
	return newProductionClient(config)
}

// LoadRetainedEmailClient shares credential validation, never authorization,
// with the benchmark observer. The fixed Email disposal dispatch is separate.
func LoadRetainedEmailClient(getenv func(string) string) (*http.Client, *url.URL, error) {
	if err := AuthorizeRetainedEmailDispatch(getenv); err != nil {
		return nil, nil, err
	}
	path := getenv("KUBECONFIG")
	if !regexp.MustCompile(`^/dev/shm/email-retained-disposal\.[A-Za-z0-9]+/kubeconfig$`).MatchString(path) || filepath.Clean(path) != path {
		return nil, nil, errors.New("fixed private Email kubeconfig is required")
	}
	raw, err := readPrivateKubeconfig(path)
	if err != nil {
		return nil, nil, err
	}
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, nil, errors.New("protected Email kubeconfig could not be decoded")
	}
	return newProductionClient(config)
}

// AuthorizeRetainedEmailDispatch refuses unless the environment is the first attempt of the
// confirmed, maintainer-dispatched disposal workflow on main in this repository, with a
// well-formed source commit and run id.
func AuthorizeRetainedEmailDispatch(getenv func(string) string) error {
	confirm := getenv("EMAIL_RETAINED_COPY_CONFIRM")
	if confirm != "dispose-retained-email-copy" && confirm != FreshEmailDisposalConfirm {
		return errors.New("protected Email disposal dispatch is unverified")
	}
	for key, want := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform",
		"GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_ACTOR": "devantler",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/dispose-retained-email-copy.yaml@refs/heads/main",
	} {
		if getenv(key) != want {
			return errors.New("protected Email disposal dispatch is unverified")
		}
	}
	if !sourcePattern.MatchString(getenv("GITHUB_SHA")) || !runPattern.MatchString(getenv("GITHUB_RUN_ID")) {
		return errors.New("email disposal source and run are required")
	}
	return nil
}

func readPrivateKubeconfig(path string) (raw []byte, err error) {
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("credential directory is not private")
	}
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("credential file is not private and regular")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("credential directory could not be opened")
	}
	defer func() {
		if root.Close() != nil {
			raw = nil
			err = errors.New("credential directory cleanup failed")
		}
	}()
	file, err := root.Open("kubeconfig")
	if err != nil {
		return nil, errors.New("credential file could not be opened within its private directory")
	}
	defer func() {
		if file.Close() != nil {
			raw = nil
			err = errors.New("credential file cleanup failed")
		}
	}()
	raw, err = io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(raw) == 0 || len(raw) > 1024*1024 {
		return nil, errors.New("credential file is incomplete or oversized")
	}
	return raw, nil
}

func newProductionClient(raw *clientcmdapi.Config) (*http.Client, *url.URL, error) {
	if raw == nil || raw.Contexts["admin@prod"] == nil {
		return nil, nil, errors.New("fixed production context is missing")
	}
	selected := raw.Contexts["admin@prod"]
	cluster, auth := raw.Clusters[selected.Cluster], raw.AuthInfos[selected.AuthInfo]
	if cluster == nil || auth == nil || selected.Cluster == "" || selected.AuthInfo == "" {
		return nil, nil, errors.New("production credential binding is incomplete")
	}
	if cluster.InsecureSkipTLSVerify || cluster.ProxyURL != "" || cluster.CertificateAuthority != "" || len(cluster.CertificateAuthorityData) == 0 {
		return nil, nil, errors.New("production TLS trust must be inline and verified without proxy overrides")
	}
	if auth.Exec != nil || auth.AuthProvider != nil || auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || auth.Impersonate != "" || auth.ImpersonateUID != "" || len(auth.ImpersonateGroups) != 0 || len(auth.ImpersonateUserExtra) != 0 || auth.Username != "" || auth.Password != "" {
		return nil, nil, errors.New("dynamic, external or impersonated credentials are forbidden")
	}
	hasToken, hasCertificate := auth.Token != "", len(auth.ClientCertificateData) > 0 && len(auth.ClientKeyData) > 0
	if hasToken == hasCertificate || (len(auth.ClientCertificateData) > 0) != (len(auth.ClientKeyData) > 0) {
		return nil, nil, errors.New("exactly one complete inline production credential is required")
	}
	endpoint, err := url.Parse(cluster.Server)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Opaque != "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, nil, errors.New("production API must be a fixed HTTPS root")
	}
	endpoint.Path, endpoint.RawPath = "", ""
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, "admin@prod", &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, nil, errors.New("production REST credential could not be constructed")
	}
	config.Host = endpoint.String()
	config.Timeout = 20 * time.Second
	config.DisableCompression = true
	config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	client, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, nil, errors.New("verified production HTTP transport could not be constructed")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client, endpoint, nil
}
