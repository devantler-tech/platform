package main

import (
	"context"
	"os"
	"regexp"
	"strings"
)

var openBaoReleaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func verifyOpenBaoRaftMember(ctx context.Context, c client, expected benchmarkResource, release string, follower bool) error {
	if c.context != "admin@prod" || !knownOpenBaoMember(expected.Name) ||
		!openBaoPairIdentity(expected, openBaoNamespace, expected.Name) || !openBaoReleaseVersion.MatchString(release) {
		return errOpenBaoProviderStorage
	}
	private, err := os.MkdirTemp("", "openbao-v2-raft-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(private) }()
	const judge = `set -euo pipefail
export OPENBAO_SERVERS=openbao-0,openbao-1,openbao-2
export REPLACEMENT_SERVER="$2" OPENBAO_VERSION="$3" PREVIOUS_VERSION=""
unset HCLOUD_TOKEN
source scripts/judge-openbao-replacement.lib.sh
judge_openbao_replacement "$1" "" "$4" >/dev/null
openbao_pod_uid "$1" "$2"
`
	role := ""
	if follower {
		role = "follower"
	}
	out, err := c.run(ctx, nil, "bash", "-ec", judge, "openbao-v2-raft", private, expected.Name, release, role)
	if err != nil || strings.TrimSpace(string(out)) != expected.UID {
		return errOpenBaoProviderStorage
	}
	return nil
}
