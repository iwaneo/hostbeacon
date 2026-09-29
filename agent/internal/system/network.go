package system

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iwaneo/hostbeacon/agent/internal/protocol"
)

const etherType = "1" // ARPHRD_ETHER in /sys/class/net/<name>/type

// NetworkSampler reads the network group: traffic per physical interface.
type NetworkSampler struct {
	root      string
	container bool
	now       func() time.Time
	last      map[string]counters
	lastTime  time.Time
}

type counters struct {
	rx, tx uint64
	ok     bool
}

// NewNetworkSampler reads /sys/class/net under root. In a container (LXC),
// the container's own interface is a veth without a device, so it is kept
// too.
func NewNetworkSampler(root string, container bool, now func() time.Time) *NetworkSampler {
	return &NetworkSampler{root: root, container: container, now: now}
}

// Sample reads the network group. A rate is the average since the last
// Sample, so the first one has no rate. A counter that went back gives no
// rate.
func (n *NetworkSampler) Sample() protocol.Network {
	now := n.now()
	seconds := now.Sub(n.lastTime).Seconds()
	network := protocol.Network{Interfaces: []protocol.Interface{}}
	current := map[string]counters{}
	for _, name := range n.physicalInterfaces() {
		item := protocol.Interface{Name: name}
		rx, rxOK := n.readCounter(name, "rx_bytes")
		tx, txOK := n.readCounter(name, "tx_bytes")
		if rxOK && txOK {
			current[name] = counters{rx: rx, tx: tx, ok: true}
			item.RxBytesTotal, item.TxBytesTotal = roundedBytes(rx), roundedBytes(tx)
			if last := n.last[name]; last.ok && seconds > 0 {
				item.RxBytesPerSecond = rate(last.rx, rx, seconds)
				item.TxBytesPerSecond = rate(last.tx, tx, seconds)
			}
		}
		network.Interfaces = append(network.Interfaces, item)
	}
	n.last, n.lastTime = current, now
	return network
}

// physicalInterfaces lists the interfaces with a device, sorted by name.
func (n *NetworkSampler) physicalInterfaces() []string {
	entries, err := os.ReadDir(filepath.Join(n.root, "sys", "class", "net"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		dir := filepath.Join(n.root, "sys", "class", "net", name)
		if exists(filepath.Join(dir, "device")) {
			names = append(names, name)
			continue
		}
		// In a container: an Ethernet interface linked to a peer outside
		// (veth or macvlan), which is not a bridge and not the host side of a
		// veth pair (Docker in the container). The kernel's tunnel devices
		// (gretap0, erspan0) have no link.
		index, link := readTrimmed(filepath.Join(dir, "ifindex")), readTrimmed(filepath.Join(dir, "iflink"))
		if n.container && readTrimmed(filepath.Join(dir, "type")) == etherType && link != "0" && link != "" && link != index &&
			!exists(filepath.Join(dir, "bridge")) && !strings.HasPrefix(name, "veth") {
			names = append(names, name)
		}
	}
	return names
}

func (n *NetworkSampler) readCounter(name, counter string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(n.root, "sys", "class", "net", name, "statistics", counter))
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return value, err == nil
}

func rate(last, current uint64, seconds float64) *float64 {
	if current < last {
		return nil
	}
	value := significant(float64(current-last)/seconds, 3)
	return &value
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
