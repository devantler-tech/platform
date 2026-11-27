package main

import (
	"context"
	"errors"
)

const (
	replaceWeddingStandbyOperation = "replace-wedding-v2-standby"
	resumeWeddingStandbyOperation  = "resume-retained-wedding-v2-standby"
	recoverWeddingStandbyOperation = "recover-wedding-standby-name"
)

var weddingStandbySource = cnpgSource{Namespace: "wedding-app", Name: "wedding-db"}

type weddingStandbyGrant struct {
	Original, Spare string
	Resume          umamiStandbyResume
	Recovery        umamiStandbyNameRecovery
}

func authorizeWeddingStandbyOperation(getenv func(string) string, operation string) (weddingStandbyGrant, error) {
	database := weddingStandbySource.Name
	switch operation {
	case replaceWeddingStandbyOperation:
		original, spare, err := authorizeCNPGStandbyTarget(getenv, operation, database)
		return weddingStandbyGrant{Original: original, Spare: spare}, err
	case resumeWeddingStandbyOperation:
		resume, err := authorizeCNPGStandbyResume(getenv, operation, database)
		return weddingStandbyGrant{Original: resume.target, Resume: resume}, err
	case recoverWeddingStandbyOperation:
		recovery, err := authorizeCNPGStandbyNameRecovery(getenv, operation, database)
		return weddingStandbyGrant{Original: recovery.original.target, Recovery: recovery}, err
	default:
		return weddingStandbyGrant{}, errors.New("the selected Wedding standby request is unverified")
	}
}

func runWeddingStandbyOperation(ctx context.Context, c client, getenv func(string) string, operation string) error {
	grant, err := authorizeWeddingStandbyOperation(getenv, operation)
	if err != nil {
		return err
	}
	owner := umamiReplacementOwner{Source: getenv("GITHUB_SHA"), Run: getenv("GITHUB_RUN_ID")}
	switch operation {
	case replaceWeddingStandbyOperation:
		return runCNPGStandbyReplacement(ctx, c, owner.Source, owner.Run, weddingStandbySource, grant.Original, grant.Spare)
	case resumeWeddingStandbyOperation:
		return resumeCNPGStandbyReplacement(ctx, c, owner, grant.Resume, weddingStandbySource.Name)
	case recoverWeddingStandbyOperation:
		return runUmamiNameRecovery(ctx, c, grant.Recovery, owner)
	default:
		return errors.New("the selected Wedding standby request is unverified")
	}
}
