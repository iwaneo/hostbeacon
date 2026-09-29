// Package helper is the root helper (v1 spec §4.1): a small program that
// runs as root, has no network code, and does only a fixed list of jobs for
// the network part. It listens on a local socket and accepts only the Agent
// user. It reads the Host config itself on every request and refuses
// everything when it cannot (fail closed).
//
// One request per connection: the caller sends one JSON line with the job
// name, and the helper answers with one JSON line. A watch job keeps the
// connection open and sends one line per change.
package helper

import "encoding/json"

// DefaultSocket is where the helper listens.
const DefaultSocket = "/run/hostbeacon-helper/helper.sock"

// The fixed job list. The action job runs an Action: the helper answers the
// Ack, and when it is accepted, the result on a second line.
const (
	JobReadSmart       = "read_smart"
	JobReadContainers  = "read_containers"
	JobWatchContainers = "watch_containers"
	JobReadSMBIOSUUID  = "read_smbios_uuid"
	JobAction          = "action"
)

// SmartDisk is what SMART says about one physical disk.
type SmartDisk struct {
	Device string `json:"device"`
	// Standby: the disk sleeps, so it was not read and not woken.
	Standby            bool     `json:"standby"`
	Health             *string  `json:"health"` // ok, failing, or null
	TemperatureCelsius *float64 `json:"temperature_celsius"`
	WearPercent        *float64 `json:"wear_percent"` // SSD only
}

// Container states.
const (
	StateRunning   = "running"
	StateStopped   = "stopped"
	StateUnhealthy = "unhealthy"
)

// Container is one Docker or Podman container.
type Container struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// Containers lists every container. Engines names the container engines
// found on the Host; without one there are no container sensors. Error says
// why an engine could not list its containers; the list is then empty.
type Containers struct {
	Engines []string    `json:"engines"`
	Items   []Container `json:"items"`
	Error   string      `json:"error,omitempty"`
}

type request struct {
	Job    string         `json:"job"`
	Action *ActionRequest `json:"action,omitempty"`
}

type reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}
