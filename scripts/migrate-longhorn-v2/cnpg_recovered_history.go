package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// The history file survives a normal restart, unlike pg_last_wal_replay_lsn.
// Only the server's own numeric timeline selects the bounded pg_wal file.
const postgresRecoverySQL = `SELECT pg_catalog.json_build_object('system_id',(pg_catalog.pg_control_system()).system_identifier::text,'timeline',(pg_catalog.pg_control_checkpoint()).timeline_id::text,'flush_lsn',pg_catalog.pg_current_wal_flush_lsn()::text,'replay_lsn',pg_catalog.pg_last_wal_replay_lsn()::text,'in_recovery',pg_catalog.pg_is_in_recovery(),'postmaster_start',pg_catalog.pg_postmaster_start_time()::text,'data_directory',pg_catalog.current_setting('data_directory'),'tablespaces_builtin_only',NOT EXISTS (SELECT 1 FROM pg_catalog.pg_tablespace WHERE oid NOT IN (1663::oid,1664::oid)),'tblspc_empty',NOT EXISTS (SELECT 1 FROM pg_catalog.pg_ls_dir('pg_tblspc',false,false)),'timeline_history',pg_catalog.pg_read_file('pg_wal/' || pg_catalog.lpad(pg_catalog.upper(pg_catalog.to_hex((pg_catalog.pg_control_checkpoint()).timeline_id::bigint)),8,'0') || '.history',0,65536,true));`

// verifyCNPGRecoveredHistory proves a new primary's direct fork reached the selected barrier.
// The caller still needs the owned fresh-backup graph, admitted spec and actual V2 storage.
func verifyCNPGRecoveredHistory(raw []byte, barrier postgresState) error {
	refused := errors.New("restored primary has not proven the recorded physical recovery target")
	fields := map[string][]string{"": {"system_id", "timeline", "flush_lsn", "replay_lsn", "in_recovery", "postmaster_start", "timeline_history", "data_directory", "tablespaces_builtin_only", "tblspc_empty"}}
	if len(raw) > 1<<20 || strictjson.Validate(raw, fields) != nil || validPostgresState(barrier) != nil || barrier.InRecovery {
		return refused
	}
	current, err := parsePostgresState(raw)
	var observation struct {
		postgresState
		History                  *string `json:"timeline_history"`
		Directory                *string `json:"data_directory"`
		TablespacesBuiltinOnly   *bool   `json:"tablespaces_builtin_only"`
		TablespaceDirectoryEmpty *bool   `json:"tblspc_empty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err != nil || decoder.Decode(&observation) != nil || observation.History == nil || len(*observation.History) == 0 || len(*observation.History) >= 65536 || observation.Directory == nil || *observation.Directory != "/var/lib/postgresql/data/pgdata" || current.PostmasterStart == "" || current.InRecovery || current.SystemID != barrier.SystemID || observation.TablespacesBuiltinOnly == nil || !*observation.TablespacesBuiltinOnly || observation.TablespaceDirectoryEmpty == nil || !*observation.TablespaceDirectoryEmpty {
		return refused
	}
	sourceTimeline, _ := strconv.ParseUint(barrier.Timeline, 10, 32)
	currentTimeline, _ := strconv.ParseUint(current.Timeline, 10, 32)
	target, _ := walPosition(barrier.FlushLSN)
	durable, _ := walPosition(current.FlushLSN)
	if currentTimeline <= sourceTimeline || durable < target {
		return refused
	}
	if current.ReplayLSN != "" {
		replayed, e := walPosition(current.ReplayLSN)
		if e != nil || replayed < target || replayed > durable {
			return refused
		}
	}
	var parent, position uint64
	for _, line := range strings.Split(*observation.History, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return refused
		}
		timeline, e := strconv.ParseUint(fields[0], 10, 32)
		if e != nil || timeline == 0 || strconv.FormatUint(timeline, 10) != fields[0] || timeline <= parent || timeline >= currentTimeline {
			return refused
		}
		fork, e := walPosition(fields[1])
		if e != nil || fork < position || fork > durable {
			return refused
		}
		parent, position = timeline, fork
	}
	if parent != sourceTimeline || position < target {
		return refused
	}
	return nil
}
