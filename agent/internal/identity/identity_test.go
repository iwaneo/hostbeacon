package identity

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestFirstStartMakesIdentityAndLaterStartsKeepIt(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first.InstanceID) {
		t.Errorf("instance ID %q is not a UUID v4", first.InstanceID)
	}
	if first.Fingerprint != sha256.Sum256(first.Certificate.Certificate[0]) {
		t.Error("fingerprint is not the SHA-256 of the certificate")
	}
	info, err := os.Stat(filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, want 0600", info.Mode().Perm())
	}

	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.InstanceID != first.InstanceID || again.Fingerprint != first.Fingerprint {
		t.Error("a later start changed the instance ID or the certificate")
	}
}

func TestNewUUIDIsRandom(t *testing.T) {
	if NewUUID() == NewUUID() {
		t.Error("two UUIDs are the same")
	}
}
