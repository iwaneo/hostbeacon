package version

import "testing"

func TestStringNamesTheAgent(t *testing.T) {
	Version = "1.2.3"
	if got, want := String(), "hostbeacon 1.2.3"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
