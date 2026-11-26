package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type nativeRestoreTarget struct {
	Name, UID   string
	Preparation nativeRestorePreparationReceipt
}

type nativeRestoreState struct {
	Version                                                                   int
	Source, Run, Phase, SeedDigest, Token, FirstNode, SecondNode, CompletedAt string
	SeedJournal                                                               benchmarkResource
	Targets                                                                   []nativeRestoreTarget
	Resources                                                                 map[string]benchmarkResource
	Expired                                                                   *nativeRestoreExpiredReceipt `json:",omitempty"`
}

// Every direct CREATE/DELETE and the non-replayable write has its own intent.
// Controller-created bindings and restore children are recorded only after
// their original parent is owned. Never replace FirstPod with SecondPod.
// Completion covers this functional data path; it is not a storage-cleanup,
// benchmark or platform-migration acceptance receipt.
var nativeRestorePhases = []struct {
	name      string
	resources []string
}{
	{"created", nil},
	{"target-tags-requested", nil},
	{"target-tags-owned", nil},
	{"policy-create-requested", nil},
	{"policy-owned", []string{"NetworkPolicy"}},
	{"class-create-requested", nil},
	{"class-owned", []string{"StorageClass"}},
	{"claim-create-requested", nil},
	{"claim-owned", []string{"PersistentVolumeClaim"}},
	{"storage-bound", []string{"PersistentVolume", "LonghornVolume"}},
	{"modifier-create-requested", nil},
	{"modifier-owned", []string{"ConfigMap"}},
	{"restore-create-requested", nil},
	{"restore-owned", []string{"Restore"}},
	{"restore-verified", []string{"FirstPod", "PodVolumeRestore"}},
	{"write-requested", nil},
	{"write-verified", nil},
	{"first-pod-delete-requested", nil},
	{"first-pod-deleted", nil},
	{"reattach-create-requested", nil},
	{"reattach-owned", []string{"SecondPod"}},
	{"reattach-verified", nil},
	{"completed", nil},
}

var errNativeRestoreEvidence = errors.New("the native restore lacks its original seed, target or consumer receipts")

func nativeRestorePhaseIndex(phase string) int {
	if nativeRestoreExpiredStage(phase) > 0 {
		phase = "restore-owned"
	}
	for i, step := range nativeRestorePhases {
		if step.name == phase {
			return i
		}
	}
	return -1
}

func nativeRestoreSeedDigest(record benchmarkRecord, seed canarySeedState) (string, error) {
	if seed.validate(record) != nil || seed.Phase != "source-cleaned" {
		return "", errNativeRestoreEvidence
	}
	data, err := json.Marshal(struct {
		Record benchmarkRecord
		Seed   canarySeedState
	}{record, seed})
	if err != nil {
		return "", errNativeRestoreEvidence
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func nativeRestoreResourceName(kind, run string) (string, string) {
	switch kind {
	case "NetworkPolicy":
		return "fio-default-deny", benchmarkNamespace
	case "StorageClass":
		return "longhorn-native-v2-" + run, ""
	case "PersistentVolumeClaim":
		return "fio-data", benchmarkNamespace
	case "FirstPod", "SecondPod":
		return "fio", benchmarkNamespace
	case "ConfigMap":
		return "longhorn-native-v2-" + run + "-modifiers", "velero"
	case "Restore":
		return "longhorn-native-v2-" + run, "velero"
	case "PodVolumeRestore":
		return "", "velero"
	case "LonghornVolume":
		return "", "longhorn-system"
	default:
		return "", ""
	}
}

func nativeRestoreUID(uid string) bool {
	return uid != "" && len(uid) <= 128 && strings.TrimSpace(uid) == uid && strings.IndexFunc(uid, unicode.IsControl) < 0
}

func (s nativeRestoreState) validate(record benchmarkRecord, seed canarySeedState) error {
	rank := nativeRestorePhaseIndex(s.Phase)
	digest, err := nativeRestoreSeedDigest(record, seed)
	if err != nil || rank < 0 || s.Version != 1 || !validBenchmarkOwner(s.Run, s.Source) || s.Run == record.Run || s.SeedDigest != digest ||
		s.SeedJournal.Name != nativeSeedJournal || s.SeedJournal.Namespace != "longhorn-system" || !nativeRestoreUID(s.SeedJournal.UID) || !nativeRestoreFixedRoster(s.Targets) {
		return errNativeRestoreEvidence
	}
	seen := map[string]bool{}
	for _, identity := range record.Resources {
		seen[identity.UID] = true
	}
	for _, identity := range []benchmarkResource{seed.Location, seed.Backup, seed.Transfer, seed.Repository, s.SeedJournal, s.Targets[0].Preparation.Activation} {
		if seen[identity.UID] {
			return errNativeRestoreEvidence
		}
		seen[identity.UID] = true
	}
	nodes := map[string]bool{}
	previous := ""
	for _, target := range s.Targets {
		_, profileErr := target.Preparation.mapper(target.Name)
		if profileErr != nil || !kubeName.MatchString(target.Name) || len(target.Name) > 63 || target.Name <= previous || !nativeRestoreUID(target.UID) || seen[target.UID] || seen[target.Preparation.Journal.UID] || target.UID == target.Preparation.Journal.UID {
			return errNativeRestoreEvidence
		}
		previous, nodes[target.Name], seen[target.UID] = target.Name, true, true
		seen[target.Preparation.Journal.UID] = true
	}
	required := map[string]bool{}
	for _, step := range nativeRestorePhases[:rank+1] {
		for _, kind := range step.resources {
			required[kind] = true
		}
	}
	if len(s.Resources) != len(required) {
		return errNativeRestoreEvidence
	}
	for kind, identity := range s.Resources {
		name, namespace := nativeRestoreResourceName(kind, s.Run)
		if !required[kind] || !nativeRestoreUID(identity.UID) || seen[identity.UID] || identity.Namespace != namespace ||
			!kubeName.MatchString(identity.Name) || len(identity.Name) > 253 || name != "" && identity.Name != name {
			return errNativeRestoreEvidence
		}
		if (kind == "PersistentVolume" || kind == "LonghornVolume") && identity.Name == record.Resources[kind].Name {
			return errNativeRestoreEvidence
		}
		seen[identity.UID] = true
	}
	if !validNativeRestoreExpiredReceipt(s, seen) {
		return errNativeRestoreEvidence
	}
	if rank < nativeRestorePhaseIndex("restore-verified") && s.FirstNode != "" || rank >= nativeRestorePhaseIndex("restore-verified") && !nodes[s.FirstNode] ||
		rank < nativeRestorePhaseIndex("write-requested") && s.Token != "" || rank >= nativeRestorePhaseIndex("write-requested") && !seedSHA256.MatchString(s.Token) ||
		rank < nativeRestorePhaseIndex("reattach-verified") && s.SecondNode != "" || rank >= nativeRestorePhaseIndex("reattach-verified") && (!nodes[s.SecondNode] || s.FirstNode == s.SecondNode) {
		return errNativeRestoreEvidence
	}
	if s.Phase != "completed" {
		if s.CompletedAt != "" {
			return errNativeRestoreEvidence
		}
		return nil
	}
	completed, err := time.Parse(time.RFC3339Nano, s.CompletedAt)
	seedCompleted, seedErr := time.Parse(time.RFC3339Nano, seed.CompletedAt)
	if err != nil || seedErr != nil || completed.IsZero() || !completed.After(seedCompleted) {
		return errNativeRestoreEvidence
	}
	return nil
}

func validateNativeRestoreTransition(before, after nativeRestoreState, record benchmarkRecord, seed canarySeedState) error {
	if before.validate(record, seed) != nil || after.validate(record, seed) != nil || !validNativeRestorePhaseTransition(before, after) ||
		before.Version != after.Version || before.Source != after.Source || before.Run != after.Run || before.SeedDigest != after.SeedDigest || before.SeedJournal != after.SeedJournal ||
		!reflect.DeepEqual(before.Targets, after.Targets) || before.Token != "" && before.Token != after.Token ||
		before.FirstNode != "" && before.FirstNode != after.FirstNode || before.SecondNode != "" && before.SecondNode != after.SecondNode ||
		before.CompletedAt != "" && before.CompletedAt != after.CompletedAt {
		return errNativeRestoreEvidence
	}
	for kind, original := range before.Resources {
		if after.Resources[kind] != original {
			return errNativeRestoreEvidence
		}
	}
	return nil
}

func decodeNativeRestoreState(raw string, record benchmarkRecord, seed canarySeedState) (nativeRestoreState, error) {
	var state nativeRestoreState
	fields := map[string][]string{
		"":                                 {"Version", "Source", "Run", "Phase", "SeedDigest", "Token", "FirstNode", "SecondNode", "CompletedAt", "SeedJournal", "Targets", "Resources", "Expired"},
		"SeedJournal":                      {"Name", "Namespace", "UID"},
		"Targets[]":                        {"Name", "UID", "Preparation"},
		"Targets[].Preparation":            {"Profile", "JournalSHA256", "BootID", "RawPartitionUUID", "Journal", "ProviderID", "MachineUUID", "LonghornUID", "DiskUUID", "Activation", "ActivationSHA256"},
		"Targets[].Preparation.Journal":    {"Name", "Namespace", "UID"},
		"Targets[].Preparation.Activation": {"Name", "Namespace", "UID"},
		"Expired":                          {"Pod", "Transfer", "Replacement"},
		"Expired.Pod":                      {"Name", "Namespace", "UID"},
		"Expired.Transfer":                 {"Name", "Namespace", "UID"},
		"Expired.Replacement":              {"Name", "Namespace", "UID"},
	}
	for _, step := range nativeRestorePhases {
		for _, kind := range step.resources {
			fields["Resources."+kind] = []string{"Name", "Namespace", "UID"}
		}
	}
	if len(raw) == 0 || len(raw) > 32<<10 || strictjson.Validate([]byte(raw), fields) != nil {
		return state, errNativeRestoreEvidence
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil || state.validate(record, seed) != nil {
		return nativeRestoreState{}, errNativeRestoreEvidence
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nativeRestoreState{}, errNativeRestoreEvidence
	}
	return state, nil
}
