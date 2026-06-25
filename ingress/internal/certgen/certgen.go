package certgen

import (
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
)

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
)

func caCertPath(dir string) string  { return filepath.Join(dir, caCertFile) }
func caKeyPath(dir string) string   { return filepath.Join(dir, caKeyFile) }

func loadOrCreateCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(caCertPath(dir))
	if err == nil {
		keyPEM, err := os.ReadFile(caKeyPath(dir))
		if err == nil {
			return parseCA(certPEM, keyPEM)
		}
	}

	cert, key, err := generateCA()
	if err != nil {
		return nil, nil, err
	}

	certPEM, keyPEM, err := marshalCA(cert, key)
	if err != nil {
		return nil, nil, err
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, fmt.Errorf("create cert dir: %w", err)
	}
	if err := os.WriteFile(caCertPath(dir), certPEM, 0644); err != nil {
		return nil, nil, fmt.Errorf("write ca cert: %w", err)
	}
	if err := os.WriteFile(caKeyPath(dir), keyPEM, 0600); err != nil {
		return nil, nil, fmt.Errorf("write ca key: %w", err)
	}

	return cert, key, nil
}

func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ca key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Homelab Ingress CA",
			Organization: []string{"homelab-ingress"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create ca cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}

	return cert, key, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("failed to decode ca cert pem")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}

	block, _ = pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("failed to decode ca key pem")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca key: %w", err)
	}

	return cert, key, nil
}

func marshalCA(cert *x509.Certificate, key *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if certPEM == nil {
		return nil, nil, fmt.Errorf("failed to encode ca cert")
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal ca key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM, nil
}

// LoadOrCreate loads the CA from disk or creates one, then generates a leaf
// certificate signed by the CA and returns a TLS certificate for use by the
// gRPC server.  CA cert PEM is also returned so callers can write it to a
// known location for clients.
func LoadOrCreate(certDir string, serverAddrs []string) (*tls.Certificate, []byte, error) {
	caCert, caKey, err := loadOrCreateCA(certDir)
	if err != nil {
		return nil, nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf serial: %w", err)
	}

	leafTmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Homelab Ingress",
			Organization: []string{"homelab-ingress"},
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	for _, addr := range serverAddrs {
		if ip := net.ParseIP(addr); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		} else {
			leafTmpl.DNSNames = append(leafTmpl.DNSNames, addr)
		}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create leaf cert: %w", err)
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{leafDER, caCert.Raw},
		PrivateKey:  leafKey,
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})

	return tlsCert, caPEM, nil
}

// LoadCACert reads the CA certificate PEM file and returns the parsed cert pool.
func LoadCACert(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ca cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no CA certificate found in %q", path)
	}
	return pool, nil
}
