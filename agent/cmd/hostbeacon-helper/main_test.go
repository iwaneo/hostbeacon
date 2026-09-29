package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The root helper is built with no network code (v1 spec §4.1). Its only
// socket is the local one.
func TestHelperHasNoNetworkCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"crypto/tls", "net/http", "github.com/coder/websocket",
		"github.com/iwaneo/hostbeacon/agent/internal/server",
		"github.com/iwaneo/hostbeacon/agent/internal/pairing",
	}
	for dep := range strings.Lines(string(out)) {
		for _, name := range forbidden {
			if strings.TrimSpace(dep) == name {
				t.Errorf("the helper links %s", name)
			}
		}
	}
}
