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
	"slices"
	"strings"
	"sync"
	"syscall"
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
	// StaleAfter is how long a Pairing may go unseen before the owner is
	// warned. Old Pairings are never removed by themselves.
	StaleAfter = 90 * 24 * time.Hour

	codeFile     = "pairing-code.json"
	pairingsFile = "pairings.json"
	fileFormat   = 1
)

// ErrRefused is returned for every refused Pairing step. It does not say why,
// so a guesser learns nothing.
var ErrRefused = errors.New("pairing refused")

var (
	// ErrNotFound means no Pairing has that ID or name.
	ErrNotFound = errors.New("no Pairing has this ID or name")
	// ErrAmbiguous means several Pairings have that name.
	ErrAmbiguous = errors.New("several Pairings have this name; use the ID")
)

// ID is the short ID of the Pairing with this key: the first 8 hex characters
// of the key's SHA-256. Home Assistant can work it out from its key, so it can
// tell the owner which Pairing to remove.
func ID(key []byte) string {
	sum := sha256.Sum256(key)
	return idOf(hex.EncodeToString(sum[:]))
}

// idOf is the Pairing ID for the hex SHA-256 of a key.
func idOf(keySHA256 string) string {
	return keySHA256[:min(8, len(keySHA256))]
}

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

// Pairing is one confirmed Pairing as the owner sees it.
type Pairing struct {
	ID       string
	Name     string
	Created  time.Time
	LastSeen time.Time
}

// Stale returns the Pairings in list not seen for StaleAfter or longer.
func Stale(list []Pairing, now time.Time) []Pairing {
	var stale []Pairing
	for _, p := range list {
		if now.Sub(p.LastSeen) >= StaleAfter {
			stale = append(stale, p)
		}
	}
	return stale
}

// pairing is one confirmed Pairing, as stored on the Host.
type pairing struct {
	Name      string    `json:"name"`
	KeySHA256 string    `json:"key_sha256"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen"`
}

func (p pairing) public() Pairing {
	lastSeen := p.LastSeen
	if lastSeen.IsZero() {
		lastSeen = p.Created
	}
	return Pairing{ID: idOf(p.KeySHA256), Name: p.Name, Created: p.Created, LastSeen: lastSeen}
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

// Login checks a key and returns its Pairing. The first Login with a pending
// key confirms it: Home Assistant only uses a key it saved together with the
// certificate pin. An error means the Pairings file could not be read or
// written; the key is refused then.
func (p *Pairings) Login(key []byte) (Pairing, bool, error) {
	if len(key) != Size {
		return Pairing{}, false, nil
	}
	hash := sha256.Sum256(key)
	p.mu.Lock()
	defer p.mu.Unlock()

	var found Pairing
	ok := false
	err := p.update(func(list *pairingList) (bool, error) {
		now := p.now().UTC()
		index := -1
		for i, item := range list.Pairings {
			stored, err := hex.DecodeString(item.KeySHA256)
			if err == nil && subtle.ConstantTimeCompare(stored, hash[:]) == 1 {
				index = i
			}
		}
		if index < 0 {
			kept := p.pending[:0]
			for _, item := range p.pending {
				switch {
				case !now.Before(item.expires):
				case subtle.ConstantTimeCompare(item.hash[:], hash[:]) == 1:
					list.Pairings = append(list.Pairings, pairing{Name: item.name, KeySHA256: hex.EncodeToString(hash[:]), Created: now})
					index = len(list.Pairings) - 1
				default:
					kept = append(kept, item)
				}
			}
			p.pending = kept
			if index < 0 {
				return false, nil
			}
		}
		list.Pairings[index].LastSeen = now
		found, ok = list.Pairings[index].public(), true
		return true, nil
	})
	if err != nil {
		return Pairing{}, false, err
	}
	return found, ok, nil
}

// List returns the confirmed Pairings, oldest first.
func (p *Pairings) List() ([]Pairing, error) {
	var list []Pairing
	err := p.update(func(stored *pairingList) (bool, error) {
		for _, item := range stored.Pairings {
			list = append(list, item.public())
		}
		return false, nil
	})
	return list, err
}

// Find returns the Pairing with this ID, or with this name if only one
// Pairing has it.
func (p *Pairings) Find(idOrName string) (Pairing, error) {
	list, err := p.List()
	if err != nil {
		return Pairing{}, err
	}
	for _, matches := range []func(Pairing) bool{
		func(item Pairing) bool { return item.ID == idOrName },
		func(item Pairing) bool { return item.Name == idOrName },
	} {
		found := slices.DeleteFunc(slices.Clone(list), func(item Pairing) bool { return !matches(item) })
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
		}
		return Pairing{}, ErrAmbiguous
	}
	return Pairing{}, ErrNotFound
}

// Remove deletes the Pairing with this ID. Its key stops working at once.
func (p *Pairings) Remove(id string) (Pairing, error) {
	var removed Pairing
	err := p.update(func(list *pairingList) (bool, error) {
		var matches []Pairing
		kept := slices.DeleteFunc(slices.Clone(list.Pairings), func(item pairing) bool {
			if item.public().ID != id {
				return false
			}
			matches = append(matches, item.public())
			return true
		})
		switch len(matches) {
		case 0:
			return false, ErrNotFound
		case 1:
			removed, list.Pairings = matches[0], kept
			return true, nil
		}
		return false, ErrAmbiguous
	})
	return removed, err
}

// RemoveAll deletes every Pairing, pending ones too, and the Pairing code.
// Their keys stop working at once.
func (p *Pairings) RemoveAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = nil
	return p.update(func(list *pairingList) (bool, error) {
		if err := os.Remove(filepath.Join(p.dir, codeFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		list.Pairings = []pairing{}
		return true, nil
	})
}

// Seen sets the last seen time of the Pairings with these IDs to now. The
// Agent calls it for Pairings that stay connected.
func (p *Pairings) Seen(ids []string) error {
	return p.update(func(list *pairingList) (bool, error) {
		now := p.now().UTC()
		for i, item := range list.Pairings {
			if slices.Contains(ids, item.public().ID) {
				list.Pairings[i].LastSeen = now
			}
		}
		return true, nil
	})
}

// update reads the Pairings file, calls change, and writes the file back if
// change says so. A lock on the state directory keeps the network part and a
// root owner command from overwriting each other's change.
func (p *Pairings) update(change func(*pairingList) (bool, error)) error {
	dir, err := os.Open(p.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("cannot lock %s: %w", p.dir, err)
	}
	defer syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)

	list, err := p.read()
	if err != nil {
		return err
	}
	write, err := change(&list)
	if err != nil || !write {
		return err
	}
	return p.write(list)
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
