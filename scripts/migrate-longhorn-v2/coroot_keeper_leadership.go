package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

const transferCorootKeeperOperation = "transfer-coroot-keeper-leadership-to-v2"

const corootKeeperRequestLeadership = "exec 3<>/dev/tcp/127.0.0.1/9181; printf rqld >&3; timeout 6 cat <&3"

// Allow exactly one retained provider member and two independently verified
// V2 members. The protected confirmation names the provider identity and the
// V2 member that may take leadership.
func bindCorootKeeperLeadership(receipt corootKeeperQuorumReceipt, input corootKeeperCutoverInput) (leader, remaining int, err error) {
	leader, remaining = -1, -1
	if input.Ordinal < 0 || input.Ordinal >= len(receipt.Members) || !retentionDigest.MatchString(input.Digest) {
		return -1, -1, errCorootKeeperStorage
	}
	for i, member := range receipt.Members {
		if member.Leader {
			if leader != -1 {
				return -1, -1, errCorootKeeperQuorum
			}
			leader = i
		}
		if receipt.V2Claims[i].UID != "" {
			if receipt.Disks[i] != (corootKeeperProviderDisk{}) {
				return -1, -1, errCorootKeeperStorage
			}
			continue
		}
		if remaining != -1 || receipt.Disks[i].Digest != input.Digest {
			return -1, -1, errCorootKeeperStorage
		}
		remaining = i
	}
	if leader == -1 || remaining == -1 || input.Ordinal == remaining {
		return -1, -1, errCorootKeeperStorage
	}
	return leader, remaining, nil
}

func unchangedKeeperMembership(before, after corootKeeperQuorumReceipt) bool {
	for i := range before.Members {
		before.Members[i].Leader = false
		after.Members[i].Leader = false
	}
	return before == after
}

func runCorootKeeperLeadershipTransfer(ctx context.Context, c client, source, run string, input corootKeeperCutoverInput) error {
	if c.context != "admin@prod" || !validBenchmarkOwner(run, source) {
		return errCorootKeeperReceipt
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	capacity, err := activation.recoveryClaimStorage(ctx, corootKeeperDataSize)
	if err != nil {
		return err
	}
	before, err := readCorootKeeperQuorum(ctx, c, source, capacity.Disks)
	if err != nil {
		return err
	}
	leader, remaining, err := bindCorootKeeperLeadership(before, input)
	if err != nil {
		return err
	}
	if leader != remaining {
		// An existing V2 leader already allows the last provider member to be
		// migrated as a follower. Another transfer would only interrupt writes.
		return nil
	}
	latest, err := readCorootKeeperQuorum(ctx, c, source, capacity.Disks)
	if err != nil || latest != before {
		return errors.New("the Keeper quorum changed before the leadership request")
	}
	target := latest.Members[input.Ordinal]
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	// Only the authenticated, selected V2 member can request leadership.
	// Read back the majority and leader even when the command times out.
	out, callErr := c.kubectl(ctx, nil, "exec", target.Pod, "--namespace="+corootKeeperNamespace,
		"--container="+corootKeeperContainer, "--pod-running-timeout=10s", "--", "bash", "-ec", corootKeeperRequestLeadership)
	acknowledged := callErr == nil && strings.TrimSpace(string(out)) == "Sent leadership request to leader."
	if err := waitReadyWithin(ctx, 90*time.Second, func(ctx context.Context) error {
		current, err := readCorootKeeperQuorum(ctx, c, source, capacity.Disks)
		if err != nil {
			return err
		}
		leader, _, err := bindCorootKeeperLeadership(current, input)
		if err != nil || !unchangedKeeperMembership(before, current) || leader != input.Ordinal {
			return errCorootKeeperQuorum
		}
		return nil
	}); err != nil {
		if !acknowledged {
			return errors.Join(err, errors.New("the Keeper leadership request was not acknowledged"))
		}
		return err
	}
	return nil
}
