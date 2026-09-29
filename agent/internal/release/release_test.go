package release

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/release/releasetest"
)

func sums(files map[string]string) []byte {
	contents := map[string][]byte{}
	for name, content := range files {
		contents[name] = []byte(content)
	}
	return releasetest.Sums(contents)
}

func TestVerifyAcceptsASignatureFromTheMinisignTool(t *testing.T) {
	read := func(name string) []byte {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	keys, err := ParseKeys(string(read("minisign.pub")))
	if err != nil {
		t.Fatal(err)
	}
	comment, err := Verify(read("SHA256SUMS"), read("SHA256SUMS.minisig"), keys)
	if err != nil || comment != "hostbeacon 1.2.3" {
		t.Fatalf("Verify = %q, %v", comment, err)
	}
}

func TestReadSumsChecksTheSignatureWithEitherKey(t *testing.T) {
	releaseKey, backupKey, otherKey := releasetest.NewKey(), releasetest.NewKey(), releasetest.NewKey()
	keys, err := ParseKeys(releaseKey.Public + "\n" + backupKey.Public)
	if err != nil {
		t.Fatal(err)
	}
	data := sums(map[string]string{"hostbeacon_1.2.3_amd64.deb": "package"})
	for _, key := range []releasetest.Key{releaseKey, backupKey} {
		got, err := ReadSums(data, key.Sign(data, "hostbeacon 1.2.3"), keys)
		if err != nil || got.Version != "1.2.3" {
			t.Fatalf("ReadSums = %+v, %v", got, err)
		}
		if err := got.Check("hostbeacon_1.2.3_amd64.deb", []byte("package")); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}

	refused := map[string][]byte{
		"another key":       otherKey.Sign(data, "hostbeacon 1.2.3"),
		"no signature":      nil,
		"changed file":      releaseKey.Sign(append([]byte("0000  evil\n"), data...), "hostbeacon 1.2.3"),
		"changed comment":   []byte(strings.Replace(string(releaseKey.Sign(data, "hostbeacon 1.2.3")), "hostbeacon 1.2.3", "hostbeacon 9.9.9", 1)),
		"no version":        releaseKey.Sign(data, "timestamp:1 file:SHA256SUMS"),
		"not a release":     releaseKey.Sign(data, "hostbeacon 1.2"),
		"a pre-release":     releaseKey.Sign(data, "hostbeacon 1.2.3-rc.1"),
		"legacy signature":  releaseKey.SignAs("Ed", data, "hostbeacon 1.2.3"),
		"garbled signature": []byte("untrusted comment: x\n!!!\ntrusted comment: hostbeacon 1.2.3\n!!!\n"),
		"missing lines":     []byte("untrusted comment: x\n"),
	}
	for name, signature := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadSums(data, signature, keys); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestSumsCheckRefusesABadHash(t *testing.T) {
	key := releasetest.NewKey()
	keys, _ := ParseKeys(key.Public)
	data := sums(map[string]string{"hostbeacon_1.2.3_amd64.deb": "package"})
	got, err := ReadSums(data, key.Sign(data, "hostbeacon 1.2.3"), keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Check("hostbeacon_1.2.3_amd64.deb", []byte("changed")); !errors.Is(err, ErrBadHash) {
		t.Fatalf("changed file: %v", err)
	}
	if err := got.Check("hostbeacon_1.2.3_arm64.deb", []byte("package")); err == nil {
		t.Fatal("accepted a file that is not in SHA256SUMS")
	}
}

func TestParseKeysRefusesABadKey(t *testing.T) {
	for _, text := range []string{"", "RWQ", "untrusted comment: only a comment", strings.Repeat("A", 56)} {
		if _, err := ParseKeys(text); err == nil {
			t.Errorf("ParseKeys(%q) accepted", text)
		}
	}
}

func TestTheCompiledInKeysParse(t *testing.T) {
	keys, err := ParseKeys(PublicKeys)
	if err != nil || len(keys) != 2 {
		t.Fatalf("ParseKeys(PublicKeys) = %d keys, %v", len(keys), err)
	}
}

func TestCompare(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "10.0.0", -1},
		{"0.0.0-dev", "0.0.1", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
	} {
		got, err := Compare(test.a, test.b)
		if err != nil || got != test.want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d", test.a, test.b, got, err, test.want)
		}
	}
	for _, bad := range []string{"", "1.2", "v1.2.3", "1.2.3.4", "01.2.3", "1.2.x"} {
		if _, err := Compare(bad, "1.2.3"); err == nil {
			t.Errorf("Compare(%q) accepted", bad)
		}
	}
}

func TestFileNames(t *testing.T) {
	for _, test := range []struct {
		kind Kind
		arch string
		want string
	}{
		{Deb, "amd64", "hostbeacon_1.2.3_amd64.deb"},
		{Deb, "arm64", "hostbeacon_1.2.3_arm64.deb"},
		{RPM, "amd64", "hostbeacon-1.2.3-1.x86_64.rpm"},
		{RPM, "arm64", "hostbeacon-1.2.3-1.aarch64.rpm"},
		{Tarball, "amd64", "hostbeacon_1.2.3_linux_amd64.tar.gz"},
	} {
		if got := FileName(test.kind, "1.2.3", test.arch); got != test.want {
			t.Errorf("FileName(%s, %s) = %q, want %q", test.kind, test.arch, got, test.want)
		}
	}
}
