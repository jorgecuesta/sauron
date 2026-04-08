package grpcutil

import (
	"crypto/tls"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// NewConnection creates a gRPC client connection with standard TLS/keepalive/passthrough config.
// If insecureConn is true, no TLS is used. The ka parameter configures keepalive ping behavior.
// Extra dial options (e.g. message size limits, raw codec) can be appended via extraOpts.
func NewConnection(target string, insecureConn bool, ka keepalive.ClientParameters, extraOpts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := make([]grpc.DialOption, 0, 2+len(extraOpts))

	if insecureConn {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	}

	opts = append(opts, grpc.WithKeepaliveParams(ka))

	opts = append(opts, extraOpts...)

	// Use passthrough:/// resolver to avoid DNS resolver IPv6 timeout issues
	resolvedTarget := target
	if !strings.HasPrefix(resolvedTarget, "passthrough://") && !strings.HasPrefix(resolvedTarget, "dns://") {
		resolvedTarget = "passthrough:///" + resolvedTarget
	}

	return grpc.NewClient(resolvedTarget, opts...)
}
