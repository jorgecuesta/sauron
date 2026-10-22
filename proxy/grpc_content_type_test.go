package proxy

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
)

// TestGRPCProxy_WireContentTypeIsProto (#23): the proxy must speak
// application/grpc+proto on both legs. "+raw" only works because grpc-go
// nodes fall back to proto for subtypes they do not know, a fallback grpc-go
// says it will remove.
func TestGRPCProxy_WireContentTypeIsProto(t *testing.T) {
	t.Parallel()

	gotBackend := make(chan string, 1)
	h := setupGRPCProxyTest(t, func(_ interface{}, stream grpc.ServerStream) error {
		md, _ := metadata.FromIncomingContext(stream.Context())
		gotBackend <- firstOr(md.Get("content-type"), "")
		var f rawFrame
		if err := stream.RecvMsg(&f); err != nil {
			return err
		}
		return stream.SendMsg(&rawFrame{payload: f.payload})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := h.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if err := stream.SendMsg(&rawFrame{payload: []byte("ping")}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	_ = stream.CloseSend()
	var resp rawFrame
	if err := stream.RecvMsg(&resp); err != nil || string(resp.payload) != "ping" {
		t.Fatalf("echo = %q, %v", resp.payload, err)
	}
	hdr, err := stream.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}

	if got := <-gotBackend; got != "application/grpc+proto" && got != "application/grpc" {
		t.Errorf("backend received content-type %q, want application/grpc+proto", got)
	}
	if got := firstOr(hdr.Get("content-type"), ""); got != "application/grpc+proto" && got != "application/grpc" {
		t.Errorf("client received content-type %q, want application/grpc+proto", got)
	}
}

// The pass-through codec must not be registered globally: a registered "raw"
// codec would be picked by any grpc-go server in the process for "+raw" calls.
func TestRawCodecNotRegisteredGlobally(t *testing.T) {
	t.Parallel()
	if encoding.GetCodec("raw") != nil || encoding.GetCodecV2("raw") != nil {
		t.Errorf("a codec named %q is registered globally", "raw")
	}
}

func firstOr(vals []string, def string) string {
	if len(vals) == 0 {
		return def
	}
	return vals[0]
}
