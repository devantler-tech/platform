package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNativeRestorePreparedTargetBindsActivationDiskAndMemory(t *testing.T) {
	for _, fault := range []string{"none", "allocation changed", "memory read failed", "node replaced", "boot changed", "disk changed", "journal changed", "reader close", "context expired"} {
		t.Run(fault, func(t *testing.T) {
			f, a := newActivationAPI(t)
			if err := a.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			activation, err := a.accepted(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			binding := activation.Targets["temporary-v1-1"]
			nodes, err := indexed(f.s.nodes)
			if err != nil {
				t.Fatal(err)
			}
			storage, err := indexed(f.s.storageNodes)
			if err != nil {
				t.Fatal(err)
			}
			node, lh, journal := nodes[binding.Name], storage[binding.Name], f.preparations[binding.Selection]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			delegate := a.c.run
			var reads []string
			a.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program != "talosctl" || len(args) < 2 || args[1] != "read" {
					return delegate(ctx, input, program, args...)
				}
				if input != nil || len(args) != 3 || !hostMemoryReadAllowed(program, args...) {
					t.Fatal("memory observation widened the fixed read boundary")
				}
				reads = append(reads, args[2])
				if args[2] == "/proc/sys/vm/nr_hugepages" {
					if fault == "allocation changed" && len(reads) == 3 {
						return []byte("318\n"), nil
					}
					return []byte("317\n"), nil
				}
				switch fault {
				case "memory read failed":
					return nil, errors.New("unreadable counters")
				case "node replaced":
					valueObject(node, "metadata")["uid"] = "replacement-node"
				case "boot changed":
					valueObject(node, "status", "nodeInfo")["bootID"] = "20000000-0000-4000-8000-000000000099"
				case "disk changed":
					valueObject(lh, "status", "diskStatus", "longhorn-v2")["diskUUID"] = "replacement-disk"
				case "journal changed":
					valueObject(journal, "metadata")["uid"] = "replacement-journal"
				case "context expired":
					cancel()
				}
				return []byte(hostMemoryFixture), nil
			}
			if fault == "reader close" {
				factory := a.reader
				a.reader = func(ctx context.Context, address string) (nativeReplicaReader, func() error, error) {
					reader, closeReader, err := factory(ctx, address)
					return reader, func() error { _ = closeReader(); return errors.New("uncertain close") }, err
				}
			}
			target, report, err := observeNativeRestorePreparedTarget(ctx, a, activation, binding)
			if f.writes != 3 {
				t.Fatal("read-only canary preflight wrote to the pool")
			}
			if fault == "none" {
				if err != nil || target.Name != binding.Name || target.UID != binding.NodeUID || target.Preparation.Activation != activation.Journal ||
					target.Preparation.ActivationSHA256 != activation.RecordSHA256 || target.Preparation.DiskUUID != activation.DiskUUIDs[binding.Selection] ||
					report.PersistentPages != 317 || report.AvailableKiB != 6000000 ||
					strings.Join(reads, ",") != "/proc/sys/vm/nr_hugepages,/proc/meminfo,/proc/sys/vm/nr_hugepages" {
					t.Fatal("the activated native storage and memory were not observed:", target, report, reads, err)
				}
			} else if err == nil || target != (nativeRestoreTarget{}) || report != (hostMemoryReport{}) {
				t.Fatal("unstable counters escaped the original activation bracket:", target, report, err)
			}
		})
	}
}
