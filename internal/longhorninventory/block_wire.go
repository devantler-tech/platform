package longhorninventory

import (
	"context"
	"io"
	"net/netip"
	"time"

	"github.com/cosi-project/runtime/api/v1alpha1"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// BlockCollections lists Talos block resources of one kind.
type BlockCollections interface {
	List(context.Context, resource.Kind, ...state.ListOption) (resource.List, error)
}

// BlockListAPI is the COSI resource-list stream BoundedBlocks reads.
type BlockListAPI interface {
	List(context.Context, *v1alpha1.ListRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[v1alpha1.ListResponse], error)
}

// BoundedBlocks is a BlockCollections limited to the block namespace's volume
// status and discovered-volume kinds on the one node named by Target.
type BoundedBlocks struct {
	API    BlockListAPI
	Target string
}

// List reads one bounded collection, refusing any other kind, option or node,
// and any deadline missing or more than 15 seconds away.
func (r BoundedBlocks) List(ctx context.Context, kind resource.Kind, options ...state.ListOption) (resource.List, error) {
	invalid := nativeFailure{code: "block-list"}
	if ctx == nil || !active(ctx) || r.API == nil || kind == nil || len(options) != 0 || kind.Namespace() != block.NamespaceName {
		return resource.List{}, invalid
	}
	deadline, _ := ctx.Deadline()
	address, err := netip.ParseAddr(r.Target)
	md, _ := metadata.FromOutgoingContext(ctx)
	if err != nil || address.String() != r.Target || !netip.MustParsePrefix("10.0.0.0/16").Contains(address) || time.Until(deadline) > 15*time.Second || len(md.Get("nodes")) != 0 || len(md.Get("node")) != 1 || md.Get("node")[0] != r.Target {
		return resource.List{}, invalid
	}
	limit := 128
	switch kind.Type() {
	case block.VolumeStatusType:
	case block.DiscoveredVolumeType:
		limit = 512
	default:
		return resource.List{}, invalid
	}
	stream, err := r.API.List(ctx, &v1alpha1.ListRequest{Namespace: block.NamespaceName, Type: kind.Type(), Options: &v1alpha1.ListOptions{}}, grpc.MaxCallRecvMsgSize(maxMessageBytes))
	if err != nil || stream == nil {
		return resource.List{}, invalid
	}
	result := resource.List{}
	total := 0
	seen := map[string]bool{}
	for {
		response, recvErr := stream.Recv()
		if recvErr == io.EOF && active(ctx) {
			return result, nil
		}
		if recvErr != nil || response == nil || !active(ctx) {
			return resource.List{}, invalid
		}
		size := proto.Size(response)
		total += size
		if size > maxMessageBytes || total > MaxMetadataBytes || len(result.Items) >= limit || response.Resource == nil || len(response.ProtoReflect().GetUnknown()) != 0 {
			return resource.List{}, invalid
		}
		raw, decodeErr := protobuf.Unmarshal(response.Resource)
		if decodeErr != nil {
			return resource.List{}, invalid
		}
		item, decodeErr := protobuf.UnmarshalResource(raw)
		if decodeErr != nil || item == nil || item.Metadata().Namespace() != block.NamespaceName || item.Metadata().Type() != kind.Type() || item.Metadata().Phase() != resource.PhaseRunning || item.Metadata().ID() == "" || len(item.Metadata().ID()) > 256 || seen[item.Metadata().ID()] {
			return resource.List{}, invalid
		}
		seen[item.Metadata().ID()] = true
		result.Items = append(result.Items, item)
	}
}
