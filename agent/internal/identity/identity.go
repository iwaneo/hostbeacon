// Package identity keeps the Agent instance ID and the Agent's TLS
// certificate in the state directory. Both are made once, on the first start,
// and never renewed.
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
)

const (
	identityFile = "identity.json"
	keyFile      = "tls.key"
	certFile     = "tls.crt"
	fileFormat   = 1
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Identity is who this Agent is.
type Identity struct {
	InstanceID  string
	Certificate tls.Certificate
	// Fingerprint is the SHA-256 of the certificate. Home Assistant pins it.
	Fingerprint [32]byte
}

type identityData struct {
	Format     int    `json:"format"`
	InstanceID string `json:"instance_id"`
}

// Load reads the identity in the state directory dir, and makes whatever is
// missing.
func Load(dir string) (Identity, error) {
	instanceID, err := loadInstanceID(dir)
	if err != nil {
		return Identity{}, err
	}
	certificate, err := loadCertificate(dir)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		InstanceID:  instanceID,
		Certificate: certificate,
		Fingerprint: sha256.Sum256(certificate.Certificate[0]),
	}, nil
}

func loadInstanceID(dir string) (string, error) {
	path := filepath.Join(dir, identityFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id := identityData{Format: fileFormat, InstanceID: NewUUID()}
		data, _ := json.MarshalIndent(id, "", "  ")
		return id.InstanceID, statefile.Write(path, append(data, '\n'), 0o644)
	}
	if err != nil {
		return "", err
	}
	var id identityData
	if err := json.Unmarshal(data, &id); err != nil || id.Format != fileFormat || !uuidPattern.MatchString(id.InstanceID) {
		return "", fmt.Errorf("cannot read %s", path)
	}
	return id.InstanceID, nil
}

func loadCertificate(dir string) (tls.Certificate, error) {
	keyPath, certPath := filepath.Join(dir, keyFile), filepath.Join(dir, certFile)
	_, keyErr := os.Stat(keyPath)
	_, certErr := os.Stat(certPath)
	if errors.Is(keyErr, os.ErrNotExist) && errors.Is(certErr, os.ErrNotExist) {
		if err := makeCertificate(keyPath, certPath); err != nil {
			return tls.Certificate{}, err
		}
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// makeCertificate makes a key pair and a self-signed certificate that does
// not expire in practice: Home Assistant pins it, so it is never renewed.
func makeCertificate(keyPath, certPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Hostbeacon Agent"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key first: a certificate without its key is useless.
	if err := statefile.Write(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return statefile.Write(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// NewUUID returns a random lowercase UUID v4.
func NewUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
