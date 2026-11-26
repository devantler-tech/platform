package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

const cnpgArchiveProofOperation = "prove-database-v2-archive"

func authorizeCNPGArchiveProof(getenv func(string) string) error {
	target := getenv("LONGHORN_V2_DATABASE")
	if getenv("LONGHORN_V2_OPERATION") != cnpgArchiveProofOperation || getenv("LONGHORN_V2_NODE") != "" ||
		!validBenchmarkOwner(getenv("GITHUB_RUN_ID"), getenv("GITHUB_SHA")) {
		return errors.New("the confirmed first-attempt database archive proof is required")
	}
	for _, archive := range cnpgArchiveSources() {
		if archive.Name == target {
			return authorizeWorkflow(getenv, "prove-longhorn-v2-database-archive-"+target)
		}
	}
	return errors.New("the selected database has no reviewed recovery archive")
}

func runCNPGArchiveProof(ctx context.Context, c client) error {
	if authorizeCNPGArchiveProof(os.Getenv) != nil || !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed {
		return errors.New("the protected database archive proof grant is unverified")
	}
	if err := verifyReclaimedNativeRestore(ctx, c, os.Getenv("GITHUB_RUN_ID")); err != nil {
		return errors.New("database archive proof requires the verified V2 restore and retired test volume")
	}
	p, err := beginCNPGProof(ctx, c, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), os.Getenv("LONGHORN_V2_DATABASE"))
	if err != nil {
		return errors.New("database archive proof held at the original-source journal")
	}
	hostname, err := p.chooseRecoveryHost(ctx)
	if err != nil {
		return errors.New("database archive proof held at recovery worker capacity")
	}
	env := func(kind string) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := p.createEnvironmentResource(ctx, kind)
			return err
		}
	}
	database := func(kind string) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := p.createDatabaseResource(ctx, kind)
			return err
		}
	}
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"fresh source backup", p.freshBackup},
		{"archived recovery target", func(ctx context.Context) error { return p.prepareRecovery(ctx, hostname) }},
		{"isolated namespace creation", env("Namespace")},
		{"isolation policy creation", env("Policy")},
		{"native isolation import", p.prepareIsolation},
		{"archive credential binding", p.copySourceCredential},
		{"read-only archive configuration", database("ObjectStore")},
		{"isolated V2 database creation", database("Cluster")},
	} {
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("database archive proof held at %s", step.name)
		}
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		if _, err := p.currentExecution(ctx); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		_, err := p.observeRecoveredStorage(ctx)
		return err
	}); err != nil {
		return errors.New("database archive proof held at recovered V2 storage")
	}
	data, err := p.observeRecoveredDatabaseData(ctx)
	if err != nil {
		return errors.New("database archive proof held at recovered physical history, filesystem or isolation")
	}
	if err := p.recordVerifiedArchive(ctx, data); err != nil {
		return err
	}
	return p.retireArchiveReadCredential(ctx)
}
