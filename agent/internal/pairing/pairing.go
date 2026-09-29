// Package pairing makes and checks Pairing codes and Pairing keys.
//
// The Pairing step (see protocol/README.md) runs inside TLS before Home
// Assistant trusts the certificate. Both sides prove that they know the code
// with an HMAC over the certificate fingerprint, so a machine in the middle,
// which has another certificate, learns nothing it can use. The code never
// crosses the network, and it is stretched with PBKDF2, so one captured proof
// cannot be cracked offline in the 10 minutes the code lives.
//
// The Host keeps only a SHA-256 hash of each key.
package pairing

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/statefile"
)

const (
	// Alphabet has no look-alikes: no 0/O and no 1/I/L.
	Alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	// CodeLifetime is how long a Pairing code works.
	CodeLifetime = 10 * time.Minute
	// MaxTries is how many wrong codes cancel the code.
	MaxTries = 5
	// Iterations is the PBKDF2 cost for the code.
	Iterations = 100_000
	// Size is the size in bytes of a key, nonce, fingerprint, and proof.
	Size = 32

	codeFile     = "pairing-code.json"
	pairingsFile = "pairings.json"
	fileFormat   = 1
)

// ErrRefused is returned for every refused Pairing step. It does not say why,
// so a guesser learns nothing.
var ErrRefused = errors.New("pairing refused")

// CodeKey stretches a Pairing code. nonce is the one Home Assistant sent.
func CodeKey(code string, nonce []byte) []byte {
	salt := append([]byte("hostbeacon pairing v1\x00"), nonce...)
	key, err := pbkdf2.Key(sha256.New, normalize(code), salt, Iterations, Size)
	if err != nil {
		panic(err) // only for sizes that are fixed here
	}
	return key
}

// HomeAssistantProof is what Home Assistant sends to prove it knows the code.
func HomeAssistantProof(codeKey, fingerprint, nonce []byte) []byte {
	return mac(codeKey, "hostbeacon pair home assistant", fingerprint, nonce)
}

// AgentProof is what the Agent answers to prove it knows the code. It also
// covers the new key.
func AgentProof(codeKey, fingerprint, nonce, key []byte) []byte {
	return mac(codeKey, "hostbeacon pair agent", fingerprint, nonce, key)
}

func mac(key []byte, label string, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(label + "\x00"))
	for _, part := range parts {
		h.Write(part)
	}
	return h.Sum(nil)
}

// normalize reads a code the way a person may type it: any case, with or
// without dashes and spaces.
func normalize(code string) string {
	code = strings.ToUpper(code)
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

type activeCode struct {
	Format    int       `json:"format"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	Tries     int       `json:"tries"`
}

// NewCode makes a Pairing code in the state directory dir and returns it as
// 3 groups of 4 characters. It replaces any earlier code.
func NewCode(dir string, now time.Time) (code string, expires time.Time, err error) {
	chars := make([]byte, 12)
	for i := range chars {
		chars[i] = Alphabet[randomIndex(len(Alphabet))]
	}
	expires = now.Add(CodeLifetime).UTC()
	data, err := json.Marshal(activeCode{Format: fileFormat, Code: string(chars), ExpiresAt: expires})
	if err != nil {
		return "", time.Time{}, err
	}
	if err := statefile.Write(filepath.Join(dir, codeFile), data, 0o600); err != nil {
		return "", time.Time{}, err
	}
	return fmt.Sprintf("%s-%s-%s", chars[:4], chars[4:8], chars[8:]), expires, nil
}

// randomIndex returns a uniform random number below n.
func randomIndex(n int) int {
	limit := 256 - 256%n
	b := make([]byte, 1)
	for {
		rand.Read(b)
		if int(b[0]) < limit {
			return int(b[0]) % n
		}
	}
}

// pairing is one confirmed Pairing, as stored on the Host.
type pairing struct {
	Name      string    `json:"name"`
	KeySHA256 string    `json:"key_sha256"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen"`
}

type pairingList struct {
	Format   int       `json:"format"`
	Pairings []pairing `json:"pairings"`
}

// pending is a key made by a Pairing step that Home Assistant has not used yet.
type pending struct {
	name    string
	hash    [32]byte
	expires time.Time
}

// Pairings holds this Host's Pairings: confirmed ones on disk, pending ones in
// memory.
type Pairings struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	pending []pending
}

// Open returns the Pairings kept in the state directory dir.
func Open(dir string, now func() time.Time) *Pairings {
	return &Pairings{dir: dir, now: now}
}

// Pair checks one Pairing step. name is Home Assistant's name for the
// Pairing, fingerprint the SHA-256 of this Agent's certificate, and nonce and
// proof what Home Assistant sent. It returns a new key and the Agent's proof.
// The key stays pending until its first Login, and is dropped when the code
// expires.
func (p *Pairings) Pair(name string, fingerprint, nonce, proof []byte) (key, agentProof []byte, err error) {
	if len(fingerprint) != Size || len(nonce) != Size || len(proof) != Size {
		return nil, nil, ErrRefused
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	path := filepath.Join(p.dir, codeFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrRefused
	}
	if err != nil {
		return nil, nil, err
	}
	var code activeCode
	if err := json.Unmarshal(data, &code); err != nil || code.Format != fileFormat {
		return nil, nil, errors.Join(ErrRefused, errors.New("cannot read "+codeFile))
	}
	if !p.now().Before(code.ExpiresAt) || code.Tries >= MaxTries {
		return nil, nil, errors.Join(ErrRefused, os.Remove(path))
	}

	codeKey := CodeKey(code.Code, nonce)
	if !hmac.Equal(proof, HomeAssistantProof(codeKey, fingerprint, nonce)) {
		code.Tries++
		if code.Tries >= MaxTries {
			return nil, nil, errors.Join(ErrRefused, os.Remove(path))
		}
		data, _ := json.Marshal(code)
		return nil, nil, errors.Join(ErrRefused, statefile.Write(path, data, 0o600))
	}

	// The code works once. If it cannot be removed, refuse.
	if err := os.Remove(path); err != nil {
		return nil, nil, errors.Join(ErrRefused, err)
	}
	key = make([]byte, Size)
	rand.Read(key)
	p.pending = append(p.pending, pending{name: name, hash: sha256.Sum256(key), expires: code.ExpiresAt})
	return key, AgentProof(codeKey, fingerprint, nonce, key), nil
}

// Login checks a key and returns the name of its Pairing. The first Login
// with a pending key confirms it: Home Assistant only uses a key it saved
// together with the certificate pin. An error means the Pairings file could
// not be read or written; the key is refused then.
func (p *Pairings) Login(key []byte) (string, bool, error) {
	if len(key) != Size {
		return "", false, nil
	}
	hash := sha256.Sum256(key)
	p.mu.Lock()
	defer p.mu.Unlock()

	list, err := p.read()
	if err != nil {
		return "", false, err
	}
	now := p.now().UTC()
	found := -1
	for i, item := range list.Pairings {
		stored, err := hex.DecodeString(item.KeySHA256)
		if err == nil && subtle.ConstantTimeCompare(stored, hash[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		kept := p.pending[:0]
		for _, item := range p.pending {
			switch {
			case !now.Before(item.expires):
			case subtle.ConstantTimeCompare(item.hash[:], hash[:]) == 1:
				list.Pairings = append(list.Pairings, pairing{Name: item.name, KeySHA256: hex.EncodeToString(hash[:]), Created: now})
				found = len(list.Pairings) - 1
			default:
				kept = append(kept, item)
			}
		}
		p.pending = kept
		if found < 0 {
			return "", false, nil
		}
	}
	list.Pairings[found].LastSeen = now
	if err := p.write(list); err != nil {
		return "", false, err
	}
	return list.Pairings[found].Name, true, nil
}

func (p *Pairings) read() (pairingList, error) {
	data, err := os.ReadFile(filepath.Join(p.dir, pairingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return pairingList{Format: fileFormat, Pairings: []pairing{}}, nil
	}
	if err != nil {
		return pairingList{}, err
	}
	var list pairingList
	if err := json.Unmarshal(data, &list); err != nil {
		return pairingList{}, fmt.Errorf("cannot read %s: %w", pairingsFile, err)
	}
	if list.Format != fileFormat {
		return pairingList{}, fmt.Errorf("%s has unknown format %d", pairingsFile, list.Format)
	}
	return list, nil
}

func (p *Pairings) write(list pairingList) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return statefile.Write(filepath.Join(p.dir, pairingsFile), append(data, '\n'), 0o600)
}
