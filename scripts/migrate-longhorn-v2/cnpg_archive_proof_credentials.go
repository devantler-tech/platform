package main

import (
	"context"
	"errors"
)

const (
	cnpgArchiveCredentialDeleteRequested = "archive-credential-delete-requested"
	cnpgArchiveCredentialRetired         = "archive-credential-retired"
)

// Retire the proof's temporary archive credential once the recovered instance
// is running independently. Keep its source Secret, evidence and V2 claim.
func (p *cnpgProofRun) retireArchiveReadCredential(ctx context.Context) error {
	refused := errors.New("the verified archive proof's temporary credential could not be retired")
	if p.record.Phase != "restore-verified" || validateCNPGRestoreReceipt(p.record) != nil {
		return refused
	}
	identity := p.record.Resources["Secret"]
	read := func() (object, error) {
		if identity.Name != "archive-read" || identity.Namespace != p.record.Resources["Namespace"].Name ||
			identity == p.record.Credential.Source || !isolationUID.MatchString(identity.UID) {
			return nil, refused
		}
		secret, err := p.c.get(ctx, "secret", identity.Namespace, identity.Name, false)
		if err != nil || benchmarkIdentity(secret) != identity || !recoveredNativeObject(secret, "v1", "Secret", identity.Namespace) ||
			text(secret, "type") != "Opaque" || value(secret, "immutable") != true ||
			text(secret, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run ||
			text(secret, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
			return nil, refused
		}
		digest, err := recoveryCredentialData(secret)
		if err != nil || digest != p.record.Credential.DataSHA {
			return nil, refused
		}
		return secret, nil
	}
	if _, err := read(); err != nil {
		return err
	}
	p.record.Phase = cnpgArchiveCredentialDeleteRequested
	if err := p.save(ctx); err != nil {
		return err
	}
	owner, err := p.currentExecution(ctx)
	if err != nil {
		return err
	}
	secret, err := read()
	if err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + identity.Namespace + "/secrets/" + identity.Name
	if err := p.c.deleteObserved(ctx, owner.Source, path, secret); err != nil {
		return refused
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		current, err := p.c.get(ctx, "secret", identity.Namespace, identity.Name, true)
		if err != nil || current != nil {
			return refused
		}
		return nil
	}); err != nil {
		return refused
	}
	p.record.Phase = cnpgArchiveCredentialRetired
	return p.save(ctx)
}
