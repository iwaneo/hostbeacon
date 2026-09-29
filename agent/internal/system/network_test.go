package system

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// addInterface makes /sys/class/net/<name> with its counters. A physical
// interface has a device link.
func addInterface(t *testing.T, root, name string, physical bool, kind int, rx, tx uint64) {
	t.Helper()
	addLinkedInterface(t, root, name, physical, kind, 5, 5, rx, tx)
}

// addLinkedInterface also sets ifindex and iflink. A veth's iflink is its
// peer in another namespace.
func addLinkedInterface(t *testing.T, root, name string, physical bool, kind, index, link int, rx, tx uint64) {
	t.Helper()
	dir := filepath.Join(root, "sys", "class", "net", name)
	writeFile(t, filepath.Join(dir, "type"), strconv.Itoa(kind)+"\n")
	writeFile(t, filepath.Join(dir, "ifindex"), strconv.Itoa(index)+"\n")
	writeFile(t, filepath.Join(dir, "iflink"), strconv.Itoa(link)+"\n")
	writeFile(t, filepath.Join(dir, "statistics", "rx_bytes"), strconv.FormatUint(rx, 10)+"\n")
	writeFile(t, filepath.Join(dir, "statistics", "tx_bytes"), strconv.FormatUint(tx, 10)+"\n")
	if physical {
		if err := os.MkdirAll(filepath.Join(dir, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func interfaceNames(sampler *NetworkSampler) []string {
	var names []string
	for _, item := range sampler.Sample().Interfaces {
		names = append(names, item.Name)
	}
	return names
}

func TestNetworkSkipsVirtualInterfaces(t *testing.T) {
	root := t.TempDir()
	addInterface(t, root, "enp1s0", true, 1, 0, 0)
	addInterface(t, root, "wlan0", true, 1, 0, 0)
	addInterface(t, root, "lo", false, 772, 0, 0)
	addInterface(t, root, "docker0", false, 1, 0, 0)
	addInterface(t, root, "veth1a2b", false, 1, 0, 0)
	addInterface(t, root, "tailscale0", false, 65534, 0, 0)
	now := time.Unix(1000, 0)
	sampler := NewNetworkSampler(root, false, func() time.Time { return now })
	if got := interfaceNames(sampler); len(got) != 2 || got[0] != "enp1s0" || got[1] != "wlan0" {
		t.Errorf("interfaces = %q, want enp1s0 and wlan0", got)
	}
}

func TestNetworkInLXCKeepsTheContainerInterface(t *testing.T) {
	// In LXC, eth0 is a veth with no device, linked to its peer outside.
	// Bridges, veth*, and the kernel's tunnel devices stay skipped.
	root := t.TempDir()
	addLinkedInterface(t, root, "eth0", false, 1, 11, 111, 0, 0)
	addLinkedInterface(t, root, "gretap0", false, 1, 4, 0, 0, 0)
	addLinkedInterface(t, root, "erspan0", false, 1, 5, 0, 0, 0)
	addInterface(t, root, "lo", false, 772, 0, 0)
	addInterface(t, root, "docker0", false, 1, 0, 0)
	if err := os.MkdirAll(filepath.Join(root, "sys", "class", "net", "docker0", "bridge"), 0o755); err != nil {
		t.Fatal(err)
	}
	addLinkedInterface(t, root, "veth1a2b", false, 1, 20, 3, 0, 0)
	addInterface(t, root, "tailscale0", false, 65534, 0, 0)
	sampler := NewNetworkSampler(root, true, time.Now)
	if got := interfaceNames(sampler); len(got) != 1 || got[0] != "eth0" {
		t.Errorf("interfaces = %q, want eth0", got)
	}
}

func TestNetworkRatesAndTotals(t *testing.T) {
	root := t.TempDir()
	addInterface(t, root, "eth0", true, 1, 1_000_000, 5_000)
	now := time.Unix(1000, 0)
	sampler := NewNetworkSampler(root, false, func() time.Time { return now })

	first := sampler.Sample().Interfaces[0]
	if first.RxBytesPerSecond != nil || first.TxBytesPerSecond != nil {
		t.Errorf("first rates = %v %v, want null (no earlier reading)", first.RxBytesPerSecond, first.TxBytesPerSecond)
	}
	if first.RxBytesTotal == nil || *first.RxBytesTotal != 1_000_000 || *first.TxBytesTotal != 5_000 {
		t.Errorf("totals = %v %v", first.RxBytesTotal, first.TxBytesTotal)
	}

	// 30 s later: 3 703 703 bytes in (123 456.8 B/s), 10 bytes out.
	now = now.Add(30 * time.Second)
	addInterface(t, root, "eth0", true, 1, 4_703_703, 5_010)
	second := sampler.Sample().Interfaces[0]
	if second.RxBytesPerSecond == nil || *second.RxBytesPerSecond != 123_000 {
		t.Errorf("download = %v, want 123000 (3 significant digits)", second.RxBytesPerSecond)
	}
	if second.TxBytesPerSecond == nil || *second.TxBytesPerSecond != 0.333 {
		t.Errorf("upload = %v, want 0.333", second.TxBytesPerSecond)
	}
	if *second.RxBytesTotal != 4_700_000 {
		t.Errorf("download total = %d, want 4700000 (3 significant digits)", *second.RxBytesTotal)
	}

	// A counter that went back (the interface was reset) gives no rate.
	now = now.Add(30 * time.Second)
	addInterface(t, root, "eth0", true, 1, 100, 5_020)
	third := sampler.Sample().Interfaces[0]
	if third.RxBytesPerSecond != nil {
		t.Errorf("download after reset = %v, want null", *third.RxBytesPerSecond)
	}
	if third.TxBytesPerSecond == nil || *third.TxBytesPerSecond != 0.333 {
		t.Errorf("upload = %v, want 0.333", third.TxBytesPerSecond)
	}
}

func TestNoNetworkGivesAnEmptyList(t *testing.T) {
	network := NewNetworkSampler(t.TempDir(), false, time.Now).Sample()
	if network.Interfaces == nil || len(network.Interfaces) != 0 {
		t.Errorf("interfaces = %#v, want an empty list", network.Interfaces)
	}
}
