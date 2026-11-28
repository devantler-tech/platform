package main

import (
	"context"
	"errors"
)

const (
	replaceProviderCNPGStandbyOperation = "replace-provider-cnpg-v2-standby"
	resumeProviderCNPGStandbyOperation  = "resume-retained-provider-cnpg-v2-standby"
	recoverProviderCNPGStandbyOperation = "recover-provider-cnpg-standby-name"
)

type providerCNPGReplacementGrant struct {
	Database        cnpgSource
	Original, Spare string
	Resume          umamiStandbyResume
	Recovery        umamiStandbyNameRecovery
}

func authorizeProviderCNPGReplacement(getenv func(string) string, operation string) (providerCNPGReplacementGrant, error) {
	database := providerCNPGStandbySource(getenv("LONGHORN_V2_DATABASE"))
	if database.Name == "" {
		return providerCNPGReplacementGrant{}, errProviderCNPGStandby
	}
	switch operation {
	case replaceProviderCNPGStandbyOperation, providerV2SwitchoverOperation:
		original, spare, err := authorizeCNPGStandbyTarget(getenv, operation, database.Name)
		return providerCNPGReplacementGrant{Database: database, Original: original, Spare: spare}, err
	case resumeProviderCNPGStandbyOperation:
		resume, err := authorizeCNPGStandbyResume(getenv, operation, database.Name)
		return providerCNPGReplacementGrant{Database: database, Resume: resume}, err
	case recoverProviderCNPGStandbyOperation:
		recovery, err := authorizeCNPGStandbyNameRecovery(getenv, operation, database.Name)
		return providerCNPGReplacementGrant{Database: database, Recovery: recovery}, err
	default:
		return providerCNPGReplacementGrant{}, errProviderCNPGStandby
	}
}

func runProviderCNPGReplacement(ctx context.Context, c client, getenv func(string) string, operation string) error {
	grant, err := authorizeProviderCNPGReplacement(getenv, operation)
	if err != nil {
		return err
	}
	owner := umamiReplacementOwner{Source: getenv("GITHUB_SHA"), Run: getenv("GITHUB_RUN_ID")}
	token := getenv("HCLOUD_TOKEN")
	if token == "" || !validBenchmarkOwner(owner.Run, owner.Source) {
		return errors.New("the reviewed provider volume reader is unavailable")
	}
	c.readProviderVolume = func(ctx context.Context, handle string) (object, error) {
		return readOpenBaoProviderVolume(ctx, nil, token, handle)
	}
	switch operation {
	case replaceProviderCNPGStandbyOperation:
		return runCNPGStandbyReplacement(ctx, c, owner.Source, owner.Run, grant.Database, grant.Original, grant.Spare)
	case resumeProviderCNPGStandbyOperation:
		return resumeCNPGStandbyReplacement(ctx, c, owner, grant.Resume, grant.Database.Name)
	case recoverProviderCNPGStandbyOperation:
		return runUmamiNameRecovery(ctx, c, grant.Recovery, owner)
	case providerV2SwitchoverOperation:
		return runCNPGV2Switchover(ctx, c, grant.Database, owner.Source, owner.Run, grant.Original, grant.Spare)
	default:
		return errProviderCNPGStandby
	}
}
