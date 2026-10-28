package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"regexp"
	"time"
)

const maxAccessOutput = 65536

//go:embed access_probe.sh
var accessProbe string

type inputExecutor func(context.Context, []byte, ...string) ([]byte, error)
type accessResult struct {
	Paths, Access, CiliumAccess string
	Block                       blockDetails
	Ready                       bool
	Ingress                     *ingressAccessResult
}
type accessObservation struct {
	LiveLoopback  *int            `json:"live_loopback"`
	ReadyLoopback *int            `json:"ready_loopback"`
	LivePod       *int            `json:"live_pod"`
	ReadyPod      *int            `json:"ready_pod"`
	Security      json.RawMessage `json:"security"`
}
type credentialSecret struct {
	Metadata metadata
	Data     map[string][]byte
}
type secretBinding struct {
	Metadata metadata
	Digest   string
}
type accessAddresses struct{ Pod, Host netip.Addr }

var readerName = regexp.MustCompile(`^[a-z][a-z0-9._+-]{0,63}$`)
var readerDomain = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,251}[a-z0-9]$`)
var readerPassword = regexp.MustCompile(`^[a-zA-Z0-9]{40,128}$`)
var supportedKubelet = regexp.MustCompile(`^v1\.36\.[0-9]+$`)
var nodeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

type probeNode struct{ UID, Version string }

type ciliumCandidate struct {
	UID, ResourceVersion string
	Address              netip.Addr
}

// bindCiliumCandidate binds one local Cilium address to the already bound Node.
// It identifies a routing candidate, not an observed connection source.
func bindCiliumCandidate(raw []byte, name, nodeUID string, addresses accessAddresses) (ciliumCandidate, error) {
	var n struct {
		Metadata struct {
			Name, UID, ResourceVersion string
			OwnerReferences            []struct{ APIVersion, Kind, Name, UID string }
		}
		Spec struct{ Addresses []struct{ Type, IP string } }
	}
	bad := errors.New("invalid Cilium source candidate")
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &n) != nil || n.Metadata.Name != name || !uuidPattern.MatchString(n.Metadata.UID) || n.Metadata.ResourceVersion == "" || len(n.Metadata.OwnerReferences) != 1 {
		return ciliumCandidate{}, bad
	}
	owner := n.Metadata.OwnerReferences[0]
	if owner.APIVersion != "v1" || owner.Kind != "Node" || owner.Name != name || owner.UID != nodeUID {
		return ciliumCandidate{}, bad
	}
	var local netip.Addr
	hostCount, localCount := 0, 0
	for _, entry := range n.Spec.Addresses {
		switch entry.Type {
		case "InternalIP", "CiliumInternalIP":
			ip, e := netip.ParseAddr(entry.IP)
			if e != nil || !ip.IsGlobalUnicast() || ip.Is4In6() || ip.Zone() != "" || ip.Is4() != addresses.Pod.Is4() {
				return ciliumCandidate{}, bad
			}
			if entry.Type == "InternalIP" {
				hostCount++
				if ip != addresses.Host {
					return ciliumCandidate{}, bad
				}
			} else {
				localCount++
				local = ip
			}
		}
	}
	if hostCount != 1 || localCount != 1 || local == addresses.Pod || local == addresses.Host {
		return ciliumCandidate{}, bad
	}
	return ciliumCandidate{UID: n.Metadata.UID, ResourceVersion: n.Metadata.ResourceVersion, Address: local}, nil
}

// bindProbeNode matches the real kubelet minor that supplies kube-probe/1.36.
func bindProbeNode(raw []byte, name string) (probeNode, error) {
	var n struct {
		Metadata metadata
		Status   struct {
			NodeInfo struct{ KubeletVersion string }
		}
	}
	if !nodeName.MatchString(name) || uniqueJSON(raw) != nil || json.Unmarshal(raw, &n) != nil || n.Metadata.Name != name || !uuidPattern.MatchString(n.Metadata.UID) || !supportedKubelet.MatchString(n.Status.NodeInfo.KubeletVersion) {
		return probeNode{}, errors.New("unsupported probe node")
	}
	return probeNode{UID: n.Metadata.UID, Version: n.Status.NodeInfo.KubeletVersion}, nil
}

// uniqueJSON refuses duplicate keys, truncated input and excessive nesting.
func uniqueJSON(b []byte) error {
	if len(b) == 0 || len(b) > maxAPIOutput {
		return errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON depth")
		}
		token, e := d.Token()
		if e != nil {
			return e
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate key")
				}
				seen[name] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		default:
			return errors.New("invalid delimiter")
		}
		end, e := d.Token()
		if e != nil {
			return e
		}
		if (delimiter == '{' && end != json.Delim('}')) || (delimiter == '[' && end != json.Delim(']')) {
			return errors.New("invalid end")
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// strictJSON additionally rejects fields outside the fixed observation schema.
func strictJSON(b []byte, target any) error {
	if e := uniqueJSON(b); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

// bindAddresses binds both targets and the two existing credential projections.
func bindAddresses(raw []byte) (accessAddresses, error) {
	var p struct {
		Status struct{ PodIP, HostIP string }
		Spec   struct {
			Volumes []struct {
				Name      string
				Projected *struct {
					Sources []struct {
						Secret *struct {
							Name  string
							Items []struct{ Key, Path string }
						}
					}
				}
			}
		}
	}
	bad := errors.New("invalid addresses or input projections")
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &p) != nil {
		return accessAddresses{}, bad
	}
	podIP, e := netip.ParseAddr(p.Status.PodIP)
	if e != nil || !podIP.IsGlobalUnicast() {
		return accessAddresses{}, bad
	}
	hostIP, e := netip.ParseAddr(p.Status.HostIP)
	if e != nil || !hostIP.IsGlobalUnicast() {
		return accessAddresses{}, bad
	}
	foundSettings, foundAdmin := false, false
	for _, v := range p.Spec.Volumes {
		if v.Name != "private-inputs" || v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.Secret == nil {
				continue
			}
			for _, item := range src.Secret.Items {
				if src.Secret.Name == "private-mail-settings" && item.Key == "settings.json" && item.Path == "settings.json" {
					foundSettings = true
				}
				if src.Secret.Name == "private-mail-admin" && item.Key == "password" && item.Path == "administrator-password" {
					foundAdmin = true
				}
			}
		}
	}
	if !foundSettings || !foundAdmin {
		return accessAddresses{}, bad
	}
	return accessAddresses{Pod: podIP.Unmap(), Host: hostIP.Unmap()}, nil
}

// decodeSecret reads exactly one named existing input; no value leaves memory.
func decodeSecret(raw []byte, name, key string) ([]byte, secretBinding, error) {
	var s credentialSecret
	bad := errors.New("invalid credential input")
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &s) != nil ||
		s.Metadata.Name != name || s.Metadata.Namespace != "private-mail" || !uuidPattern.MatchString(s.Metadata.UID) || s.Metadata.ResourceVersion == "" ||
		len(s.Data[key]) == 0 || len(s.Data[key]) > 4096 {
		return nil, secretBinding{}, bad
	}
	switch {
	case name == "private-mail-settings" && key == "settings.json":
		if len(s.Data) != 1 {
			return nil, secretBinding{}, bad
		}
	case name == "private-mail-admin" && key == "password":
		if len(s.Data) != 2 || len(s.Data["credential"]) == 0 || len(s.Data["credential"]) > 4096 {
			return nil, secretBinding{}, bad
		}
	default:
		return nil, secretBinding{}, bad
	}
	return s.Data[key], secretBinding{Metadata: s.Metadata, Digest: hashBytes(s.Data[key])}, nil
}

// readerAuthorization accepts the same narrow administrative identity shape as setup.
func readerAuthorization(settings, password []byte) (string, error) {
	var s struct {
		Domain, Hostname, Maintainer, Agent, Administrator string
		MaintainerAliases                                  []string
	}
	if strictJSON(settings, &s) != nil || !readerName.MatchString(s.Administrator) ||
		!readerDomain.MatchString(s.Domain) || !readerPassword.Match(password) {
		return "", errors.New("invalid reader identity")
	}
	return base64.StdEncoding.EncodeToString(append([]byte(s.Administrator+"@"+s.Domain+":"), password...)), nil
}

type addressQuery struct {
	AccountID           string    `json:"accountId"`
	QueryState          string    `json:"queryState"`
	CanCalculateChanges *bool     `json:"canCalculateChanges"`
	Position            *int      `json:"position"`
	IDs                 *[]string `json:"ids"`
	Total               *int      `json:"total"`
}

// completeQueries proves each requested page contains the complete bounded ID set.
func completeQueries(responses [][]json.RawMessage) (map[string]addressQuery, error) {
	bad := errors.New("incomplete address query")
	queries := map[string]addressQuery{}
	for _, response := range responses {
		if len(response) != 3 {
			return nil, bad
		}
		var method, tag string
		if json.Unmarshal(response[0], &method) != nil || json.Unmarshal(response[2], &tag) != nil {
			return nil, bad
		}
		switch tag {
		case "allow", "block":
			continue
		case "allowIds", "blockIds":
			key := "allow"
			expected := "x:AllowedIp/query"
			if tag == "blockIds" {
				key = "block"
				expected = "x:BlockedIp/query"
			}
			if _, exists := queries[key]; exists || method != expected {
				return nil, bad
			}
			var q addressQuery
			if strictJSON(response[1], &q) != nil || q.AccountID == "" || len(q.AccountID) > 128 || q.QueryState == "" || q.CanCalculateChanges == nil || q.Position == nil || *q.Position != 0 || q.IDs == nil || q.Total == nil || *q.Total < 0 || *q.Total > 256 || len(*q.IDs) != *q.Total {
				return nil, bad
			}
			seen := map[string]bool{}
			for _, id := range *q.IDs {
				if id == "" || len(id) > 128 || seen[id] {
					return nil, bad
				}
				seen[id] = true
			}
			queries[key] = q
		default:
			return nil, bad
		}
	}
	if len(queries) != 2 {
		return nil, bad
	}
	return queries, nil
}

// effectiveHostBlock evaluates only the observed host candidate, never every probe source.
func effectiveHostBlock(raw []byte, host netip.Addr, now time.Time) (string, error) {
	var envelope struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
		SessionState    *string             `json:"sessionState"`
	}
	bad := errors.New("incomplete security state")
	if len(raw) > maxAccessOutput || strictJSON(raw, &envelope) != nil || envelope.SessionState == nil || *envelope.SessionState == "" || len(*envelope.SessionState) > 512 || len(envelope.MethodResponses) != 4 {
		return "", bad
	}
	queries, queryError := completeQueries(envelope.MethodResponses)
	if queryError != nil {
		return "", bad
	}
	matched := map[string]bool{}
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, response := range envelope.MethodResponses {
		if len(response) != 3 {
			return "", bad
		}
		var method, tag string
		if json.Unmarshal(response[2], &tag) == nil && (tag == "allowIds" || tag == "blockIds") {
			continue
		}
		if json.Unmarshal(response[0], &method) != nil || json.Unmarshal(response[2], &tag) != nil || (tag != "allow" && tag != "block") || seen[tag] {
			return "", bad
		}
		seen[tag] = true
		expected := "x:AllowedIp/get"
		if tag == "block" {
			expected = "x:BlockedIp/get"
		}
		if method != expected {
			return "", bad
		}
		var body struct {
			AccountID string             `json:"accountId"`
			List      *[]json.RawMessage `json:"list"`
			NotFound  *[]string          `json:"notFound"`
			State     *string            `json:"state,omitempty"`
		}
		query := queries[tag]
		if strictJSON(response[1], &body) != nil || body.AccountID != query.AccountID || body.List == nil || body.NotFound == nil || len(*body.NotFound) != 0 || len(*body.List) != len(*query.IDs) {
			return "", bad
		}
		remaining := map[string]bool{}
		for _, id := range *query.IDs {
			remaining[id] = true
		}
		for _, entry := range *body.List {
			var value struct {
				ID        *string `json:"id,omitempty"`
				Address   string  `json:"address"`
				ExpiresAt *string `json:"expiresAt"`
				Reason    *string `json:"reason,omitempty"`
				CreatedAt *string `json:"createdAt,omitempty"`
			}
			var fields map[string]json.RawMessage
			if strictJSON(entry, &value) != nil || json.Unmarshal(entry, &fields) != nil || value.Address == "" || value.ID == nil || !remaining[*value.ID] {
				return "", bad
			}
			delete(remaining, *value.ID)
			if tag == "allow" && (fields["reason"] != nil || fields["createdAt"] != nil) {
				return "", bad
			}
			if _, ok := fields["expiresAt"]; !ok {
				return "", bad
			}
			var network netip.Prefix
			if ip, e := netip.ParseAddr(value.Address); e == nil {
				if ip.Is4In6() {
					return "", bad
				}
				ip = ip.Unmap()
				network = netip.PrefixFrom(ip, ip.BitLen())
			} else {
				var e error
				network, e = netip.ParsePrefix(value.Address)
				if e != nil || network.Addr().Is4In6() {
					return "", bad
				}
			}
			if value.ExpiresAt != nil {
				expiry, e := time.Parse(time.RFC3339, *value.ExpiresAt)
				if e != nil || expiry.Unix() < 0 {
					return "", bad
				}
				if !expiry.After(now) {
					continue
				}
			}
			counts[tag]++
			if network.Contains(host) {
				matched[tag] = true
			}
		}
	}
	switch {
	case matched["block"] && !matched["allow"]:
		return "STORED_HOST_BLOCK_MATCH", nil
	case counts["block"] == 0:
		return "NO_ACTIVE_STORED_BLOCKS", nil
	case matched["allow"] && matched["block"]:
		return "STORED_HOST_ALLOW_OVERRIDE", nil
	default:
		return "OTHER_ACTIVE_STORED_BLOCKS", nil
	}
}

// decodeAccess requires all four status observations and complete native API results.
func decodeAccess(raw []byte, host netip.Addr, now time.Time) (accessResult, error) {
	var observation accessObservation
	bad := errors.New("invalid access observation")
	if len(raw) > maxAccessOutput || strictJSON(raw, &observation) != nil {
		return accessResult{}, bad
	}
	codes := []*int{observation.LiveLoopback, observation.ReadyLoopback, observation.LivePod, observation.ReadyPod}
	for _, c := range codes {
		if c == nil || (*c != 0 && (*c < 100 || *c > 599)) {
			return accessResult{}, bad
		}
	}
	access, e := effectiveHostBlock(observation.Security, host, now)
	if e != nil {
		return accessResult{}, bad
	}
	paths := "ALL_LOCAL_TARGETS_HTTP_200"
	if *codes[0] != 200 || *codes[1] != 200 {
		paths = "LOOPBACK_PATH_FAILED"
	} else if *codes[2] != 200 || *codes[3] != 200 {
		paths = "POD_ADDRESS_PATH_FAILED"
	}
	return accessResult{Paths: paths, Access: access}, nil
}

// collectAccess observes fixed resources once, with bounded waiting and before/after bindings.
func collectAccess(ctx context.Context, run executor, send inputExecutor, pause func(context.Context) error, now func() time.Time) (accessResult, error) {
	return collectAccessWithIngress(ctx, run, send, pause, now, nil)
}

func collectAccessWithIngress(ctx context.Context, run executor, send inputExecutor, pause func(context.Context) error, now func() time.Time, observe func(context.Context, binding, accessAddresses) (ingressObservation, error)) (accessResult, error) {
	stage := ingressFailure("pod_before")
	fail := func() (accessResult, error) { return accessResult{}, stage }
	var before binding
	var addresses accessAddresses
	var createdBefore time.Time
	found := false
	for attempt := 0; attempt < 11; attempt++ {
		raw, e := request(ctx, run, "get", "pod", "private-mail-0", "-n", "private-mail", "-o", "json")
		if e != nil {
			return fail()
		}
		id, running, _, e := bindPod(raw)
		if e != nil {
			return fail()
		}
		if running {
			createdBefore, e = podCreationTime(raw)
			if e != nil {
				return fail()
			}
			addresses, e = bindAddresses(raw)
			if e != nil {
				return fail()
			}
			before = id
			found = true
			break
		}
		if attempt < 10 && pause(ctx) != nil {
			return fail()
		}
	}
	if !found {
		return fail()
	}
	stage = "proxy_before"
	proxyBefore, e := proxyConfiguration(ctx, run, before)
	if e != nil {
		return fail()
	}
	stage = "node_before"
	if !nodeName.MatchString(before.Node) {
		return fail()
	}
	readNode := func() (probeNode, error) {
		raw, e := request(ctx, run, "get", "node", before.Node, "-o", "json")
		if e != nil {
			return probeNode{}, e
		}
		return bindProbeNode(raw, before.Node)
	}
	nodeBefore, e := readNode()
	if e != nil {
		return fail()
	}
	readCilium := func() (ciliumCandidate, error) {
		raw, e := request(ctx, run, "get", "ciliumnode", before.Node, "-o", "json")
		if e != nil {
			return ciliumCandidate{}, e
		}
		return bindCiliumCandidate(raw, before.Node, nodeBefore.UID, addresses)
	}
	stage = "cilium_before"
	ciliumBefore, e := readCilium()
	if e != nil {
		return fail()
	}
	readMap := func() (configMap, error) {
		raw, e := request(ctx, run, "get", "configmap", "private-mail-datastore", "-n", "private-mail", "-o", "json")
		if e != nil {
			return configMap{}, e
		}
		return readPublicMap(raw)
	}
	stage = "datastore_before"
	cm, e := readMap()
	if e != nil {
		return fail()
	}
	readSecret := func(name, key string) ([]byte, secretBinding, error) {
		raw, e := request(ctx, run, "get", "secret", name, "-n", "private-mail", "-o", "json")
		if e != nil {
			return nil, secretBinding{}, e
		}
		return decodeSecret(raw, name, key)
	}
	stage = "settings_before"
	settings, sb, e := readSecret("private-mail-settings", "settings.json")
	if e != nil {
		return fail()
	}
	stage = "credential_before"
	password, pb, e := readSecret("private-mail-admin", "password")
	if e != nil {
		return fail()
	}
	stage = "authorization"
	authorization, e := readerAuthorization(settings, password)
	if e != nil {
		return fail()
	}
	input := []byte(authorization + "\n" + addresses.Pod.String() + "\n")
	stage = "access_before"
	raw, e := send(ctx, input, "--context", "admin@prod", "--request-timeout=10s", "exec", "-i", "-n", "private-mail", "private-mail-0", "-c", "private-mail", "--", "/bin/bash", "--noprofile", "--norc", "-euo", "pipefail", "-c", accessProbe)
	defer clear(raw)
	clear(input)
	clear(password)
	if e != nil {
		return fail()
	}
	stage = "access_decode"
	observedAt := now()
	result, e := decodeAccess(raw, addresses.Host, observedAt)
	if e != nil {
		return fail()
	}
	ciliumResult, e := decodeAccess(raw, ciliumBefore.Address, observedAt)
	if e != nil {
		return fail()
	}
	result.CiliumAccess = ciliumResult.Access
	result.Block, e = decodeBlockDetails(raw, ciliumBefore.Address, createdBefore, observedAt)
	if e != nil {
		return fail()
	}
	switch result.CiliumAccess {
	case "STORED_HOST_BLOCK_MATCH":
		result.CiliumAccess = "STORED_CILIUM_BLOCK_MATCH"
	case "STORED_HOST_ALLOW_OVERRIDE":
		result.CiliumAccess = "STORED_CILIUM_ALLOW_OVERRIDE"
	}
	if observe != nil {
		observation, err := observe(ctx, before, addresses)
		if err != nil {
			return accessResult{}, ingressFailure(ingressFailureStage(err))
		}
		stage = "access_after"
		input := []byte(authorization + "\n" + addresses.Pod.String() + "\n")
		afterRaw, err := send(ctx, input, "--context", "admin@prod", "--request-timeout=10s", "exec", "-i", "-n", "private-mail", "private-mail-0", "-c", "private-mail", "--", "/bin/bash", "--noprofile", "--norc", "-euo", "pipefail", "-c", accessProbe)
		clear(input)
		defer clear(afterRaw)
		if err != nil {
			return fail()
		}
		stage = "access_compare"
		assessment, err := assessIngressAccess(raw, afterRaw, observation, observedAt, now())
		if err != nil {
			return fail()
		}
		result.Ingress = &assessment
	}
	stage = "pod_after"
	raw, e = request(ctx, run, "get", "pod", "private-mail-0", "-n", "private-mail", "-o", "json")
	if e != nil {
		return fail()
	}
	after, running, ready, e := bindPod(raw)
	if e != nil || !running || after != before {
		return fail()
	}
	stage = "proxy_after"
	proxyAfter, e := proxyConfiguration(ctx, run, after)
	if e != nil || proxyAfter != proxyBefore {
		return fail()
	}
	stage = "pod_after"
	afterAddresses, e := bindAddresses(raw)
	if e != nil || afterAddresses != addresses {
		return fail()
	}
	createdAfter, e := podCreationTime(raw)
	if e != nil || !createdAfter.Equal(createdBefore) {
		return fail()
	}
	stage = "node_after"
	nodeAfter, e := readNode()
	if e != nil || nodeAfter != nodeBefore {
		return fail()
	}
	stage = "cilium_after"
	ciliumAfter, e := readCilium()
	if e != nil || ciliumAfter != ciliumBefore {
		return fail()
	}
	stage = "datastore_after"
	afterMap, e := readMap()
	if e != nil || afterMap.Metadata != cm.Metadata || !reflect.DeepEqual(afterMap.Data, cm.Data) {
		return fail()
	}
	stage = "settings_after"
	_, afterSettings, e := readSecret("private-mail-settings", "settings.json")
	if e != nil || afterSettings != sb {
		return fail()
	}
	stage = "credential_after"
	afterPassword, afterAdmin, e := readSecret("private-mail-admin", "password")
	clear(afterPassword)
	if e != nil || afterAdmin != pb {
		return fail()
	}
	result.Ready = ready
	return result, nil
}

// runAccessEntry fails closed before any read, and emits only fixed classifications.
func runAccessEntry(ctx context.Context, env func(string) string, run executor, send inputExecutor, pause func(context.Context) error, out io.Writer) int {
	unknown := func() int { _, _ = io.WriteString(out, "MAIL_PROBE_STATE=INCONCLUSIVE\n"); return 3 }
	if env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || env("MAIL_ACCESS_CONFIRM") != "read-email-probe-state" || send == nil {
		return unknown()
	}
	result, e := collectAccess(ctx, run, send, pause, time.Now)
	if e != nil {
		return unknown()
	}
	ready := "FALSE"
	if result.Ready {
		ready = "TRUE"
	}
	line := fmt.Sprintf("MAIL_PROBE_PATHS=%s\nMAIL_ACCESS_STATE=%s\nMAIL_CILIUM_ACCESS_STATE=%s\nMAIL_CILIUM_BLOCK_REASON=%s\nMAIL_CILIUM_BLOCK_SCOPE=%s\nMAIL_CILIUM_BLOCK_EXPIRY=%s\nMAIL_CILIUM_BLOCK_CREATION=%s\nMAIL_KUBERNETES_READY=%s\n", result.Paths, result.Access, result.CiliumAccess, result.Block.Reason, result.Block.Scope, result.Block.Expiry, result.Block.Creation, ready)
	if n, e := io.WriteString(out, line); e != nil || n != len(line) {
		return 3
	}
	if result.Ready && result.Paths == "ALL_LOCAL_TARGETS_HTTP_200" && result.Access == "NO_ACTIVE_STORED_BLOCKS" {
		return 0
	}
	return 1
}
