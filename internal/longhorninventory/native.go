package longhorninventory

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"time"

	common "github.com/siderolabs/talos/pkg/machinery/api/common"
	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const ReplicaRoot = "/var/lib/longhorn/replicas"

type Files interface {
	List(context.Context, *machine.ListRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[machine.FileInfo], error)
	Read(context.Context, *machine.ReadRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[common.Data], error)
}

type Native struct {
	API    Files
	Target string
}

const maxMessageBytes = 64 << 10

var errNative = errors.New("native file observation is incomplete, changed or outside its reviewed scope")
var bootFormat = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\n$`)

func (n Native) requestContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	address, err := netip.ParseAddr(n.Target)
	if err != nil || address.Zone() != "" || address.String() != n.Target || n.API == nil {
		return nil, nil, errNative
	}
	ctx, cancel := context.WithTimeout(talos.WithNode(ctx, n.Target), 15*time.Second)
	return ctx, cancel, nil
}

func (n Native) ListReplicas(ctx context.Context) (Listing, error) {
	ctx, cancel, err := n.requestContext(ctx)
	if err != nil {
		return Listing{}, err
	}
	defer cancel()
	stream, err := n.API.List(ctx, &machine.ListRequest{Root: ReplicaRoot, RecursionDepth: 1}, grpc.MaxCallRecvMsgSize(maxMessageBytes))
	if err != nil || stream == nil {
		return Listing{}, errNative
	}
	listing := Listing{}
	total := 0
	for {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF && ctx.Err() == nil {
			listing.Complete = true
			if _, err = Analyze(listing, nil); err != nil {
				return Listing{}, errNative
			}
			return listing, nil
		}
		if recvErr != nil || ctx.Err() != nil || item == nil {
			return Listing{}, errNative
		}
		size := proto.Size(item)
		total += size
		if size > maxMessageBytes || total > MaxMetadataBytes || len(listing.Entries) >= MaxEntries ||
			!n.validMetadata(item.Metadata) || item.Error != "" || item.Link != "" || len(item.Xattrs) != 0 ||
			(!baseName(item.RelativeName) && item.RelativeName != ".") || item.Name != path.Join(ReplicaRoot, item.RelativeName) {
			return Listing{}, errNative
		}
		listing.Entries = append(listing.Entries, Entry{Name: item.RelativeName, Mode: item.Mode, UID: item.Uid, GID: item.Gid,
			IsDir: item.IsDir, Size: item.Size, Modified: item.Modified})
	}
}

// BootID reads a fixed, exact-format kernel identity. It does not accept a
// successful stream ending after a partial file, or legacy error envelopes.
func (n Native) BootID(ctx context.Context) (string, error) {
	ctx, cancel, err := n.requestContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	stream, err := n.API.Read(ctx, &machine.ReadRequest{Path: "/proc/sys/kernel/random/boot_id"}, grpc.MaxCallRecvMsgSize(maxMessageBytes))
	if err != nil || stream == nil {
		return "", errNative
	}
	var content []byte
	for messages := 0; messages < 64; messages++ {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF && ctx.Err() == nil {
			if !bootFormat.Match(content) {
				return "", errNative
			}
			return strings.TrimSuffix(string(content), "\n"), nil
		}
		if recvErr != nil || ctx.Err() != nil || item == nil || proto.Size(item) > maxMessageBytes ||
			!n.validMetadata(item.Metadata) || len(content)+len(item.Bytes) > 37 {
			return "", errNative
		}
		content = append(content, item.Bytes...)
	}
	return "", errNative
}

func (n Native) validMetadata(metadata *common.Metadata) bool {
	if metadata == nil {
		// Native One2One routing does not inject a metadata envelope. Hardware and
		// boot identity must be bracketed independently by the caller.
		return true
	}
	// Talos 1.13's legacy fields can carry errors even on a successful transport.
	// Inspect their wire descriptors while using the current One2One API rather
	// than switching to the deprecated aggregating routing API.
	message := metadata.ProtoReflect()
	fields := message.Descriptor().Fields()
	if len(message.GetUnknown()) != 0 || message.Get(fields.ByName("error")).String() != "" {
		return false
	}
	hostname := message.Get(fields.ByName("hostname")).String()
	if hostname != "" && hostname != n.Target {
		return false
	}
	statusField := fields.ByName("status")
	if message.Has(statusField) {
		status := message.Get(statusField).Message()
		if status.Get(status.Descriptor().Fields().ByName("code")).Int() != 0 {
			return false
		}
	}
	return true
}
