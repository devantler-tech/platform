package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type hostMemoryReceipt struct {
	Schema              int              `json:"schema"`
	ObservationOnly     bool             `json:"observationOnly"`
	ResetClearance      bool             `json:"resetClearance"`
	APIProjectionDigest string           `json:"apiProjectionDigest"`
	Report              hostMemoryReport `json:"report"`
}

func hostMemoryReadAllowed(program string, args ...string) bool {
	if program == "talosctl" && len(args) == 3 && strings.HasPrefix(args[0], "--nodes=") && args[1] == "read" &&
		(args[2] == "/proc/sys/vm/nr_hugepages" || args[2] == "/proc/meminfo") {
		target := strings.TrimPrefix(args[0], "--nodes=")
		address, err := netip.ParseAddr(target)
		return err == nil && address.String() == target && netip.MustParsePrefix("10.0.0.0/16").Contains(address)
	}
	return inventoryReadAllowed(program, args...)
}

func hostMemoryReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil || !hostMemoryReadAllowed(program, args...) {
		return nil, errHostMemory
	}
	return inventoryExecuteCommand(ctx, program, args...)
}

func observeHostMemory(ctx context.Context, c client, target string, factory inventoryReaderFactory) (receipt hostMemoryReceipt, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	expires, _ := ctx.Deadline()
	active := func() bool { return ctx.Err() == nil && time.Now().Before(expires) }
	if !active() || !inventoryTarget.MatchString(target) {
		return hostMemoryReceipt{}, stageDeadline
	}
	before, node, err := c.hostMemoryProjection(ctx, target)
	if err != nil {
		return hostMemoryReceipt{}, stageProjectionBefore
	}
	reader, closeReader, err := factory(ctx, before.Address)
	if err != nil || reader == nil || closeReader == nil {
		return hostMemoryReceipt{}, stageClient
	}
	defer func() {
		if closeReader() != nil {
			receipt = hostMemoryReceipt{}
			err = errors.Join(err, stageClose)
		}
		if !active() {
			receipt = hostMemoryReceipt{}
			err = errors.Join(err, stageDeadline)
		}
	}()
	if c.verifyHost(ctx, before.Address, node) != nil {
		return hostMemoryReceipt{}, stageHostBefore
	}
	boot, err := reader.BootID(ctx)
	if err != nil || boot != before.BootID {
		return hostMemoryReceipt{}, stageBootBefore
	}
	pages, err := c.talosCommand(ctx, talosReadLimit, before.Address, "read", "/proc/sys/vm/nr_hugepages")
	if err != nil {
		return hostMemoryReceipt{}, stageHostMemoryCounters
	}
	memory, err := c.talosCommand(ctx, talosReadLimit, before.Address, "read", "/proc/meminfo")
	if err != nil {
		return hostMemoryReceipt{}, stageHostMemoryCounters
	}
	report, err := parseHostMemory(pages, memory)
	if err != nil {
		return hostMemoryReceipt{}, stageHostMemoryCounters
	}
	second, err := c.talosCommand(ctx, talosReadLimit, before.Address, "read", "/proc/sys/vm/nr_hugepages")
	if err != nil || !bytes.Equal(pages, second) {
		return hostMemoryReceipt{}, stageHostMemoryAllocationChanged
	}
	after, node, err := c.hostMemoryProjection(ctx, target)
	if err != nil || before.Digest != after.Digest {
		return hostMemoryReceipt{}, stageAPIConsistency
	}
	if c.verifyHost(ctx, after.Address, node) != nil {
		return hostMemoryReceipt{}, stageHostAfter
	}
	boot, err = reader.BootID(ctx)
	if err != nil || boot != before.BootID {
		return hostMemoryReceipt{}, stageBootAfter
	}
	return hostMemoryReceipt{Schema: 1, ObservationOnly: true, ResetClearance: false, APIProjectionDigest: before.Digest, Report: report}, nil
}

func authorizeHostMemory(getenv func(string) string, target string) error {
	if !inventoryTarget.MatchString(target) || getenv("LONGHORN_V2_NODE") != target || getenv("LONGHORN_V2_OPERATION") != "inspect-host-memory" {
		return errHostMemory
	}
	return authorizeWorkflow(getenv, "inspect-longhorn-v2-host-memory-"+target)
}

func runHostMemory(ctx context.Context, target, source, runID string) error {
	c := client{run: hostMemoryReadCommand, context: "admin@prod"}
	if sourceCurrent(ctx, c.run, source) != nil {
		return stageSourceBefore
	}
	receipt, err := observeHostMemory(ctx, c, target, newInventoryReader)
	if err != nil {
		return err
	}
	if sourceCurrent(ctx, c.run, source) != nil {
		return stageSourceAfter
	}
	if json.NewEncoder(os.Stdout).Encode(struct {
		Source      string            `json:"source"`
		Run         string            `json:"run"`
		Observation hostMemoryReceipt `json:"observation"`
	}{source, runID, receipt}) != nil {
		return stageOutput
	}
	return nil
}

var errHostMemory = errors.New("native host memory observation is incomplete or changed")
var hostPageCount = regexp.MustCompile(`^[0-9]{1,10}\n$`)

type hostMemoryReport struct {
	PersistentPages, TotalPages, FreePages, ReservedPages, SurplusPages uint64
	TotalKiB, AvailableKiB, PageSizeKiB                                 uint64
}

func parseHostMemory(pages, memory []byte) (hostMemoryReport, error) {
	if !hostPageCount.Match(pages) || len(memory) == 0 || len(memory) > 16384 || !bytes.HasSuffix(memory, []byte("\n")) {
		return hostMemoryReport{}, errHostMemory
	}
	report := hostMemoryReport{}
	var err error
	report.PersistentPages, err = strconv.ParseUint(strings.TrimSuffix(string(pages), "\n"), 10, 32)
	if err != nil {
		return hostMemoryReport{}, errHostMemory
	}
	fields := map[string]*uint64{
		"MemTotal:": &report.TotalKiB, "MemAvailable:": &report.AvailableKiB,
		"HugePages_Total:": &report.TotalPages, "HugePages_Free:": &report.FreePages,
		"HugePages_Rsvd:": &report.ReservedPages, "HugePages_Surp:": &report.SurplusPages,
		"Hugepagesize:": &report.PageSizeKiB,
	}
	seen := make(map[string]bool, len(fields))
	for line := range strings.SplitSeq(string(memory), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		destination, selected := fields[parts[0]]
		if !selected {
			continue
		}
		kilobytes := parts[0] == "MemTotal:" || parts[0] == "MemAvailable:" || parts[0] == "Hugepagesize:"
		if seen[parts[0]] || kilobytes && (len(parts) != 3 || parts[2] != "kB") || !kilobytes && len(parts) != 2 {
			return hostMemoryReport{}, errHostMemory
		}
		if parts[1] == "" || strings.IndexFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return hostMemoryReport{}, errHostMemory
		}
		*destination, err = strconv.ParseUint(parts[1], 10, 63)
		if err != nil {
			return hostMemoryReport{}, errHostMemory
		}
		seen[parts[0]] = true
	}
	if len(seen) != len(fields) || report.TotalKiB == 0 || report.PageSizeKiB == 0 || report.AvailableKiB > report.TotalKiB ||
		report.FreePages > report.TotalPages || report.ReservedPages > report.TotalPages || report.SurplusPages > report.TotalPages {
		return hostMemoryReport{}, errHostMemory
	}
	return report, nil
}
