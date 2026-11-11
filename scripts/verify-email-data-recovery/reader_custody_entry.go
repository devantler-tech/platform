package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var errReaderInputRead = errors.New("reader-input-read")

type readerInputKubernetes struct{ inner kubernetes }

// This command has only fixed native reads. Lifecycle operations cannot borrow
// its credential-custody authority to mutate a Pod or persistent object.
func (k readerInputKubernetes) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if k.inner == nil || len(input) != 0 || (len(args) != 5 && len(args) != 6) || args[0] != "get" || args[3] != "--ignore-not-found=true" || args[4] != "-o=json" {
		return nil, errReaderInputRead
	}
	namespace := ""
	if len(args) == 6 {
		if !strings.HasPrefix(args[5], "--namespace=") {
			return nil, errReaderInputRead
		}
		namespace = strings.TrimPrefix(args[5], "--namespace=")
	}
	name, allowed := args[2], false
	switch args[1] {
	case "Pod":
		allowed = (namespace == retainedRecovery.namespace && name == "reader") || (namespace == "private-mail" && name == "private-mail-0")
	case "PersistentVolumeClaim":
		allowed = (namespace == retainedRecovery.namespace && name == "data") || (namespace == "private-mail" && name == "private-mail-data")
	case "PersistentVolume":
		allowed = namespace == "" && strings.HasPrefix(name, "pvc-") && kubernetesName.MatchString(name)
	case "Namespace", "StorageClass":
		allowed = namespace == "" && name == retainedRecovery.namespace
	case "CiliumNetworkPolicy":
		allowed = namespace == retainedRecovery.namespace && name == "isolation"
	case "ConfigMap":
		allowed = namespace == retainedRecovery.namespace && name == "datastore"
	case "Secret":
		allowed = namespace == retainedRecovery.namespace && (name == "storage-key" || name == "test-authentication")
	}
	if !allowed {
		return nil, errReaderInputRead
	}
	return k.inner.call(ctx, nil, args...)
}

func authorizeReaderInputCustody(env func(string) string) error {
	for key, expected := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform",
		"GITHUB_ACTOR": "devantler", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF":         "devantler-tech/platform/.github/workflows/prepare-retained-email-reader-inputs.yaml@refs/heads/main",
		"EMAIL_READER_INPUTS_CONFIRM": "prepare-retained-email-reader-inputs",
	} {
		if env(key) != expected {
			return errors.New("reader-input-dispatch")
		}
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reader, claim := env("EMAIL_RETAINED_READER_UID"), env("EMAIL_RETAINED_CLAIM_UID")
	if !recoveryRunPattern.MatchString(env("GITHUB_RUN_ID")) || !recoveryRevisionPattern.MatchString(env("GITHUB_SHA")) || !uuid.MatchString(reader) || !uuid.MatchString(claim) || reader == claim {
		return errors.New("reader-input-dispatch")
	}
	return nil
}

func runReaderInputCustody(ctx context.Context, env func(string) string, k kubernetes, verifyMain func(context.Context, string) error, loadStore func(func(string) string) (*readerCustodyStore, error), output io.Writer) int {
	stage := "dispatch"
	fail := func() int {
		if output != nil {
			_, _ = fmt.Fprintf(output, "EMAIL_READER_INPUT_CUSTODY=UNVERIFIED stage=%s restart=NOT_AUTHORIZED recovery=UNVERIFIED\n", stage)
		}
		return 2
	}
	if output == nil || verifyMain == nil || loadStore == nil || authorizeReaderInputCustody(env) != nil {
		return fail()
	}
	if _, err := fmt.Fprintln(output, "EMAIL_READER_INPUT_CUSTODY=STARTED source_writes=0 restart=NOT_AUTHORIZED recovery=UNVERIFIED"); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	stage = "source"
	if verifyMain(ctx, env("GITHUB_SHA")) != nil {
		return fail()
	}
	d := &recoveryDriver{i: retainedRecovery, k: readerInputKubernetes{inner: k}}
	var err error
	d.source, err = d.readSource(ctx)
	if err != nil {
		return fail()
	}
	reader, claim := env("EMAIL_RETAINED_READER_UID"), env("EMAIL_RETAINED_CLAIM_UID")
	stage = "inputs"
	original, err := d.captureCustodyInputs(ctx, reader, claim)
	if err != nil {
		return fail()
	}
	stage = "credentials"
	store, err := loadStore(env)
	if err != nil || store == nil || store.client == nil {
		return fail()
	}
	defer store.client.CloseIdleConnections()
	verify := func(ctx context.Context) error {
		// Rebind reviewed source before the last native reads. The store calls
		// this after its final non-destructive preflight, just before CAS=0.
		if verifyMain(ctx, env("GITHUB_SHA")) != nil {
			return errors.New("custody-source-changed")
		}
		fresh, err := d.captureCustodyInputs(ctx, reader, claim)
		if err != nil || !reflect.DeepEqual(original, fresh) {
			return errors.New("custody-inputs-changed")
		}
		return nil
	}
	stage = "custody"
	if store.ensure(ctx, original, verify) != nil {
		return fail()
	}
	if _, err := fmt.Fprintln(output, "EMAIL_READER_INPUT_CUSTODY=STORED source_writes=0 restart=NOT_AUTHORIZED recovery=UNVERIFIED"); err != nil {
		return 2
	}
	return 0
}

func (d *recoveryDriver) captureCustodyInputs(ctx context.Context, readerUID, claimUID string) (retainedReaderInputs, error) {
	fail := func() (retainedReaderInputs, error) { return retainedReaderInputs{}, errors.New("reader-inputs") }
	reader, found, err := d.lookup(ctx, "Pod", "reader", retainedRecovery.namespace)
	if err != nil || !found || text(child(reader, "metadata"), "uid") != readerUID {
		return fail()
	}
	claim, found, err := d.lookup(ctx, "PersistentVolumeClaim", "data", retainedRecovery.namespace)
	if err != nil || !found || text(child(claim, "metadata"), "uid") != claimUID || claimUID == d.source.claimUID || text(child(claim, "spec"), "volumeName") == d.source.pv {
		return fail()
	}
	var graph []object
	for _, want := range append(recoveryResources(retainedRecovery, "", ""), recoveryReader(retainedRecovery, d.image)) {
		m := child(want, "metadata")
		actual := reader
		switch text(want, "kind") {
		case "Pod":
		case "PersistentVolumeClaim":
			actual = claim
		default:
			actual, found, err = d.lookup(ctx, text(want, "kind"), text(m, "name"), text(m, "namespace"))
			if err != nil || !found {
				return fail()
			}
		}
		graph = append(graph, actual)
	}
	bundle, err := captureRetainedReaderInputs(retainedRecovery, d.image, readerUID, claimUID, d.source.claimUID, graph)
	if err != nil || d.sourceRetained(ctx) != nil {
		return fail()
	}
	return bundle, nil
}
