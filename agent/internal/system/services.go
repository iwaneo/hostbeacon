package system

import (
	"context"
	"slices"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// ServiceSource reads the failed systemd services and signals changes.
type ServiceSource interface {
	Failed(ctx context.Context) ([]string, error)
	Changes() <-chan struct{}
}

// WatchFailedServices publishes the failed services group at start, after
// a change signal, and every fullEvery. Reads are at least minGap apart, so a
// burst of signals gives at most one delta per minGap.
func WatchFailedServices(ctx context.Context, source ServiceSource, minGap, fullEvery time.Duration, publish func(protocol.NameList)) {
	var last time.Time
	check := func() {
		names, err := source.Failed(ctx)
		last = time.Now()
		if ctx.Err() == nil {
			publish(nameList(names, err))
		}
	}
	check()
	full := time.NewTicker(fullEvery)
	defer full.Stop()
	var wait <-chan time.Time
	request := func() {
		if wait == nil {
			wait = time.After(max(0, minGap-time.Since(last)))
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-full.C:
			request()
		case <-source.Changes():
			request()
		case <-wait:
			wait = nil
			check()
		}
	}
}

// nameList sorts and caps the names. An error gives an unknown count.
func nameList(names []string, err error) protocol.NameList {
	if err != nil {
		return protocol.NameList{Names: []string{}}
	}
	slices.Sort(names)
	count := int64(len(names))
	return protocol.NameList{Count: &count, Names: append([]string{}, names[:min(len(names), maxNames)]...)}
}
