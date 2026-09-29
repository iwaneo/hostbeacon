// Package release reads Hostbeacon releases: the minisign signature of
// SHA256SUMS, the hashes in it, release versions, and the file names of each
// release (v1 spec §10). It has no network code; the caller downloads.
package release

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// PublicKeys are the minisign keys that may sign a release: the release key
// and the backup key. Release builds keep them; a test build may set others
// with -ldflags "-X github.com/iwaneo/hostbeacon/agent/internal/release.PublicKeys=...".
var PublicKeys = "RWQdtGLWn0hUvBq2UHOd68XCxhTpoXJWHiBdDrfEPGnd3lPk/v6SxsVD RWR9UElf4XqFiZVup9OaGQM40n2/eCDXq/mnh4UBvKcgdcULHudnvmuv"

// BaseURL holds the releases. Home Assistant never sends a URL. A test build
// may set another with -ldflags -X, like PublicKeys.
var BaseURL = "https://github.com/iwaneo/hostbeacon/releases"

// The signed list of every file of a release, and its signature.
const (
	SumsFile      = "SHA256SUMS"
	SignatureFile = "SHA256SUMS.minisig"
)

// LatestURL is a file of the newest release.
func LatestURL(name string) string { return BaseURL + "/latest/download/" + name }

// VersionURL is a file of the release with this version.
func VersionURL(version, name string) string {
	return BaseURL + "/download/v" + version + "/" + name
}

// Kind is how the Agent is installed on a Host.
type Kind string

const (
	Deb     Kind = "deb"
	RPM     Kind = "rpm"
	Tarball Kind = "tarball"
)

// FileName is the file of a release for one kind and architecture (amd64 or
// arm64), as packaging/build.sh names it.
func FileName(kind Kind, version, arch string) string {
	switch kind {
	case Deb:
		return fmt.Sprintf("hostbeacon_%s_%s.deb", version, arch)
	case RPM:
		rpmArch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
		return fmt.Sprintf("hostbeacon-%s-1.%s.rpm", version, rpmArch)
	}
	return fmt.Sprintf("hostbeacon_%s_linux_%s.tar.gz", version, arch)
}

// PublicKey is one minisign public key.
type PublicKey struct {
	id  [8]byte
	key ed25519.PublicKey
}

// ParseKeys reads minisign public keys: base64 keys separated by spaces or
// lines. "untrusted comment:" lines are skipped.
func ParseKeys(text string) ([]PublicKey, error) {
	var keys []PublicKey
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		for _, field := range strings.Fields(line) {
			raw, err := base64.StdEncoding.DecodeString(field)
			if err != nil || len(raw) != 2+8+ed25519.PublicKeySize || string(raw[:2]) != "Ed" {
				return nil, errors.New("not a minisign public key: " + field)
			}
			var key PublicKey
			copy(key.id[:], raw[2:10])
			key.key = ed25519.PublicKey(raw[10:])
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no minisign public key")
	}
	return keys, nil
}

// Verify checks a minisign signature of data made by one of keys, and returns
// its trusted comment, which the signature covers too. Only the default
// algorithm of minisign 0.8 and later (a signed BLAKE2b hash) is accepted.
func Verify(data, signature []byte, keys []PublicKey) (trustedComment string, err error) {
	lines := strings.Split(strings.ReplaceAll(string(signature), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") || !strings.HasPrefix(lines[2], "trusted comment: ") {
		return "", errors.New("the signature file is not a minisign signature")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != 2+8+ed25519.SignatureSize {
		return "", errors.New("the signature file is not a minisign signature")
	}
	if string(raw[:2]) != "ED" {
		return "", errors.New("the signature does not use minisign's hashed algorithm")
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return "", errors.New("the signature file is not a minisign signature")
	}
	comment := strings.TrimPrefix(lines[2], "trusted comment: ")
	signed := raw[10:]
	for _, key := range keys {
		if !bytes.Equal(key.id[:], raw[2:10]) {
			continue
		}
		hash := blake2b.Sum512(data)
		if !ed25519.Verify(key.key, hash[:], signed) {
			return "", errors.New("the signature does not match the file")
		}
		if !ed25519.Verify(key.key, append(append([]byte{}, signed...), comment...), global) {
			return "", errors.New("the signature does not match its trusted comment")
		}
		return comment, nil
	}
	return "", errors.New("the file is not signed with a Hostbeacon release key")
}

// Sums is a verified SHA256SUMS: the release version and each file's hash.
type Sums struct {
	Version string
	files   map[string][sha256.Size]byte
}

// ErrBadHash: a downloaded file differs from the signed SHA256SUMS.
var ErrBadHash = errors.New("the file's hash differs from the signed SHA256SUMS")

// commentPrefix starts the trusted comment the release workflow signs:
// "hostbeacon <version>". The version is signed with the file list.
const commentPrefix = "hostbeacon "

// ReadSums checks the signature of SHA256SUMS and reads it. The release
// version comes from the signed trusted comment and must be X.Y.Z.
func ReadSums(data, signature []byte, keys []PublicKey) (Sums, error) {
	comment, err := Verify(data, signature, keys)
	if err != nil {
		return Sums{}, err
	}
	version, found := strings.CutPrefix(comment, commentPrefix)
	if !found || !releasePattern.MatchString(version) {
		return Sums{}, fmt.Errorf("the signed comment %q does not name a release version", comment)
	}
	sums := Sums{Version: version, files: map[string][sha256.Size]byte{}}
	for line := range strings.Lines(string(data)) {
		// sha256sum writes "<hash>  <name>", or "<hash> *<name>" in binary mode.
		hash, name, found := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		raw, err := hex.DecodeString(hash)
		if !found || err != nil || len(raw) != sha256.Size {
			return Sums{}, fmt.Errorf("%s has a line that is not a hash and a name", SumsFile)
		}
		sums.files[strings.TrimLeft(name, " *")] = [sha256.Size]byte(raw)
	}
	return sums, nil
}

// Has says whether the release has a file with this name.
func (s Sums) Has(name string) bool {
	_, found := s.files[name]
	return found
}

// Check compares data with the hash of name in SHA256SUMS.
func (s Sums) Check(name string, data []byte) error {
	want, found := s.files[name]
	if !found {
		return fmt.Errorf("%s is not in the release's %s", name, SumsFile)
	}
	if sha256.Sum256(data) != want {
		return fmt.Errorf("%s: %w", name, ErrBadHash)
	}
	return nil
}

var (
	// releasePattern is a release version: X.Y.Z, no leading zeros.
	releasePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	// versionPattern also takes a pre-release or build, like 0.0.0-dev.
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?$`)
)

// IsRelease says whether version is a release version, X.Y.Z.
func IsRelease(version string) bool { return releasePattern.MatchString(version) }

// Compare compares two versions as semantic versions: -1 when a is older
// than b, 0 when they are the same, and 1 when a is newer. A version with a
// pre-release part (1.0.0-rc.1) is older than the same version without one.
func Compare(a, b string) (int, error) {
	pa, pb := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return 0, fmt.Errorf("cannot compare the versions %q and %q", a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.ParseUint(pa[i], 10, 64)
		y, _ := strconv.ParseUint(pb[i], 10, 64)
		if x != y {
			if x < y {
				return -1, nil
			}
			return 1, nil
		}
	}
	switch {
	case pa[4] == pb[4]:
		return 0, nil
	case pa[4] == "":
		return 1, nil
	case pb[4] == "":
		return -1, nil
	}
	return comparePreRelease(pa[4], pb[4]), nil
}

// comparePreRelease compares dot-separated parts: numbers by value and
// below text, text in ASCII order, and fewer parts first.
func comparePreRelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		x, xErr := strconv.ParseUint(as[i], 10, 64)
		y, yErr := strconv.ParseUint(bs[i], 10, 64)
		switch {
		case xErr == nil && yErr == nil && x != y:
			if x < y {
				return -1
			}
			return 1
		case xErr == nil && yErr != nil:
			return -1
		case xErr != nil && yErr == nil:
			return 1
		case as[i] != bs[i]:
			return strings.Compare(as[i], bs[i])
		}
	}
	return cmp.Compare(len(as), len(bs))
}
