package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PostgreSQL does not guarantee that last_archived_wal is the highest completed
// segment. Read the exact .done marker instead; a recycled/missing marker is
// unknown, never success. An actual isolated restore must still verify bytes.
const postgresArchiveSQL = `WITH target AS (SELECT '__TARGET_LSN__'::pg_catalog.pg_lsn AS lsn), segment AS (SELECT lsn, pg_catalog.pg_walfile_name(lsn) AS wal FROM target), marker AS (SELECT lsn, wal, pg_catalog.pg_stat_file('pg_wal/archive_status/' || wal || '.done', true) AS info FROM segment) SELECT pg_catalog.json_build_object('system_id',(pg_catalog.pg_control_system()).system_identifier::text,'timeline',(pg_catalog.pg_control_checkpoint()).timeline_id::text,'flush_lsn',pg_catalog.pg_current_wal_flush_lsn()::text,'in_recovery',pg_catalog.pg_is_in_recovery(),'target_lsn',lsn::text,'required_wal',wal,'archive_done',coalesce((info).size=0 AND NOT (info).isdir,false)) FROM marker;`

var postgresWALSegment = regexp.MustCompile(`^[0-9A-F]{24}$`)

// cnpgArchiveQuery permits only a parsed WAL coordinate inside the fixed query.
func cnpgArchiveQuery(target string) (string, error) {
	if _, err := walPosition(target); err != nil {
		return "", err
	}
	return strings.ReplaceAll(postgresArchiveSQL, "__TARGET_LSN__", target), nil
}

// verifyCNPGArchiveMarker accepts the exact target segment, never a later archive statistic.
func verifyCNPGArchiveMarker(raw []byte, barrier postgresState) (bool, error) {
	refused := errors.New("archive marker lacks its exact target and physical source")
	if validPostgresState(barrier) != nil || barrier.InRecovery {
		return false, refused
	}
	current, err := parsePostgresState(raw)
	var marker struct {
		TargetLSN   string `json:"target_lsn"`
		RequiredWAL string `json:"required_wal"`
		Done        *bool  `json:"archive_done"`
	}
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Done == nil || current.InRecovery || current.SystemID != barrier.SystemID || current.Timeline != barrier.Timeline || marker.TargetLSN != barrier.FlushLSN || !postgresWALSegment.MatchString(marker.RequiredWAL) {
		return false, refused
	}
	timeline, _ := strconv.ParseUint(barrier.Timeline, 10, 32)
	wanted, _ := walPosition(barrier.FlushLSN)
	observed, _ := walPosition(current.FlushLSN)
	if !strings.HasPrefix(marker.RequiredWAL, fmt.Sprintf("%08X", timeline)) || observed < wanted {
		return false, refused
	}
	return *marker.Done, nil
}
