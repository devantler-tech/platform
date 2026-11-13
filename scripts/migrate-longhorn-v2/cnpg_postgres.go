package main

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type postgresState struct {
	SystemID   string `json:"system_id"`
	Timeline   string `json:"timeline"`
	FlushLSN   string `json:"flush_lsn"`
	ReplayLSN  string `json:"replay_lsn"`
	InRecovery bool   `json:"in_recovery"`
	// PostmasterStart catches an in-place server restart, which keeps the container and its restart count.
	PostmasterStart string `json:"postmaster_start"`
}

// Only fixed SQL is passed to PostgreSQL. This reveals physical recovery
// coordinates, never application rows or credentials.
const postgresStateSQL = `SELECT pg_catalog.json_build_object('system_id',(pg_catalog.pg_control_system()).system_identifier::text,'timeline',(pg_catalog.pg_control_checkpoint()).timeline_id::text,'flush_lsn',CASE WHEN pg_catalog.pg_is_in_recovery() THEN NULL ELSE pg_catalog.pg_current_wal_flush_lsn()::text END,'replay_lsn',pg_catalog.pg_last_wal_replay_lsn()::text,'in_recovery',pg_catalog.pg_is_in_recovery(),'postmaster_start',pg_catalog.pg_postmaster_start_time()::text);`

var postgresLSN = regexp.MustCompile(`^[0-9A-F]{1,8}/[0-9A-F]{1,8}$`)
var postgresImageDigest = regexp.MustCompile(`(^|@)sha256:[0-9a-f]{64}$`)
var postgresContainerID = regexp.MustCompile(`^containerd://[0-9a-f]{64}$`)

type postgresProcess struct {
	Image, Container string
	Restarts         float64
}

// postgresExecutable binds one ready process to immutable image and container identities.
func postgresExecutable(pod object) (postgresProcess, error) {
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	matched := 0
	var result postgresProcess
	for _, raw := range statuses {
		s, _ := raw.(object)
		if text(s, "name") != "postgres" {
			continue
		}
		matched++
		restarts, ok := value(s, "restartCount").(float64)
		result = postgresProcess{text(s, "imageID"), text(s, "containerID"), restarts}
		if !ok || restarts < 0 || restarts > 2147483647 || restarts != float64(int64(restarts)) || value(s, "ready") != true || !postgresImageDigest.MatchString(result.Image) || !postgresContainerID.MatchString(result.Container) {
			return result, errors.New("native PostgreSQL process identity is incomplete")
		}
	}
	if matched != 1 {
		return result, errors.New("native PostgreSQL process identity is ambiguous")
	}
	return result, nil
}

// walPosition parses a nonzero PostgreSQL LSN without accepting truncated halves.
func walPosition(raw string) (uint64, error) {
	if !postgresLSN.MatchString(raw) {
		return 0, errors.New("native WAL coordinate is missing or malformed")
	}
	parts := strings.Split(raw, "/")
	high, _ := strconv.ParseUint(parts[0], 16, 32)
	low, _ := strconv.ParseUint(parts[1], 16, 32)
	position := high<<32 | low
	if position == 0 {
		return 0, errors.New("native WAL coordinate is invalid")
	}
	return position, nil
}

// validPostgresState requires canonical physical identity and the current role's WAL coordinate.
func validPostgresState(p postgresState) error {
	for _, field := range []struct {
		value string
		bits  int
	}{{p.SystemID, 64}, {p.Timeline, 32}} {
		n, err := strconv.ParseUint(field.value, 10, field.bits)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != field.value {
			return errors.New("physical PostgreSQL identity or timeline is invalid")
		}
	}
	position := p.FlushLSN
	if p.InRecovery {
		position = p.ReplayLSN
	}
	_, err := walPosition(position)
	return err
}

// verifyPostgresPod rejects replacement, restart or image changes across SQL execution.
func verifyPostgresPod(current, original, cluster object) error {
	status, _ := value(current, "status").(object)
	if text(original, "metadata", "uid") == "" || text(current, "metadata", "uid") != text(original, "metadata", "uid") || value(current, "metadata", "deletionTimestamp") != nil ||
		!cnpgOwned(current, cluster) || !ready(status) || text(current, "status", "phase") != "Running" {
		return errors.New("database execution requires the exact healthy owned PostgreSQL pod")
	}
	containers, _ := value(current, "spec", "containers").([]any)
	matched := 0
	for _, raw := range containers {
		container, _ := raw.(object)
		if text(container, "name") == "postgres" {
			matched++
			if text(container, "image") != text(cluster, "spec", "imageName") || text(container, "image") == "" {
				return errors.New("PostgreSQL operand image changed")
			}
		}
	}
	if matched != 1 {
		return errors.New("PostgreSQL executable is ambiguous")
	}
	expected, err := postgresExecutable(original)
	if err != nil {
		return err
	}
	actual, err := postgresExecutable(current)
	if err != nil || actual != expected {
		return errors.New("PostgreSQL restarted or its executable image changed during the observation")
	}
	return nil
}

// observePostgres brackets the fixed bounded SQL command with native process reads.
func (c client) observePostgres(ctx context.Context, cluster, pod object) (postgresState, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ns, name := text(cluster, "metadata", "namespace"), text(pod, "metadata", "name")
	before, err := c.get(ctx, "pod", ns, name, false)
	if err != nil {
		return postgresState{}, err
	}
	if err = verifyPostgresPod(before, pod, cluster); err != nil {
		return postgresState{}, err
	}
	// Clear libpq service, host-address, options and executable overrides. The
	// fixed local socket belongs to this Pod, not an environment-selected peer.
	// Override persisted role/database lookup and write defaults as well. Types
	// and operators resolve through the catalog alongside the qualified functions.
	raw, err := c.kubectl(ctx, nil, "exec", name, "--namespace="+ns, "--container=postgres", "--pod-running-timeout=20s", "--", "/usr/bin/env", "-i", "PATH=/usr/lib/postgresql/18/bin:/usr/local/bin:/usr/bin:/bin", "PGCONNECT_TIMEOUT=10", "PGOPTIONS=-c search_path=pg_catalog -c default_transaction_read_only=on", "psql", "--host=/controller/run", "--port=5432", "--no-psqlrc", "--no-password", "--username=postgres", "--dbname=postgres", "--set=ON_ERROR_STOP=1", "--tuples-only", "--no-align", "--quiet", "--command="+postgresStateSQL)
	if err != nil {
		return postgresState{}, err
	}
	after, err := c.get(ctx, "pod", ns, name, false)
	if err != nil {
		return postgresState{}, err
	}
	if err = verifyPostgresPod(after, pod, cluster); err != nil {
		return postgresState{}, err
	}
	return parsePostgresState(raw)
}

// parsePostgresState requires an explicit recovery role in a complete JSON response.
func parsePostgresState(raw []byte) (postgresState, error) {
	var p postgresState
	var present struct {
		InRecovery *bool `json:"in_recovery"`
	}
	if json.Unmarshal(raw, &p) != nil || json.Unmarshal(raw, &present) != nil || present.InRecovery == nil {
		return p, errors.New("native PostgreSQL role observation is incomplete")
	}
	return p, validPostgresState(p)
}

// verifyPostgresStandby proves the captured primary barrier was replayed on the same history.
func verifyPostgresStandby(primary, standby postgresState) error {
	if validPostgresState(primary) != nil || validPostgresState(standby) != nil || primary.InRecovery || !standby.InRecovery || primary.SystemID != standby.SystemID || primary.Timeline != standby.Timeline {
		return errors.New("standby does not share the current primary's physical history")
	}
	barrier, _ := walPosition(primary.FlushLSN)
	replayed, _ := walPosition(standby.ReplayLSN)
	if replayed < barrier {
		return errors.New("standby has not replayed the captured primary WAL barrier")
	}
	return nil
}
