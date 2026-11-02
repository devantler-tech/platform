package longhorninventory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	common "github.com/siderolabs/talos/pkg/machinery/api/common"
	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

type fileStream struct {
	grpc.ClientStream
	items []*machine.FileInfo
	err   error
}

func (s *fileStream) Recv() (*machine.FileInfo, error) {
	if len(s.items) == 0 {
		return nil, s.err
	}
	item := s.items[0]
	s.items = s.items[1:]
	return item, nil
}

type dataStream struct {
	grpc.ClientStream
	items []*common.Data
	err   error
}

func (s *dataStream) Recv() (*common.Data, error) {
	if len(s.items) == 0 {
		return nil, s.err
	}
	item := s.items[0]
	s.items = s.items[1:]
	return item, nil
}

type fileAPI struct {
	list func(context.Context, *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error)
	read func(context.Context, *machine.ReadRequest) (grpc.ServerStreamingClient[common.Data], error)
}

func (a fileAPI) List(ctx context.Context, request *machine.ListRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[machine.FileInfo], error) {
	return a.list(ctx, request)
}

func (a fileAPI) Read(ctx context.Context, request *machine.ReadRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[common.Data], error) {
	return a.read(ctx, request)
}

func nativeEntry(name string, dir bool) *machine.FileInfo {
	mode := os.FileMode(0600)
	if dir {
		mode = os.ModeDir | 0700
	}
	full := ReplicaRoot
	if name != "." {
		full += "/" + name
	}
	return &machine.FileInfo{Name: full, RelativeName: name, Mode: uint32(mode), IsDir: dir, Modified: 100}
}

func envelope(t *testing.T, value string) *machine.FileInfo {
	t.Helper()
	data, err := json.Marshal(map[string]any{"name": ReplicaRoot + "/unreported", "relativeName": "unreported", "mode": os.ModeDir, "isDir": true, "metadata": json.RawMessage(value)})
	if err != nil {
		t.Fatal(err)
	}
	m := new(machine.FileInfo)
	if err := protojson.Unmarshal(data, m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNativeListingRequiresTrueEOFAndQualifiedEveryEntry(t *testing.T) {
	root, child := nativeEntry(".", true), nativeEntry("unreported", true)
	for _, test := range []struct {
		name  string
		items []*machine.FileInfo
		err   error
	}{
		{"partial transport", []*machine.FileInfo{root, child}, errors.New("disconnected")},
		{"EOF joined with failure", []*machine.FileInfo{root, child}, errors.Join(io.EOF, errors.New("disconnected"))},
		{"nil entry", []*machine.FileInfo{root, nil}, io.EOF},
		{"empty stream", nil, io.EOF},
		{"missing root", []*machine.FileInfo{child}, io.EOF},
		{"duplicate", []*machine.FileInfo{root, child, child}, io.EOF},
		{"native error", []*machine.FileInfo{root, {Name: ReplicaRoot + "/hidden", RelativeName: "hidden", Error: "denied"}}, io.EOF},
		{"envelope error", []*machine.FileInfo{root, envelope(t, `{"error":"denied"}`)}, io.EOF},
		{"deadline status", []*machine.FileInfo{root, envelope(t, `{"status":{"code":4}}`)}, io.EOF},
		{"wrong target", []*machine.FileInfo{root, envelope(t, `{"hostname":"10.0.0.9"}`)}, io.EOF},
		{"escaped full name", []*machine.FileInfo{root, {Name: "/other/unreported", RelativeName: "unreported", Mode: uint32(os.ModeDir), IsDir: true}}, io.EOF},
		{"symlink", []*machine.FileInfo{root, {Name: ReplicaRoot + "/link", RelativeName: "link", Link: "/elsewhere", Mode: uint32(os.ModeSymlink)}}, io.EOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := Native{Target: "10.0.0.1", API: fileAPI{list: func(context.Context, *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
				return &fileStream{items: slices.Clone(test.items), err: test.err}, nil
			}}}
			if _, err := reader.ListReplicas(context.Background()); err == nil {
				t.Fatal("incomplete or ambiguous native evidence accepted")
			}
		})
	}
}

func TestNativeListingIsFixedReadOnlyBoundedAndRetainsUnreportedData(t *testing.T) {
	reader := Native{Target: "10.0.0.1", API: fileAPI{list: func(ctx context.Context, request *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
		md, _ := metadata.FromOutgoingContext(ctx)
		if _, bounded := ctx.Deadline(); !bounded || request.GetRoot() != ReplicaRoot || request.GetRecurse() || request.GetRecursionDepth() != 1 || len(request.GetTypes()) != 0 || request.GetReportXattrs() || !slices.Equal(md.Get("node"), []string{"10.0.0.1"}) || len(md.Get("nodes")) != 0 {
			t.Fatal("native read escaped its fixed scope, routing or deadline")
		}
		return &fileStream{items: []*machine.FileInfo{nativeEntry(".", true), envelope(t, `{"hostname":"10.0.0.1","status":{"code":0}}`)}, err: io.EOF}, nil
	}}}
	listing, err := reader.ListReplicas(context.Background())
	if err != nil || !listing.Complete || len(listing.Entries) != 2 || listing.Entries[1].Name != "unreported" {
		t.Fatal("complete native listing lost data")
	}
}

func TestBootReadRejectsPartialAndErrorEnvelopes(t *testing.T) {
	const boot = "12345678-1234-4234-8234-123456789abc\n"
	for _, test := range []struct {
		items []*common.Data
		err   error
		valid bool
	}{
		{[]*common.Data{{Bytes: []byte(boot)}}, io.EOF, true},
		{[]*common.Data{{Bytes: []byte(boot)}}, errors.New("disconnected"), false},
		{[]*common.Data{{Bytes: []byte(boot), Metadata: envelope(t, `{"status":{"code":1}}`).GetMetadata()}}, io.EOF, false},
		{[]*common.Data{{Bytes: []byte(boot[:36])}}, io.EOF, false},
		{[]*common.Data{{Bytes: []byte(boot + boot)}}, io.EOF, false},
		{[]*common.Data{nil}, io.EOF, false},
		{nil, io.EOF, false},
	} {
		reader := Native{Target: "10.0.0.1", API: fileAPI{read: func(ctx context.Context, request *machine.ReadRequest) (grpc.ServerStreamingClient[common.Data], error) {
			if _, bounded := ctx.Deadline(); !bounded || request.GetPath() != "/proc/sys/kernel/random/boot_id" {
				t.Fatal("boot identity read escaped its fixed scope or deadline")
			}
			return &dataStream{items: slices.Clone(test.items), err: test.err}, nil
		}}}
		_, err := reader.BootID(context.Background())
		if (err == nil) != test.valid {
			t.Fatal("boot read accepted incomplete evidence or rejected complete evidence")
		}
	}
}

type lateCancellation struct {
	context.Context
	expires time.Time
}

func (c lateCancellation) Deadline() (time.Time, bool) { return c.expires, true }

func TestNativeReadsDoNotTrustDelayedCancellationDelivery(t *testing.T) {
	ctx := lateCancellation{Context: context.Background(), expires: time.Now().Add(-time.Second)}
	requests := 0
	reader := Native{Target: "10.0.0.1", API: fileAPI{
		list: func(context.Context, *machine.ListRequest) (grpc.ServerStreamingClient[machine.FileInfo], error) {
			requests++
			return &fileStream{items: []*machine.FileInfo{nativeEntry(".", true)}, err: io.EOF}, nil
		},
		read: func(context.Context, *machine.ReadRequest) (grpc.ServerStreamingClient[common.Data], error) {
			requests++
			return &dataStream{items: []*common.Data{{Bytes: []byte("12345678-1234-4234-8234-123456789abc\n")}}, err: io.EOF}, nil
		},
	}}
	if _, err := reader.ListReplicas(ctx); err == nil {
		t.Fatal("expired listing was accepted before cancellation delivery")
	}
	if _, err := reader.BootID(ctx); err == nil {
		t.Fatal("expired boot read was accepted before cancellation delivery")
	}
	if requests != 0 {
		t.Fatal("expired authorization started a native read")
	}
}
