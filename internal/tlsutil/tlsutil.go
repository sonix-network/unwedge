// Package tlsutil builds TLS credentials for the gRPC server and clients,
// supporting optional mutual TLS. The service is intended to be exposed over
// the internet, so TLS (ideally mTLS) is the norm.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// ServerOptions configures server-side TLS.
type ServerOptions struct {
	CertFile string
	KeyFile  string
	// ClientCAFile, if set, requires and verifies client certificates (mTLS).
	ClientCAFile string
}

// ServerCredentials builds gRPC transport credentials for the server.
func ServerCredentials(o ServerOptions) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: load server keypair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if o.ClientCAFile != "" {
		pool, err := loadCAPool(o.ClientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(cfg), nil
}

// ClientOptions configures client-side TLS.
type ClientOptions struct {
	// CAFile is the CA that signed the server cert. Empty uses the system pool.
	CAFile string
	// ServerName overrides the SNI / verified name (defaults to the dial host).
	ServerName string
	// CertFile / KeyFile provide a client certificate for mTLS.
	CertFile string
	KeyFile  string
	// InsecureSkipVerify disables server verification (development only).
	InsecureSkipVerify bool
}

// ClientCredentials builds gRPC transport credentials for a client.
func ClientCredentials(o ClientOptions) (credentials.TransportCredentials, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         o.ServerName,
		InsecureSkipVerify: o.InsecureSkipVerify, //nolint:gosec // opt-in dev flag
	}
	if o.CAFile != "" {
		pool, err := loadCAPool(o.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if o.CertFile != "" || o.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(cfg), nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: read CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlsutil: no certificates found in %s", path)
	}
	return pool, nil
}
