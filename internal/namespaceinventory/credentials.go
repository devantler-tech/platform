package namespaceinventory

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var credentialPath = regexp.MustCompile(`^/dev/shm/longhorn-namespace-inventory\.[A-Za-z0-9]+/kubeconfig$`)
var benchmarkCredentialPath = regexp.MustCompile(`^/dev/shm/longhorn-v2\.[A-Za-z0-9]+/kubeconfig$`)
var sourcePattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var runPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var cnpgNameRecoveryPattern = regexp.MustCompile(`^(recover-(?:umami|wedding|provider-cnpg)-standby-name):((?:umami|wedding|customer-identity|business-site-portal)-db)-[1-9][0-9]*:([1-9][0-9]{0,18}):[0-9a-f]{40}:[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}:[1-9][0-9]{0,18}:[0-9a-f]{40}:[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}(?::[1-9][0-9]{0,18})?$`)

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

var emailRecoveryCredentialPath = regexp.MustCompile(`^/dev/shm/email-data-recovery\.[A-Za-z0-9]+/kubeconfig$`)

// LoadEmailRecoveryClient reads only the private memory-backed credential of
// the confirmed first-attempt Email recovery dispatch. It shares credential
// validation, never authorization, with the benchmark observer.
func LoadEmailRecoveryClient(getenv func(string) string) (*http.Client, *url.URL, error) {
	for key, want := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_ACTOR":        "devantler",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/verify-email-data-recovery.yaml@refs/heads/main",
	} {
		if getenv(key) != want {
			return nil, nil, errors.New("protected Email recovery dispatch is unverified")
		}
	}
	confirm := getenv("EMAIL_DATA_RECOVERY_CONFIRM")
	if confirm != "verify-email-data-recovery" && confirm != "verify-email-data-recovery-preflight" {
		return nil, nil, errors.New("protected Email recovery dispatch is unverified")
	}
	if !sourcePattern.MatchString(getenv("GITHUB_SHA")) || !runPattern.MatchString(getenv("GITHUB_RUN_ID")) {
		return nil, nil, errors.New("email recovery source and run are required")
	}
	path := getenv("KUBECONFIG")
	if !emailRecoveryCredentialPath.MatchString(path) || filepath.Clean(path) != path {
		return nil, nil, errors.New("fixed private Email recovery kubeconfig is required")
	}
	raw, err := readPrivateKubeconfig(path)
	if err != nil {
		return nil, nil, err
	}
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, nil, errors.New("protected Email recovery kubeconfig could not be decoded")
	}
	return newProductionClient(config)
}

// LoadBenchmarkAdmissionClient admits the protected benchmark, restore or
// explicitly confirmed CNPG name recovery. The caller must first join the
// selected address to its serving process and verified Hetzner server.
func LoadBenchmarkAdmissionClient(getenv func(string) string, directIP netip.Addr, originalRun string) (*http.Client, *url.URL, error) {
	operation, confirmation := "benchmark-v1", "benchmark-longhorn-storage"
	database := ""
	switch {
	case originalRun != "":
		if !runPattern.MatchString(originalRun) || originalRun == getenv("GITHUB_RUN_ID") {
			return nil, nil, errors.New("protected benchmark admission dispatch is unverified")
		}
		operation, confirmation = "recover-bound-v1-benchmark", "recover-bound-longhorn-v1-benchmark:"+originalRun
		switch getenv("LONGHORN_V2_OPERATION") {
		case "reclaim-native-v2-seed":
			operation, confirmation = "reclaim-native-v2-seed", "reclaim-longhorn-native-v2-seed:"+originalRun
		case "resume-native-v2-reclaim":
			prefix := "resume-longhorn-native-v2-reclaim:" + originalRun + ":"
			prior, ok := strings.CutPrefix(getenv("LONGHORN_V2_CONFIRM"), prefix)
			if !ok || !runPattern.MatchString(prior) || prior == originalRun || prior == getenv("GITHUB_RUN_ID") {
				return nil, nil, errors.New("protected benchmark admission dispatch is unverified")
			}
			operation, confirmation = "resume-native-v2-reclaim", prefix+prior
		case "recover-umami-standby-name", "recover-wedding-standby-name", "recover-provider-cnpg-standby-name":
			operation = getenv("LONGHORN_V2_OPERATION")
			database = strings.TrimSuffix(strings.TrimPrefix(operation, "recover-"), "-standby-name") + "-db"
			if operation == "recover-provider-cnpg-standby-name" {
				database = getenv("LONGHORN_V2_DATABASE")
				if database != "customer-identity-db" && database != "business-site-portal-db" {
					return nil, nil, errors.New("protected provider CNPG database is unverified")
				}
			}
			confirmation = getenv("LONGHORN_V2_CONFIRM")
			fields := cnpgNameRecoveryPattern.FindStringSubmatch(confirmation)
			if fields == nil || fields[1] != operation || fields[2] != database || fields[3] != originalRun {
				return nil, nil, errors.New("protected CNPG name recovery dispatch is unverified")
			}
		}
	case getenv("LONGHORN_V2_OPERATION") == "prepare-native-v1-seed":
		operation, confirmation = "prepare-native-v1-seed", "prepare-longhorn-native-v1-seed"
	case getenv("LONGHORN_V2_OPERATION") == "migrate-coroot-keeper-v2":
		operation, confirmation = "migrate-coroot-keeper-v2", getenv("LONGHORN_V2_CONFIRM")
		if !corootKeeperCutoverConfirm.MatchString(confirmation) {
			return nil, nil, errors.New("protected Keeper admission dispatch is unverified")
		}
	}
	if operation != "benchmark-v1" && (getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != database) {
		return nil, nil, errors.New("protected benchmark admission dispatch is unverified")
	}
	for key, want := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF":   "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"LONGHORN_V2_CONFIRM":   confirmation,
		"LONGHORN_V2_OPERATION": operation,
	} {
		if getenv(key) != want {
			return nil, nil, errors.New("protected benchmark admission dispatch is unverified")
		}
	}
	path := getenv("KUBECONFIG")
	if !sourcePattern.MatchString(getenv("GITHUB_SHA")) || !runPattern.MatchString(getenv("GITHUB_RUN_ID")) ||
		!benchmarkCredentialPath.MatchString(path) || filepath.Dir(path) != getenv("LONGHORN_V2_CREDENTIAL_DIR") ||
		!directIP.Is4() || !directIP.IsGlobalUnicast() || directIP.IsPrivate() {
		return nil, nil, errors.New("fixed benchmark admission credentials and direct IPv4 peer are required")
	}
	raw, err := readPrivateKubeconfig(path)
	if err != nil {
		return nil, nil, err
	}
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, nil, errors.New("protected benchmark kubeconfig could not be decoded")
	}
	var scope directCreateScope
	switch operation {
	case "reclaim-native-v2-seed", "resume-native-v2-reclaim":
		scope = directReclaimCreate
	case "recover-umami-standby-name":
		scope = directUmamiCreate
	case "recover-wedding-standby-name":
		scope = directWeddingCreate
	case "recover-provider-cnpg-standby-name":
		scope = directIdentityCreate
		if database == "business-site-portal-db" {
			scope = directPortalCreate
		}
	case "migrate-coroot-keeper-v2":
		scope = directKeeperCreate
	}
	return newProductionClientAt(config, directIP, scope)
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
	return newProductionClientAt(raw, netip.Addr{}, "")
}

type directCreateScope string

const (
	directReclaimCreate  directCreateScope = "reclaim"
	directUmamiCreate    directCreateScope = "umami"
	directWeddingCreate  directCreateScope = "wedding-app"
	directKeeperCreate   directCreateScope = "keeper"
	directIdentityCreate directCreateScope = "customer-identity"
	directPortalCreate   directCreateScope = "business-site"
)

var corootKeeperCutoverConfirm = regexp.MustCompile(`^migrate-coroot-keeper-v2:[0-2]:[a-f0-9]{64}$`)

func newProductionClientAt(raw *clientcmdapi.Config, directIP netip.Addr, scope directCreateScope) (*http.Client, *url.URL, error) {
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
	if directIP.IsValid() {
		if !directIP.Is4() || !directIP.IsGlobalUnicast() || directIP.IsPrivate() {
			return nil, nil, errors.New("benchmark direct API address is unverified")
		}
		endpoint.Host = net.JoinHostPort(directIP.String(), "6443")
		config.Host, config.ServerName = endpoint.String(), directIP.String()
	}
	client, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, nil, errors.New("verified production HTTP transport could not be constructed")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if directIP.IsValid() {
		client.Transport = directAPIPeer{host: endpoint.Host, transport: client.Transport, createScope: scope}
	}
	return client, endpoint, nil
}

type directAPIPeer struct {
	host        string
	transport   http.RoundTripper
	createScope directCreateScope
}

func (peer directAPIPeer) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != peer.host || req.URL.User != nil || req.URL.Opaque != "" ||
		(req.Host != "" && req.Host != peer.host) {
		return nil, errors.New("direct API request addressed another peer")
	}
	create := req.Method == http.MethodPost && req.URL.RawPath == "" &&
		req.Header.Get("Content-Type") == "application/json" && peer.createScope.permits(req.URL.Path)
	patch := req.Method == http.MethodPatch && (peer.createScope == "" || peer.createScope == directReclaimCreate)
	if req.Method != http.MethodGet && (req.URL.RawQuery != "dryRun=All" || (!patch && !create)) {
		return nil, errors.New("direct API client permits only observations and server dry-runs")
	}
	return peer.transport.RoundTrip(req)
}

func (scope directCreateScope) permits(path string) bool {
	switch scope {
	case directReclaimCreate:
		return path == "/api/v1/persistentvolumes" || path == "/api/v1/namespaces/longhorn-benchmark/persistentvolumeclaims"
	case directUmamiCreate, directWeddingCreate, directIdentityCreate, directPortalCreate:
		base := "/api/v1/namespaces/" + string(scope)
		return path == base+"/pods" || path == base+"/persistentvolumeclaims" || path == "/apis/batch/v1/namespaces/"+string(scope)+"/jobs"
	case directKeeperCreate:
		return path == "/api/v1/namespaces/observability/pods" || path == "/api/v1/namespaces/observability/persistentvolumeclaims"
	default:
		return false
	}
}

func (peer directAPIPeer) CloseIdleConnections() {
	if transport, ok := peer.transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}
