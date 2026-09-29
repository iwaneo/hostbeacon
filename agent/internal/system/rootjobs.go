package system

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/helper"
	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

// RootHelper is the root helper: SMART and containers need root, so the
// network part asks the helper for them (v1 spec §4.1).
type RootHelper interface {
	ReadSmart(ctx context.Context) ([]helper.SmartDisk, error)
	ReadContainers(ctx context.Context) (helper.Containers, error)
	WatchContainers(ctx context.Context, changed func()) error
}

// SmartSampler makes the SMART group. A disk in standby is not read, so it
// keeps the values of its last read.
type SmartSampler struct {
	helper RootHelper
	last   map[string]protocol.SmartDisk
}

// Sample reads SMART. It is nil when the helper cannot answer, so the last
// group stays.
func (s *SmartSampler) Sample(ctx context.Context) *protocol.Smart {
	disks, err := s.helper.ReadSmart(ctx)
	if err != nil {
		return nil
	}
	return s.group(disks)
}

func (s *SmartSampler) group(disks []helper.SmartDisk) *protocol.Smart {
	group := &protocol.Smart{Disks: []protocol.SmartDisk{}}
	last := map[string]protocol.SmartDisk{}
	for _, disk := range disks {
		item, known := s.last[disk.Device]
		if !disk.Standby || !known {
			item = protocol.SmartDisk{
				Device:             disk.Device,
				Health:             disk.Health,
				TemperatureCelsius: whole(disk.TemperatureCelsius),
				WearPercent:        whole(disk.WearPercent),
			}
		}
		last[disk.Device] = item
		group.Disks = append(group.Disks, item)
	}
	s.last = last
	slices.SortFunc(group.Disks, func(a, b protocol.SmartDisk) int { return cmp.Compare(a.Device, b.Device) })
	return group
}

func whole(value *float64) *float64 {
	if value == nil {
		return nil
	}
	rounded := math.Round(*value)
	return &rounded
}

// WatchContainers publishes the containers group at start, after a
// container event, and every fullEvery. Reads are at least minGap apart.
// When the helper's event stream fails, it starts again after retry.
func WatchContainers(ctx context.Context, rootHelper RootHelper, minGap, fullEvery, retry time.Duration, publish func(protocol.Containers)) {
	changes := make(chan struct{}, 1)
	signal := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			rootHelper.WatchContainers(ctx, signal)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
				// Events were lost while the watch was down.
				signal()
			}
		}
	}()
	watch(ctx, changes, minGap, fullEvery, func() {
		containers, err := rootHelper.ReadContainers(ctx)
		if ctx.Err() == nil {
			publish(containersGroup(containers, err))
		}
	})
}

// containersGroup counts every container and caps the list. An error gives
// unknown counts.
func containersGroup(containers helper.Containers, err error) protocol.Containers {
	if err != nil || containers.Error != "" {
		return protocol.Containers{Items: []protocol.Container{}}
	}
	var running, stopped, unhealthy int64
	items := []protocol.Container{}
	for _, container := range containers.Items {
		state := container.State
		switch state {
		case helper.StateRunning:
			running++
		case helper.StateUnhealthy:
			unhealthy++
		default:
			state = helper.StateStopped
			stopped++
		}
		if len(items) < maxNames {
			items = append(items, protocol.Container{Name: container.Name, State: state})
		}
	}
	count := int64(len(containers.Items))
	return protocol.Containers{Count: &count, Running: &running, Stopped: &stopped, Unhealthy: &unhealthy, Items: items}
}
