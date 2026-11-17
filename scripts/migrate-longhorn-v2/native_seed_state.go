package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type canarySeedState struct {
	Version                                                                                int
	Source, Run, Phase, PayloadSHA256, Snapshot, LocationSpec, RepositorySpec, CompletedAt string
	Location, Backup, Transfer, Repository                                                 benchmarkResource
}

var seedPhases = []string{"created", "source-write-requested", "payload-verified", "backup-create-requested", "backup-owned", "backup-verified", "source-retirement-requested", "source-cleaned"}

func seedPhaseIndex(phase string) int {
	for i, p := range seedPhases {
		if p == phase {
			return i
		}
	}
	return -1
}

var seedSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errSeedEvidence = errors.New("the canary seed lacks its original owned transfer or retirement evidence")

func (s canarySeedState) validate(r benchmarkRecord) error {
	rank := seedPhaseIndex(s.Phase)
	if rank < 0 || s.Version != 1 || s.Source != r.Source || s.Run != r.Run || !validBenchmarkOwner(s.Run, s.Source) ||
		r.Version != 3 || r.Phase != "v1" || r.Namespace != benchmarkNamespace || r.Workload != nativeSeedWorkload() || r.Image != fioImage ||
		r.Cleaned || len(r.Measurements) != 0 || r.CompletedAt != "" || r.Recovery != nil || r.PendingCreate != "" || validateBenchmarkResources(r) != nil || validateBenchmarkRuntimeReceipt(r) != nil ||
		s.Location.Name != "default" || s.Location.Namespace != "velero" || s.Location.UID == "" || !seedSHA256.MatchString(s.LocationSpec) ||
		s.Backup.Name != "longhorn-v2-seed-"+r.Run || s.Backup.Namespace != "velero" {
		return errSeedEvidence
	}
	if rank >= 1 && len(r.Resources) != 7 {
		return errSeedEvidence
	}
	if rank < 2 && s.PayloadSHA256 != "" || rank >= 2 && !seedSHA256.MatchString(s.PayloadSHA256) {
		return errSeedEvidence
	}
	if rank < 4 && s.Backup.UID != "" || rank >= 4 && s.Backup.UID == "" {
		return errSeedEvidence
	}
	if rank < 5 {
		if s.Transfer != (benchmarkResource{}) || s.Repository != (benchmarkResource{}) || s.RepositorySpec != "" || s.Snapshot != "" {
			return errSeedEvidence
		}
	} else if s.Transfer.Namespace != "velero" || s.Repository.Namespace != "velero" || !seedSHA256.MatchString(s.RepositorySpec) ||
		s.Snapshot == "" || len(s.Snapshot) > 256 || strings.TrimSpace(s.Snapshot) != s.Snapshot || strings.IndexFunc(s.Snapshot, unicode.IsControl) >= 0 {
		return errSeedEvidence
	}
	seen := map[string]bool{}
	for _, identity := range r.Resources {
		seen[identity.UID] = true
	}
	for _, identity := range []benchmarkResource{s.Location, s.Backup, s.Transfer, s.Repository} {
		if identity.UID == "" {
			continue
		}
		if !kubeName.MatchString(identity.Name) || len(identity.Name) > 253 || len(identity.UID) > 128 || seen[identity.UID] {
			return errSeedEvidence
		}
		seen[identity.UID] = true
	}
	if rank >= 5 && (s.Transfer.UID == "" || s.Repository.UID == "") {
		return errSeedEvidence
	}
	if rank < 7 && s.CompletedAt != "" {
		return errSeedEvidence
	}
	if rank == 7 {
		// This is retirement evidence, not a benchmark result. Reuse the
		// completed-fence checks without claiming measurements or Cleaned.
		retired := r
		retired.Cleaned = true
		if validateBenchmarkFenceReceipts(retired) != nil || retired.FenceCheckpoint == "" {
			return errSeedEvidence
		}
		if completed, err := time.Parse(time.RFC3339Nano, s.CompletedAt); err != nil || completed.IsZero() {
			return errSeedEvidence
		}
	}
	return nil
}

func decodeCanarySeedState(raw string, r benchmarkRecord) (canarySeedState, error) {
	var s canarySeedState
	fields := map[string][]string{"": {"Version", "Source", "Run", "Phase", "PayloadSHA256", "Snapshot", "LocationSpec", "RepositorySpec", "CompletedAt", "Location", "Backup", "Transfer", "Repository"}}
	for _, name := range []string{"Location", "Backup", "Transfer", "Repository"} {
		fields[name] = []string{"Name", "Namespace", "UID"}
	}
	if len(raw) == 0 || len(raw) > 32<<10 || strictjson.Validate([]byte(raw), fields) != nil {
		return s, errSeedEvidence
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if d.Decode(&s) != nil || s.validate(r) != nil {
		return canarySeedState{}, errSeedEvidence
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return canarySeedState{}, errSeedEvidence
	}
	return s, nil
}

func validateSeedTransition(before, after canarySeedState, r benchmarkRecord) error {
	if before.validate(r) != nil || after.validate(r) != nil || seedPhaseIndex(after.Phase) != seedPhaseIndex(before.Phase)+1 ||
		before.Version != after.Version || before.Source != after.Source || before.Run != after.Run || before.Location != after.Location || before.LocationSpec != after.LocationSpec {
		return errSeedEvidence
	}
	if before.PayloadSHA256 != "" && before.PayloadSHA256 != after.PayloadSHA256 ||
		before.Backup.UID != "" && before.Backup != after.Backup || before.Transfer.UID != "" && before.Transfer != after.Transfer ||
		before.Repository.UID != "" && before.Repository != after.Repository || before.RepositorySpec != "" && before.RepositorySpec != after.RepositorySpec ||
		before.Snapshot != "" && before.Snapshot != after.Snapshot || before.CompletedAt != "" && before.CompletedAt != after.CompletedAt {
		return errSeedEvidence
	}
	return nil
}
