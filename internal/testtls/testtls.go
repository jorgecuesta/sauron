// Package testtls gives tests a TLS server certificate that Go's default
// verification trusts, without a hook in production code: Install writes a
// self-signed certificate for 127.0.0.1 and points SSL_CERT_FILE at it. It must
// run in TestMain, before anything in the process loads the system roots
// (they are loaded once).
package testtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	tmservice "cosmossdk.io/api/cosmos/base/tendermint/v1beta1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Install writes the certificate under dir, sets SSL_CERT_FILE to it, and
// returns a server config that presents it.
func Install(dir string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sauron-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	path := filepath.Join(dir, "sauron-test-ca.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.Setenv("SSL_CERT_FILE", path); err != nil {
		return nil, fmt.Errorf("set SSL_CERT_FILE: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// blockServer answers GetLatestBlock with a block at a fixed height.
type blockServer struct {
	tmservice.UnimplementedServiceServer
	height int64
}

func (s blockServer) GetLatestBlock(context.Context, *tmservice.GetLatestBlockRequest) (*tmservice.GetLatestBlockResponse, error) {
	return &tmservice.GetLatestBlockResponse{
		SdkBlock: &tmservice.Block{Header: &tmservice.Header{ChainId: "sauron-test", Height: s.height}},
	}, nil
}

// StartBlockServer serves the Cosmos tendermint Service on 127.0.0.1, over TLS
// with serverTLS or in plaintext when it is nil, answering GetLatestBlock at
// height. stop shuts it down.
func StartBlockServer(serverTLS *tls.Config, height int64) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	var opts []grpc.ServerOption
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	srv := grpc.NewServer(opts...)
	tmservice.RegisterServiceServer(srv, blockServer{height: height})
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), srv.Stop, nil
}
