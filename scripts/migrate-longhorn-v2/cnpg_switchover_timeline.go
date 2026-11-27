package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// Read only the timeline history for the current control-file timeline. The
// filename is derived from PostgreSQL's own numeric timeline inside the query.
const umamiTimelineSQL = `SELECT pg_catalog.json_build_object('system_id',(pg_catalog.pg_control_system()).system_identifier::text,'timeline',checkpoint.timeline_id::text,'in_recovery',pg_catalog.pg_is_in_recovery(),'history',pg_catalog.pg_read_file('pg_wal/'||pg_catalog.lpad(pg_catalog.upper(pg_catalog.to_hex(checkpoint.timeline_id)),8,'0')||'.history',0,65537,true)) FROM pg_catalog.pg_control_checkpoint() AS checkpoint;`

func verifyUmamiTimeline(ctx context.Context, c client, cluster, writer object, previous, promoted postgresState) error {
	raw, err := c.readBoundPostgresSQL(ctx, cluster, writer, umamiTimelineSQL)
	var native struct {
		SystemID   string `json:"system_id"`
		Timeline   string `json:"timeline"`
		InRecovery *bool  `json:"in_recovery"`
		History    string `json:"history"`
	}
	if err != nil || len(raw) > 128*1024 || json.Unmarshal(raw, &native) != nil ||
		native.SystemID != promoted.SystemID || native.Timeline != promoted.Timeline ||
		native.InRecovery == nil || *native.InRecovery {
		return errors.New("the promoted writer's timeline file is unverified")
	}
	return verifyUmamiTimelineParent(native.History, previous, promoted)
}

func verifyUmamiTimelineParent(history string, previous, promoted postgresState) error {
	refused := errors.New("the promoted timeline does not prove continuity at the original writer's WAL barrier")
	if len(history) == 0 || len(history) > 65536 || !strings.HasSuffix(history, "\n") ||
		validPostgresState(previous) != nil || validPostgresState(promoted) != nil ||
		previous.InRecovery || promoted.InRecovery || previous.SystemID != promoted.SystemID {
		return refused
	}
	oldTimeline, _ := strconv.ParseUint(previous.Timeline, 10, 32)
	newTimeline, _ := strconv.ParseUint(promoted.Timeline, 10, 32)
	oldFlush, _ := walPosition(previous.FlushLSN)
	newFlush, _ := walPosition(promoted.FlushLSN)
	var parent, fork uint64
	for line := range strings.SplitSeq(history, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return refused
		}
		timeline, parseErr := strconv.ParseUint(fields[0], 10, 32)
		lsn, walErr := walPosition(fields[1])
		if parseErr != nil || walErr != nil || timeline <= parent || timeline >= newTimeline || lsn < fork {
			return refused
		}
		parent, fork = timeline, lsn
	}
	if parent != oldTimeline || fork < oldFlush || fork > newFlush {
		return refused
	}
	return nil
}
