package grpcutil

import (
	"strings"

	"google.golang.org/grpc/codes"
)

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

// ParseCode parses a gRPC status code name as written in config, ignoring
// case, surrounding spaces, '_' and '-': "resource_exhausted",
// "RESOURCE_EXHAUSTED" and "ResourceExhausted" are the same code.
func ParseCode(name string) (codes.Code, bool) {
	want := normalizeCodeName(name)
	if want == "" {
		return 0, false
	}
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		if normalizeCodeName(c.String()) == want {
			return c, true
		}
	}
	return 0, false
}

func normalizeCodeName(name string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(name)))
}

// IsBreakerFailure reports whether code counts against a node's circuit
// breaker. configured is the network's circuit_breaker.grpc_codes: when set it
// replaces the default IsNodeFailure set, as http_codes does for HTTP. Names
// are validated when the config loads, so an unparsable one never matches.
func IsBreakerFailure(configured []string, code codes.Code) bool {
	if code == codes.OK {
		return false
	}
	if len(configured) == 0 {
		return IsNodeFailure(code)
	}
	for _, name := range configured {
		if c, ok := ParseCode(name); ok && c == code {
			return true
		}
	}
	return false
}
