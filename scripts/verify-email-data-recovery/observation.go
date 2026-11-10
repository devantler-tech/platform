package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"
)

// The retained invocation is fixed in reviewed code, never selected by workflow
// input, logs or cluster content. Observation never creates or removes data.
var retainedRecovery = invocation{
	run:       "41561215415",
	revision:  "7e4486cad02c326004ac9a1a637bb5205dbfe76a",
	namespace: "email-data-recovery-41561215415",
}

const retainedParityConfirm = "verify-retained-email-parity"

type observationReadOnly struct {
	base          kubernetes
	parity        bool
	servingVolume string
}

// call permits fixed reads and the existing account credential. Parity also
// reads the serving identity and its bound volume, named by the fixed claim.
// Input bytes, extra flags, other targets and every write are refused.
func (o observationReadOnly) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if len(input) != 0 {
		return nil, errors.New("observation-scope")
	}
	allowedReads := [][]string{
		{"get", "Namespace", retainedRecovery.namespace, "--ignore-not-found=true", "-o=json"},
		{"get", "Pod", "reader", "--ignore-not-found=true", "-o=json", "--namespace=" + retainedRecovery.namespace},
		{"get", "Secret", "private-mail-maintainer", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"},
	}
	if o.parity {
		allowedReads = append(allowedReads,
			[]string{"get", "Pod", "private-mail-0", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"},
			[]string{"get", "PersistentVolumeClaim", "private-mail-data", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"},
			[]string{"get", "PersistentVolumeClaim", "data", "--ignore-not-found=true", "-o=json", "--namespace=" + retainedRecovery.namespace},
		)
		if o.servingVolume != "" {
			allowedReads = append(allowedReads, []string{"get", "PersistentVolume", o.servingVolume, "--ignore-not-found=true", "-o=json"})
		}
	}
	for _, allowed := range allowedReads {
		if reflect.DeepEqual(args, allowed) {
			return o.base.call(ctx, nil, args...)
		}
	}
	return nil, errors.New("observation-scope")
}

// observationHealth samples the fixed readiness URL without credentials.
// Its body is ignored; the client's redirect and proxy refusals still apply.
func observationHealth(ctx context.Context, c *mailboxClient) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/healthz/ready", nil)
	if err != nil {
		return 0, errors.New("health-request")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return 0, errors.New("health-transport")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 100 || response.StatusCode > 599 {
		return 0, errors.New("health-status")
	}
	return response.StatusCode, nil
}

// runRecoveryObservation uses the same UID-fenced private forwarding path as
// recovery. Parity mode compares a diagnostic message without authorizing
// cleanup or establishing a fresh backup restore.
func runRecoveryObservation(ctx context.Context, env func(string) string, k kubernetes, output io.Writer) int {
	fail := func(stage string) int {
		_, _ = fmt.Fprintf(output, "EMAIL_RECOVERY_OBSERVATION=UNVERIFIED stage=%s\n", stage)
		return 2
	}
	confirm := env("EMAIL_DATA_RECOVERY_CONFIRM")
	parity := confirm == retainedParityConfirm
	if env("GITHUB_WORKFLOW_REF") != "devantler-tech/platform/.github/workflows/observe-email-data-recovery.yaml@refs/heads/main" || (!parity && confirm != "observe-email-data-recovery") {
		return fail("dispatch")
	}
	if _, err := authorizeDataRecovery(func(key string) string {
		switch key {
		case "GITHUB_WORKFLOW_REF":
			return "devantler-tech/platform/.github/workflows/verify-email-data-recovery.yaml@refs/heads/main"
		case "EMAIL_DATA_RECOVERY_CONFIRM":
			return "verify-email-data-recovery"
		default:
			return env(key)
		}
	}); err != nil {
		return fail("dispatch")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	reads := observationReadOnly{base: k, parity: parity}
	d := &recoveryDriver{i: retainedRecovery, k: reads, refs: map[string]ownedReference{}}
	ns, found, err := d.lookup(ctx, "Namespace", d.i.namespace, "")
	if err != nil || !found || text(child(child(ns, "metadata"), "labels"), recoveryLabel) != d.i.run || child(ns, "metadata")["deletionTimestamp"] != nil {
		return fail("reader-identity")
	}
	if d.image, err = loadRecoveryImage(); err != nil {
		return fail("image-declaration")
	}
	pod, found, err := d.lookup(ctx, "Pod", "reader", d.i.namespace)
	if err != nil || !found || text(child(pod, "status"), "phase") != "Running" || !admittedRecoveryObject(recoveryReader(d.i, d.image), pod) {
		return fail("reader-identity")
	}
	for _, o := range []object{ns, pod} {
		m := child(o, "metadata")
		if text(m, "uid") == "" || m["deletionTimestamp"] != nil || text(child(m, "labels"), recoveryLabel) != d.i.run {
			return fail("reader-identity")
		}
		ref := ownedReference{kind: text(o, "kind"), name: text(m, "name"), namespace: text(m, "namespace"), uid: text(m, "uid")}
		if !d.i.permitsMutation(ref.kind, ref.namespace, ref.name) {
			return fail("reader-identity")
		}
		d.refs[ref.key()] = ref
	}
	readerUID := text(child(pod, "metadata"), "uid")
	if parity {
		if readerUID != env("EMAIL_RETAINED_READER_UID") {
			return fail("reader-identity")
		}
		if stage := bindObservationParity(ctx, d, reads, readerUID, env("EMAIL_RETAINED_CLAIM_UID")); stage != "" {
			return fail(stage)
		}
	}
	endpoint, stop, err := d.forward(ctx, d.i.namespace, "reader", readerUID)
	if err != nil {
		return fail("reader-forward")
	}
	defer stop()
	probe, err := newMailboxClient(endpoint, "unused-observation-probe")
	if err != nil {
		return fail("reader-endpoint")
	}
	status, err := observationHealth(ctx, probe)
	if err != nil {
		return fail("reader-health-transport")
	}
	if _, err = fmt.Fprintf(output, "EMAIL_RECOVERY_OBSERVATION_HEALTH status=%d writes=0\n", status); err != nil {
		return 2
	}
	if status != http.StatusOK {
		return fail("reader-health")
	}
	secret, found, err := d.lookup(ctx, "Secret", "private-mail-maintainer", "private-mail")
	if err != nil || !found {
		return fail("account-credential")
	}
	password, err := base64.StdEncoding.DecodeString(text(child(secret, "data"), "password"))
	if err != nil || len(password) < 16 || len(password) > 4096 {
		return fail("account-credential")
	}
	reader, err := newMailboxClient(endpoint, string(password))
	if err != nil {
		return fail("reader-endpoint")
	}
	if err = reader.session(ctx); err != nil {
		switch err.Error() {
		case "mailbox-http":
			return fail("reader-session-http")
		case "mailbox-transport":
			return fail("reader-session-transport")
		case "session-account":
			return fail("reader-session-account")
		default:
			return fail("reader-session-response")
		}
	}
	if parity {
		if stage := observeRetainedParity(ctx, d, reader); stage != "" {
			return fail(stage)
		}
	}
	for _, ref := range d.refs {
		if _, err = d.owned(ctx, ref); err != nil {
			return fail("reader-replaced")
		}
	}
	if parity {
		if _, err = fmt.Fprintln(output, "EMAIL_RETAINED_PARITY=VERIFIED source_writes=0 original_trial=UNVERIFIED"); err != nil {
			return 2
		}
		return 0
	}
	if _, err = fmt.Fprintln(output, "EMAIL_RECOVERY_OBSERVATION=PASS readiness=VERIFIED session=VERIFIED writes=0"); err != nil {
		return 2
	}
	return 0
}

func bindObservationParity(ctx context.Context, d *recoveryDriver, reads observationReadOnly, readerUID, claimUID string) string {
	claim, found, err := d.lookup(ctx, "PersistentVolumeClaim", "data", d.i.namespace)
	m := child(claim, "metadata")
	readerVolume := text(child(claim, "spec"), "volumeName")
	if err != nil || !found || claimUID == "" || text(m, "uid") != claimUID || m["deletionTimestamp"] != nil || text(child(claim, "status"), "phase") != "Bound" || !kubernetesName.MatchString(readerVolume) {
		return "reader-claim"
	}
	ref := ownedReference{kind: "PersistentVolumeClaim", name: "data", namespace: d.i.namespace, uid: claimUID}
	d.refs[ref.key()] = ref
	if _, err = d.owned(ctx, ref); err != nil {
		return "reader-claim"
	}
	claim, found, err = d.lookup(ctx, "PersistentVolumeClaim", "private-mail-data", "private-mail")
	reads.servingVolume = text(child(claim, "spec"), "volumeName")
	if err != nil || !found || !kubernetesName.MatchString(reads.servingVolume) {
		return "source-identity"
	}
	d.k = reads
	d.source, err = d.readSource(ctx)
	if err != nil || d.source.podUID == readerUID || d.source.claimUID == claimUID || d.source.pv == readerVolume {
		return "source-identity"
	}
	return ""
}

func observeRetainedParity(ctx context.Context, d *recoveryDriver, reader *mailboxClient) string {
	endpoint, stop, err := d.forward(ctx, "private-mail", "private-mail-0", d.source.podUID)
	if err != nil {
		return "source-forward"
	}
	defer stop()
	source, err := newRetainedMailboxClient(endpoint, reader.password)
	if err != nil {
		return "source-endpoint"
	}
	if _, err = verifyRetainedMessage(ctx, reader, source); err != nil {
		return err.Error()
	}
	if err = d.sourceRetained(ctx); err != nil {
		return "source-replaced"
	}
	return ""
}
