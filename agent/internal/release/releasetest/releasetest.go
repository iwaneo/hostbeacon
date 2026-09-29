// Package releasetest signs fake releases for tests, like the release
// workflow does with minisign.
package releasetest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Key is a minisign key pair.
type Key struct {
	id      [8]byte
	private ed25519.PrivateKey
	// Public is the public key as minisign writes it.
	Public string
}

// NewKey makes a new key pair.
func NewKey() Key {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	key := Key{private: private}
	rand.Read(key.id[:])
	key.Public = base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), key.id[:]...), public...))
	return key
}

// Sign signs data like `minisign -S -t <comment>`: a signed BLAKE2b hash.
func (k Key) Sign(data []byte, comment string) []byte {
	hash := blake2b.Sum512(data)
	return k.SignAs("ED", hash[:], comment)
}

// SignAs signs message with the algorithm alg: "ED" is for a hash, and "Ed"
// (legacy) for the file itself.
func (k Key) SignAs(alg string, message []byte, comment string) []byte {
	signature := ed25519.Sign(k.private, message)
	global := ed25519.Sign(k.private, append(append([]byte{}, signature...), comment...))
	line := base64.StdEncoding.EncodeToString(append(append([]byte(alg), k.id[:]...), signature...))
	return fmt.Appendf(nil, "untrusted comment: signature from minisign secret key\n%s\ntrusted comment: %s\n%s\n",
		line, comment, base64.StdEncoding.EncodeToString(global))
}

// Sums writes a SHA256SUMS for files, which maps names to contents.
func Sums(files map[string][]byte) []byte {
	var out strings.Builder
	for _, name := range slices.Sorted(maps.Keys(files)) {
		hash := sha256.Sum256(files[name])
		fmt.Fprintf(&out, "%s  %s\n", hex.EncodeToString(hash[:]), name)
	}
	return []byte(out.String())
}
