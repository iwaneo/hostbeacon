package main

import (
	"slices"
	"testing"
)

func TestForbiddenGroups(t *testing.T) {
	if got := forbiddenGroups([]string{"hostbeacon", "docker", "systemd-journal", "adm", "disk"}); !slices.Equal(got, []string{"docker", "adm", "disk"}) {
		t.Errorf("forbiddenGroups = %v", got)
	}
	if got := forbiddenGroups([]string{"hostbeacon"}); len(got) != 0 {
		t.Errorf("forbiddenGroups = %v, want none", got)
	}
}
