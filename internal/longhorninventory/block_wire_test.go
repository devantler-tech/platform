package longhorninventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/cosi-project/runtime/api/v1alpha1"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type blockListAPIFixture struct {
	v1alpha1.StateClient
	stream grpc.ServerStreamingClient[v1alpha1.ListResponse]
}

func (f blockListAPIFixture) List(context.Context, *v1alpha1.ListRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[v1alpha1.ListResponse], error) {
	return f.stream, nil
}

type blockWireStream struct {
	grpc.ClientStream
	response *v1alpha1.ListResponse
	end      error
	calls    int
	repeat   bool
}

func (s *blockWireStream) Recv() (*v1alpha1.ListResponse, error) {
	s.calls++
	if s.calls == 1 || s.repeat && s.calls <= 130 {
		if s.repeat {
			response := proto.Clone(s.response).(*v1alpha1.ListResponse)
			response.Resource.Metadata.Id = fmt.Sprintf("entry-%03d", s.calls)
			return response, nil
		}
		return s.response, nil
	}
	return nil, s.end
}

func TestBoundedBlockWireRequiresCleanCompletionBeforeReturningAnyResource(t *testing.T) {
	for _, scenario := range []string{"complete", "joined EOF", "receive error", "entry bound", "message bound"} {
		t.Run(scenario, func(t *testing.T) {
			v := block.NewVolumeStatus(block.NamespaceName, "EPHEMERAL")
			encoded, err := protobuf.FromResource(v)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := encoded.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			response := &v1alpha1.ListResponse{Resource: wire}
			stream := &blockWireStream{response: response, end: io.EOF}
			switch scenario {
			case "joined EOF":
				stream.end = errors.Join(io.EOF, errors.New("private-provider-canary"))
			case "receive error":
				stream.end = errors.New("private-provider-canary")
			case "entry bound":
				stream.repeat = true
			case "message bound":
				response.ProtoReflect().SetUnknown(make([]byte, maxMessageBytes+1))
			}
			ctx, cancel := context.WithTimeout(talos.WithNode(context.Background(), "10.0.0.8"), 15*time.Second)
			defer cancel()
			got, err := (BoundedBlocks{API: blockListAPIFixture{stream: stream}, Target: "10.0.0.8"}).List(ctx, block.NewVolumeStatus(block.NamespaceName, "").Metadata())
			if (err == nil) != (scenario == "complete") {
				t.Fatal("stream completion or bounds were misclassified:", err)
			}
			if err != nil && len(got.Items) != 0 {
				t.Fatal("failed stream returned a resource prefix")
			}
			if err == nil {
				if len(got.Items) != 1 {
					t.Fatal("complete native stream lost its resource")
				}
				if _, ok := got.Items[0].(*block.VolumeStatus); !ok {
					t.Fatal("native type was not decoded")
				}
			}
			if stream.calls > 129 {
				t.Fatal("entry bound was not enforced while streaming")
			}
			if scenario == "entry bound" && stream.calls != 129 {
				t.Fatal("entry-limit path was not exercised")
			}
		})
	}
}
