package longhorninventory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func storageRootEntry(name string, dir bool) *machine.FileInfo {
	item := nativeEntry(name, dir)
	item.Name = strings.Replace(item.Name, ReplicaRoot, StorageRoot, 1)
	return item
}

func TestStorageRootObservationReportsPresenceWithoutInventingReplicaEntries(t *testing.T) {
	api := fileAPI{list: func(_ context.Context, r *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
		if r.Root != StorageRoot || r.RecursionDepth != 1 {
			t.Fatal("root observation escaped its fixed parent path")
		}
		return &fileStream{items: []*machine.FileInfo{storageRootEntry(".", true), storageRootEntry("engine-binaries", true), storageRootEntry("private-data-canary", true)}, err: io.EOF}, nil
	}}
	listing, err := (Native{API: api, Target: "10.0.0.8"}).ListStorageRoot(context.Background())
	if err != nil || !listing.Complete || len(listing.Entries) != 3 {
		t.Fatal("complete parent directory was not observed:", err)
	}
	report, err := AnalyzeStorageRoot(listing)
	if err != nil || report.ReplicaEntryPresent || report.ReplicaDirectoryPresent || !report.EngineBinariesPresent || report.OtherEntries != 1 || len(report.Digest) != 64 {
		t.Fatal("parent presence metadata was lost:", err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "private-data-canary") || strings.Contains(string(encoded), "EmptyAndAccounted") {
		t.Fatal("root metadata exposed names or claimed replica/reset clearance")
	}
	listing.Entries = append(listing.Entries, Entry{Name: "replicas", Mode: 0600})
	report, err = AnalyzeStorageRoot(listing)
	if err != nil || !report.ReplicaEntryPresent || report.ReplicaDirectoryPresent {
		t.Fatal("a file named replicas was represented as an absent path")
	}
}

func TestNativeFailureCodesRemainClosedAndNeverExposeProviderText(t *testing.T) {
	for _, test := range []struct {
		name, code         string
		apiErr, errorAtEnd error
		items              []*machine.FileInfo
	}{
		{name: "not found", code: "not-found", apiErr: status.Error(codes.NotFound, "private-path")},
		{name: "permission", code: "permission", apiErr: status.Error(codes.PermissionDenied, "private-path")},
		{name: "stream not found", code: "not-found", errorAtEnd: status.Error(codes.NotFound, "private-path")},
		{name: "stream permission", code: "permission", errorAtEnd: status.Error(codes.PermissionDenied, "private-path")},
		{name: "partial", code: "stream", errorAtEnd: errors.New("private-path"), items: []*machine.FileInfo{nativeEntry(".", true)}},
		{name: "wrapped EOF", code: "stream", errorAtEnd: errors.Join(io.EOF, errors.New("private-path")), items: []*machine.FileInfo{nativeEntry(".", true)}},
		{name: "entry error", code: "entry-error", errorAtEnd: io.EOF, items: []*machine.FileInfo{{Error: "private-path"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := fileAPI{list: func(context.Context, *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
				if test.apiErr != nil {
					return nil, test.apiErr
				}
				return &fileStream{items: test.items, err: test.errorAtEnd}, nil
			}}
			listing, err := (Native{API: api, Target: "10.0.0.8"}).ListReplicas(context.Background())
			if err == nil || listing.Complete || FailureCode(err) != test.code || strings.Contains(err.Error(), "private-path") {
				t.Fatal("native failure was accepted, unclassified or leaked:", err, FailureCode(err))
			}
		})
	}
	if FailureCode(errors.New("not-found private-path")) != "unknown" {
		t.Fatal("provider-controlled error text selected a diagnostic class")
	}
}
