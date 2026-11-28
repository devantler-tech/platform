package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
)

const (
	corootClickHouseMaxRows  = 2048
	corootClickHouseMaxBytes = 1024 * 1024
)

var errCorootClickHouseStorage = errors.New("the Coroot ClickHouse storage or replication evidence is unverified")

type corootClickHouseTable struct {
	Database string
	Name     string
	Engine   string
}

type corootClickHouseReplica struct {
	Database, Name, Engine, Replica, KeeperPath          string
	ReadOnly, SessionExpired, PartsToCheck, DelaySeconds string
	ActiveReplicas, TotalReplicas                        int
	QueueErrorAbsent, KeeperErrorAbsent                  int
}

type corootClickHouseStorageSummary struct {
	Tables, Replicated, Local, Views int
	MaxReplicaDelaySeconds           uint64
}

func readClickHouseStorageRows[T any](raw []byte) ([]T, error) {
	if len(raw) > corootClickHouseMaxBytes {
		return nil, errCorootClickHouseStorage
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("query returned no rows: %w", errCorootClickHouseStorage)
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	if len(lines) > corootClickHouseMaxRows {
		return nil, errCorootClickHouseStorage
	}
	rows := make([]T, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			return nil, errCorootClickHouseStorage
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var row T
		var trailing any
		if decoder.Decode(&row) != nil || decoder.Decode(&trailing) != io.EOF {
			return nil, errCorootClickHouseStorage
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func classifyCorootClickHouseStorage(members [2]string, tables [2][]corootClickHouseTable,
	replicas [2][]corootClickHouseReplica) (corootClickHouseStorageSummary, error) {
	var none, counts corootClickHouseStorageSummary
	if members[0] == "" || members[1] == "" || members[0] == members[1] {
		return none, errCorootClickHouseStorage
	}
	type tableName struct{ database, name string }
	var catalogs [2]map[tableName]string
	for member, rows := range tables {
		catalogs[member] = make(map[tableName]string, len(rows))
		for _, row := range rows {
			key := tableName{row.Database, row.Name}
			if row.Database == "" || row.Name == "" || row.Engine == "" || catalogs[member][key] != "" {
				return none, errCorootClickHouseStorage
			}
			catalogs[member][key] = row.Engine
		}
	}
	if len(catalogs[0]) == 0 || !maps.Equal(catalogs[0], catalogs[1]) {
		return none, fmt.Errorf("table catalogs differ or are empty: %w", errCorootClickHouseStorage)
	}
	replicated := make(map[tableName]string)
	counts.Tables = len(catalogs[0])
	for key, engine := range catalogs[0] {
		switch {
		case strings.HasPrefix(engine, "Replicated") && strings.HasSuffix(engine, "MergeTree"):
			replicated[key] = engine
			counts.Replicated++
		case engine == "View":
			counts.Views++
		default:
			counts.Local++
		}
	}
	if counts.Replicated == 0 {
		return none, fmt.Errorf("neither table catalog identifies replicated storage: %w", errCorootClickHouseStorage)
	}
	var paths [2]map[tableName]string
	for member, rows := range replicas {
		paths[member] = make(map[tableName]string, len(rows))
		for _, row := range rows {
			key := tableName{row.Database, row.Name}
			delay, err := strconv.ParseUint(row.DelaySeconds, 10, 64)
			if want, ok := replicated[key]; !ok || want != row.Engine || row.Replica != members[member] ||
				!strings.HasPrefix(row.KeeperPath, "/") || paths[member][key] != "" {
				return none, fmt.Errorf("replica identity did not match the table catalog for member %d: %w", member, errCorootClickHouseStorage)
			}
			var signals []string
			for _, check := range []struct {
				signal string
				ok     bool
			}{
				{"active replicas", row.ActiveReplicas == 2},
				{"total replicas", row.TotalReplicas == 2},
				{"read-only replica", row.ReadOnly == "0"},
				{"Keeper session", row.SessionExpired == "0"},
				{"parts pending verification", row.PartsToCheck == "0"},
				{"replica delay observation", err == nil},
				{"replication queue exception", row.QueueErrorAbsent == 1},
				{"Keeper exception", row.KeeperErrorAbsent == 1},
			} {
				if !check.ok {
					signals = append(signals, check.signal)
				}
			}
			if len(signals) != 0 {
				return none, fmt.Errorf("replication health was unverified for member %d (%s): %w",
					member, strings.Join(signals, ", "), errCorootClickHouseStorage)
			}
			paths[member][key] = row.KeeperPath
			counts.MaxReplicaDelaySeconds = max(counts.MaxReplicaDelaySeconds, delay)
		}
	}
	if len(paths[0]) != counts.Replicated || !maps.Equal(paths[0], paths[1]) {
		return none, fmt.Errorf("replicated tables do not have matching Keeper paths: %w", errCorootClickHouseStorage)
	}
	return counts, nil
}
