package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/brutella/dnssd"

	"github.com/iwaneo/hostbeacon/agent/internal/identity"
)

// lookup browses for the Agent with this instance ID. The responder answers
// only after probing, so it asks again until the deadline.
func lookup(t *testing.T, id string, deadline time.Duration) (dnssd.BrowseEntry, bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		found := make(chan dnssd.BrowseEntry, 16)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		go dnssd.LookupType(ctx, ServiceType+".local.", func(e dnssd.BrowseEntry) { found <- e }, func(dnssd.BrowseEntry) {})
		for waiting := true; waiting; {
			select {
			case e := <-found:
				if e.Text["id"] == id { // not another Agent on this network
					cancel()
					return e, true
				}
			case <-ctx.Done():
				waiting = false
			}
		}
		cancel()
	}
	return dnssd.BrowseEntry{}, false
}

func TestAnnouncesTheHostnamePortAndInstanceID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := identity.NewUUID()
	go Announce(ctx, "test-host "+id[:8], id, 8743)

	entry, ok := lookup(t, id, 15*time.Second)
	if !ok {
		t.Fatal("the announcement was not found")
	}
	if entry.Name != "test-host "+id[:8] || entry.Port != 8743 {
		t.Errorf("entry = %+v", entry)
	}
}
