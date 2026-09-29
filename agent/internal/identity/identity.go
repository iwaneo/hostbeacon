// Package identity keeps the Agent instance ID and the Agent's TLS
// certificate in the state directory. Both are made once, on the first start,
// and never renewed. At every start, the clone check compares hashes of the
// machine ID and the SMBIOS UUID with those stored at install, so a copy of
// the Host gets its own identity (v1 spec §4.4).
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/pairing"
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
	InstanceID string
	// CopiedFrom holds every instance ID and Host ID this Agent had before
	// it found out that it is a copy (v1 spec §4.4).
	CopiedFrom  []string
	Certificate tls.Certificate
	// Fingerprint is the SHA-256 of the certificate. Home Assistant pins it.
	Fingerprint [32]byte
}

// The signals that tell one machine from its copy.
const (
	SignalMachineID  = "machine_id"
	SignalSMBIOSUUID = "smbios_uuid"
)

// Signals is what the Agent read from the Host at this start: the value of
// each signal it could read, and the names of those it could not read. A
// signal the Host does not have is in neither.
type Signals struct {
	Values     map[string]string
	Unreadable []string
}

// hashes are the app-specific hashes of the readable signals, so the state
// directory never holds the raw values.
func (s Signals) hashes() map[string]string {
	hashes := map[string]string{}
	for name, value := range s.Values {
		sum := sha256.Sum256([]byte("hostbeacon identity v1\x00" + name + "\x00" + value))
		hashes[name] = hex.EncodeToString(sum[:])
	}
	return hashes
}

// Outcome is the result of the clone check.
type Outcome int

const (
	// Same: this is the machine the identity was made on.
	Same Outcome = iota
	// Copied: a signal changed, so this is a copy. It has a new identity
	// and no Pairings now.
	Copied
	// Hold: a signal read at install cannot be read now. The Agent keeps its
	// credentials but accepts no connections.
	Hold
)

func (o Outcome) String() string {
	return [...]string{"same", "copied", "hold"}[o]
}

// Check is the result of the clone check at start.
type Check struct {
	Outcome Outcome
	// Missing lists the signals that caused an identity hold.
	Missing []string
}

type identityData struct {
	Format     int    `json:"format"`
	InstanceID string `json:"instance_id"`
	// KnownIDs are every instance ID this Agent had and every Host ID it
	// was paired as, since it last became a copy.
	KnownIDs   []string `json:"known_ids,omitempty"`
	CopiedFrom []string `json:"copied_from,omitempty"`
	// Signals holds the hash of each signal read at install. Nil in files
	// written before the clone check: the next start stores it.
	Signals map[string]string `json:"signals,omitempty"`
	// Hold lists the signals the last start could not read.
	Hold []string `json:"hold,omitempty"`
}

// Status is what the owner sees about the identity.
type Status struct {
	InstanceID string
	CopiedFrom []string
	// Hold lists the signals the last start could not read. Empty when
	// there is no identity hold.
	Hold []string
}

// Start runs the clone check (v1 spec §4.4) with the signals read at this
// start, and returns the identity. On the first start it makes the identity
// and stores the signals. The caller must accept no connection on Hold.
func Start(dir string, signals Signals) (Identity, Check, error) {
	data, found, err := read(dir)
	if err != nil {
		return Identity{}, Check{}, err
	}
	if !found {
		data = identityData{Format: fileFormat, InstanceID: NewUUID()}
		data.KnownIDs = []string{data.InstanceID}
		data.Signals = signals.hashes()
		if err := write(dir, data); err != nil {
			return Identity{}, Check{}, err
		}
		id, err := load(dir, data)
		return id, Check{Outcome: Same}, err
	}
	if data.Signals == nil {
		// Written before the clone check: the signals of now are the ones
		// at install.
		data.Signals = signals.hashes()
		data.KnownIDs = appendNew(data.KnownIDs, data.InstanceID)
		if err := write(dir, data); err != nil {
			return Identity{}, Check{}, err
		}
	}

	current := signals.hashes()
	var missing []string
	changed := false
	for _, name := range slices.Sorted(maps.Keys(data.Signals)) {
		hash, ok := current[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case hash != data.Signals[name]:
			changed = true
		}
	}
	switch {
	case changed:
		id, err := Reset(dir, signals)
		return id, Check{Outcome: Copied}, err
	case len(missing) > 0:
		if !slices.Equal(data.Hold, missing) {
			data.Hold = missing
			if err := write(dir, data); err != nil {
				return Identity{}, Check{}, err
			}
		}
		return Identity{InstanceID: data.InstanceID, CopiedFrom: data.CopiedFrom}, Check{Outcome: Hold, Missing: missing}, nil
	}
	if data.Hold != nil {
		data.Hold = nil
		if err := write(dir, data); err != nil {
			return Identity{}, Check{}, err
		}
	}
	id, err := load(dir, data)
	return id, Check{Outcome: Same}, err
}

// Reset gives the Agent a new identity, as for a copy: it deletes the key,
// the certificate, and every Pairing, makes a new instance ID, key, and
// certificate, and moves every known ID to the copied-from list. The signals
// of now become the ones checked at start.
func Reset(dir string, signals Signals) (Identity, error) {
	data, found, err := read(dir)
	if err != nil {
		return Identity{}, err
	}
	if !found {
		data = identityData{Format: fileFormat}
	}
	// The credentials go first. If this stops half way, the next start
	// still sees the old signals and does it again.
	if err := pairing.Open(dir, time.Now).RemoveAll(); err != nil {
		return Identity{}, err
	}
	if err := removeCertificate(dir); err != nil {
		return Identity{}, err
	}
	data.CopiedFrom = appendNew(data.CopiedFrom, append(data.KnownIDs, data.InstanceID)...)
	data.CopiedFrom = slices.DeleteFunc(data.CopiedFrom, func(id string) bool { return id == "" })
	data.InstanceID = NewUUID()
	data.KnownIDs = []string{data.InstanceID}
	data.Signals = signals.hashes()
	data.Hold = nil
	if err := write(dir, data); err != nil {
		return Identity{}, err
	}
	return load(dir, data)
}

// Keep stores the signals of now as the ones checked at start, and ends an
// identity hold. Every signal checked so far must be readable, unless
// dropMissing: then those that are not are no longer checked, and returned.
func Keep(dir string, signals Signals, dropMissing bool) (dropped []string, err error) {
	data, found, err := read(dir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the Agent has no identity yet; start it once first")
	}
	current := signals.hashes()
	missing := slices.Clone(signals.Unreadable)
	for name := range data.Signals {
		if _, ok := current[name]; !ok {
			missing = appendNew(missing, name)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 && !dropMissing {
		return nil, &MissingError{Names: missing}
	}
	data.Signals = current
	data.Hold = nil
	return missing, write(dir, data)
}

// MissingError is Keep's error when a signal cannot be read.
type MissingError struct {
	Names []string
}

func (e *MissingError) Error() string {
	return "cannot read " + strings.Join(e.Names, ", ")
}

// RegenerateKey deletes every Pairing, then makes a new key and certificate.
// The instance ID stays.
func RegenerateKey(dir string) (Identity, error) {
	data, found, err := read(dir)
	if err != nil {
		return Identity{}, err
	}
	if !found {
		return Identity{}, errors.New("the Agent has no identity yet; start it once first")
	}
	if err := pairing.Open(dir, time.Now).RemoveAll(); err != nil {
		return Identity{}, err
	}
	if err := removeCertificate(dir); err != nil {
		return Identity{}, err
	}
	return load(dir, data)
}

// AddKnownID adds the Host ID Home Assistant knows this Agent as, so a copy
// can say what it was copied from.
func AddKnownID(dir, id string) error {
	if !uuidPattern.MatchString(id) {
		return fmt.Errorf("%q is not a Host ID", id)
	}
	data, found, err := read(dir)
	if err != nil || !found || slices.Contains(data.KnownIDs, id) {
		return err
	}
	data.KnownIDs = append(data.KnownIDs, id)
	return write(dir, data)
}

// ReadStatus returns the identity as the owner sees it.
func ReadStatus(dir string) (Status, error) {
	data, found, err := read(dir)
	if err != nil || !found {
		return Status{}, err
	}
	return Status{InstanceID: data.InstanceID, CopiedFrom: data.CopiedFrom, Hold: data.Hold}, nil
}

// appendNew appends each id not in list yet.
func appendNew(list []string, ids ...string) []string {
	for _, id := range ids {
		if !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	return list
}

func read(dir string) (identityData, bool, error) {
	path := filepath.Join(dir, identityFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return identityData{}, false, nil
	}
	if err != nil {
		return identityData{}, false, err
	}
	var data identityData
	if err := json.Unmarshal(raw, &data); err != nil || data.Format != fileFormat || !uuidPattern.MatchString(data.InstanceID) {
		return identityData{}, false, fmt.Errorf("cannot read %s", path)
	}
	return data, true, nil
}

func write(dir string, data identityData) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return statefile.Write(filepath.Join(dir, identityFile), append(raw, '\n'), 0o644)
}

func load(dir string, data identityData) (Identity, error) {
	certificate, err := loadCertificate(dir)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		InstanceID:  data.InstanceID,
		CopiedFrom:  data.CopiedFrom,
		Certificate: certificate,
		Fingerprint: sha256.Sum256(certificate.Certificate[0]),
	}, nil
}

func removeCertificate(dir string) error {
	for _, name := range []string{keyFile, certFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
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
