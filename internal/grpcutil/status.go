package grpcutil

import "google.golang.org/grpc/codes"

// IsNodeFailure reports whether a gRPC status code from a node means the node
// itself is failing (the 5xx equivalents), as opposed to an answer about the
// request such as NotFound or InvalidArgument. The proxy's circuit breaker and
// the gRPC health check share it, so they agree on what a broken node is.
func IsNodeFailure(code codes.Code) bool {
	switch code {
	case codes.Internal, codes.Unavailable, codes.DataLoss, codes.Unknown:
		return true
	}
	return false
}
