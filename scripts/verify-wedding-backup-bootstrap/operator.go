package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func prove(k client, r recipe) (err error) {
	ns := "wedding-bootstrap-" + k.run
	fmt.Fprintln(os.Stderr, "Bootstrap proof: checking ownership and creating isolated services")
	// Even matching labels cannot adopt a previous fixture: run IDs are one-use.
	for _, target := range [][3]string{{"", "namespaces", ns}, {"external-secrets", "ciliumnetworkpolicies", ns}} {
		o, e := k.get(target[0], target[1], target[2])
		if e != nil || o != nil {
			return refused
		}
	}
	access, e := randomCredential(12)
	if e != nil {
		return e
	}
	password, e := randomCredential(24)
	if e != nil {
		return e
	}
	items, e := fixture(k.run, r, access, password)
	if e != nil {
		return e
	}
	if e = k.create(items[0]); e != nil {
		return e
	}
	var nsUID string
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		clean := k
		clean.ctx = ctx
		if cleanup(clean, nsUID) != nil {
			err = refused
		}
	}()
	n, e := k.get("", "namespaces", ns)
	if e != nil || !owned(n, ns, "", k.run) {
		return refused
	}
	nsUID = str(n, "metadata", "uid")
	// Install isolation before a Pod can start.
	for _, o := range items[1 : len(items)-2] {
		if str(o, "kind") == "CiliumNetworkPolicy" {
			if e = k.create(o); e != nil {
				return e
			}
		}
	}
	for _, o := range items[1 : len(items)-2] {
		if str(o, "kind") != "CiliumNetworkPolicy" {
			if e = k.create(o); e != nil {
				return e
			}
		}
	}
	policy, e := esoPolicy(k.run, nsUID)
	if e != nil {
		return e
	}
	if e = k.create(policy); e != nil {
		return e
	}
	for _, name := range []string{"openbao", "minio"} {
		if e = waitFor(k, func() (bool, error) { o, e := k.get(ns, "pods", name); return ready(o), e }); e != nil {
			return refused
		}
	}
	url, stop, e := forward(k.ctx, k.run)
	if e != nil {
		return e
	}
	defer stop()
	b := bao{url: url, ctx: k.ctx, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return refused }}}
	token, e := b.initialize()
	if e != nil {
		return e
	}
	fmt.Fprintln(os.Stderr, "Bootstrap proof: empty OpenBao initialized; waiting for credential delivery")
	if _, e = b.request("GET", "/v1/secret/metadata/"+dedicatedPath, token, nil, 404); e != nil {
		return e
	}
	if _, e = b.request("GET", "/v1/secret/data/infrastructure/backup/r2", token, nil, 403); e != nil {
		return e
	}
	if e = k.create(object{"apiVersion": "v1", "kind": "Secret", "metadata": meta("fixture-openbao-token", ns, k.run), "stringData": object{"token": token}}); e != nil {
		return e
	}
	if e = k.create(object{"apiVersion": "external-secrets.io/v1", "kind": "SecretStore", "metadata": meta("fixture-openbao", ns, k.run), "spec": object{"provider": object{"vault": object{"server": "http://openbao." + ns + ".svc.cluster.local:8200", "path": "secret", "version": "v2", "auth": object{"tokenSecretRef": object{"name": "fixture-openbao-token", "key": "token"}}}}}}); e != nil {
		return e
	}
	if e = waitFor(k, func() (bool, error) { s, e := k.get(ns, "secretstores", "fixture-openbao"); return ready(s), e }); e != nil {
		return e
	}
	for _, o := range items[len(items)-2:] {
		if e = k.create(o); e != nil {
			return e
		}
	}
	var currentSecret, currentPush, currentExternal object
	check := func() (bool, error) {
		push, e := k.get(ns, "pushsecrets", "seed-wedding-db-backup-r2")
		if e != nil {
			return false, e
		}
		pull, e := k.get(ns, "externalsecrets", projectedSecret)
		if e != nil {
			return false, e
		}
		s, e := k.get(ns, "secrets", projectedSecret)
		if e != nil {
			return false, e
		}
		if !ready(push) || !ready(pull) || !projection(s, pull, k.run, access, password) {
			return false, nil
		}
		kv, e := b.request("GET", "/v1/secret/data/"+dedicatedPath, token, nil, 200)
		if e != nil {
			return false, nil
		}
		data, _ := at(kv, "data", "data").(map[string]any)
		if len(data) != 2 || str(data, "access_key_id") != access || str(data, "secret_access_key") != password {
			return false, refused
		}
		currentSecret, currentPush, currentExternal = s, push, pull
		return true, nil
	}
	if e = waitFor(k, check); e != nil {
		return e
	}
	initialUID := str(currentSecret, "metadata", "uid")
	initialTime, e := time.Parse(time.RFC3339Nano, str(currentPush, "status", "refreshTime"))
	if e != nil {
		return refused
	}
	source, e := k.get(ns, "secrets", "wedding-db-backup-r2-bootstrap")
	if e != nil || !owned(source, "wedding-db-backup-r2-bootstrap", ns, k.run) {
		return refused
	}
	initialSource := source
	fmt.Fprintln(os.Stderr, "Bootstrap proof: initial delivery verified; removing fixture copies")
	if _, e = b.request("DELETE", "/v1/secret/metadata/"+dedicatedPath, token, nil, 204); e != nil {
		return e
	}
	if _, e = b.request("GET", "/v1/secret/metadata/"+dedicatedPath, token, nil, 404); e != nil {
		return e
	}
	if e = removeOwned(k, ns, "secrets", projectedSecret, initialUID); e != nil {
		return e
	}
	// Only the periodic controller loop repairs it: no force-sync or source update.
	if e = waitFor(k, func() (bool, error) {
		ok, e := check()
		if !ok || e != nil {
			return false, e
		}
		refresh, e := time.Parse(time.RFC3339Nano, str(currentPush, "status", "refreshTime"))
		if e != nil {
			return false, refused
		}
		return str(currentSecret, "metadata", "uid") != initialUID && refresh.After(initialTime) && uuid.MatchString(str(currentExternal, "metadata", "uid")), nil
	}); e != nil {
		return e
	}
	source, e = k.get(ns, "secrets", "wedding-db-backup-r2-bootstrap")
	if e != nil || !unchangedSource(initialSource, source) {
		return refused
	}
	fmt.Fprintln(os.Stderr, "Bootstrap proof: periodic repair verified; testing projected credentials")
	p, e := probePod(k.run, r)
	if e != nil {
		return e
	}
	server, e := k.get(ns, "pods", "minio")
	if e != nil || !unchangedPeerServer(server, server, k.run) || bindPeerAddress(p, server, k.run) != nil {
		return refused
	}
	if e = k.create(p); e != nil {
		return e
	}
	return waitFor(k, func() (bool, error) {
		o, e := k.get(ns, "pods", "storage-proof")
		if e != nil {
			return false, e
		}
		switch str(o, "status", "phase") {
		case "Succeeded":
			after, e := k.get(ns, "pods", "minio")
			if e != nil || !unchangedPeerServer(server, after, k.run) {
				return false, refused
			}
			return true, nil
		case "Failed":
			return false, refused
		}
		return false, nil
	})
}

func main() {
	run, err := invocation(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Bootstrap proof refused: protected dispatch required")
		os.Exit(1)
	}
	mode := "proof"
	if len(os.Args) == 2 && os.Args[1] == "--cleanup" {
		mode = "cleanup"
	} else if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "Bootstrap proof refused: unsupported argument")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 28*time.Minute)
	defer cancel()
	k := newClient(ctx, run)
	if mode == "cleanup" {
		err = cleanup(k, "")
	} else {
		var r recipe
		r, err = loadRecipe(".")
		if err == nil {
			err = prove(k, r)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Bootstrap proof failed; no credential or provider response is logged")
		os.Exit(1)
	}
	receipt := object{"runId": run, "sourceSha": os.Getenv("GITHUB_SHA"), "cleanupVerified": true}
	if mode == "proof" {
		receipt["emptyOpenBaoSeedVerified"] = true
		receipt["periodicReseedVerified"] = true
		receipt["projectedCredentialStorageVerified"] = true
	}
	_ = json.NewEncoder(os.Stdout).Encode(receipt)
}
