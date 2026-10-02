package protocol

import "errors"

// The types below follow protocol/schema.json. A pointer field is a value
// that may be null. A field with omitempty may be left out of the message but
// is never null; every other field is required. The tags enum, format, max,
// and min hold the schema rules that Decode checks (see check in protocol.go).
// Encode refuses a nil slice, because it would be written as null.

// Action is an Action Home Assistant can request.
type Action string

const (
	ActionReboot      Action = "reboot"
	ActionUpdateRun   Action = "update_run"
	ActionAgentUpdate Action = "agent_update"
)

// RefusalReason says why the Agent refused an Action.
type RefusalReason string

const (
	ReasonDisabled         RefusalReason = "disabled"
	ReasonBusy             RefusalReason = "busy"
	ReasonUpdateRunRunning RefusalReason = "update_run_running"
	ReasonTooSoonAfterBoot RefusalReason = "too_soon_after_boot"
	ReasonCannotLog        RefusalReason = "cannot_log"
	ReasonDuplicate        RefusalReason = "duplicate"
	ReasonNotAllowed       RefusalReason = "not_allowed"    // Reserved, unused in v1.
	ReasonUnknownTarget    RefusalReason = "unknown_target" // Reserved, unused in v1.
)

// --- State groups ---

// AgentInfo is the agent group. hello carries the same fields.
type AgentInfo struct {
	Hostname           string   `json:"hostname"`
	AgentVersion       string   `json:"agent_version"`
	NewestAgentVersion *string  `json:"newest_agent_version"`
	Capabilities       []string `json:"capabilities"`
	EnabledActions     []Action `json:"enabled_actions" enum:"reboot,update_run,agent_update"`
}

// System is the system group: CPU, memory, and load.
type System struct {
	CPUPercent      *float64 `json:"cpu_percent"`
	MemoryPercent   *float64 `json:"memory_percent"`
	MemoryUsedBytes *int64   `json:"memory_used_bytes"`
	SwapPercent     *float64 `json:"swap_percent"`
	Load1           *float64 `json:"load_1"`
	Load5           *float64 `json:"load_5"`
	Load15          *float64 `json:"load_15"`
}

// Mount is one real mount in the disks group.
type Mount struct {
	Mount       string   `json:"mount"`
	UsedPercent *float64 `json:"used_percent"`
	FreeBytes   *int64   `json:"free_bytes"`
	TotalBytes  *int64   `json:"total_bytes"`
}

// Disks is the disks group: space per real mount.
type Disks struct {
	Mounts []Mount `json:"mounts"`
}

// Interface is one physical network interface in the network group.
type Interface struct {
	Name             string   `json:"name"`
	RxBytesPerSecond *float64 `json:"rx_bytes_per_second"`
	TxBytesPerSecond *float64 `json:"tx_bytes_per_second"`
	RxBytesTotal     *int64   `json:"rx_bytes_total"`
	TxBytesTotal     *int64   `json:"tx_bytes_total"`
}

// Network is the network group: traffic per physical interface.
type Network struct {
	Interfaces []Interface `json:"interfaces"`
}

// Temperatures is the temperatures group.
type Temperatures struct {
	CPUCelsius *float64 `json:"cpu_celsius"`
}

// NameList is a count of the full set plus a list capped at 100 names.
type NameList struct {
	Count *int64   `json:"count"`
	Names []string `json:"names" max:"100"`
}

// Container is one Docker or Podman container in the containers group.
type Container struct {
	Name  string `json:"name"`
	State string `json:"state" enum:"running,stopped,unhealthy"`
}

// Containers is the containers group. The counts cover every container.
type Containers struct {
	Count     *int64      `json:"count"`
	Running   *int64      `json:"running"`
	Stopped   *int64      `json:"stopped"`
	Unhealthy *int64      `json:"unhealthy"`
	Items     []Container `json:"items" max:"100"`
}

// SmartDisk is one physical disk in the SMART group.
type SmartDisk struct {
	Device             string   `json:"device"`
	Health             *string  `json:"health" enum:"ok,failing"`
	TemperatureCelsius *float64 `json:"temperature_celsius"`
	WearPercent        *float64 `json:"wear_percent"`
}

// Smart is the SMART group.
type Smart struct {
	Disks []SmartDisk `json:"disks"`
}

// Package is one Available update.
type Package struct {
	Name             string `json:"name"`
	InstalledVersion string `json:"installed_version"`
	NewVersion       string `json:"new_version"`
}

// AvailableUpdates is the Available updates group.
type AvailableUpdates struct {
	Count       *int64    `json:"count"`
	Packages    []Package `json:"packages" max:"100"`
	Fingerprint *string   `json:"fingerprint"`
	LastRefresh *string   `json:"last_refresh" format:"time"`
	// RefreshSchedule is off, every_24h, or the hour of the daily refresh,
	// such as 03:00 (v1 spec §4.6). Since 1.2.
	RefreshSchedule *string `json:"refresh_schedule,omitempty" format:"refresh_schedule"`
}

// UpdateRun is the Update run record kept on the Host.
type UpdateRun struct {
	RunID             *string  `json:"run_id" format:"uuid"`
	State             string   `json:"state" enum:"idle,waiting_for_lock,running,finished,result_unknown"`
	Percent           *float64 `json:"percent"`
	StartedAt         *string  `json:"started_at" format:"time"`
	FinishedAt        *string  `json:"finished_at" format:"time"`
	Result            *string  `json:"result" enum:"ok,failed,needs_manual_update"`
	Installed         *int64   `json:"installed"`
	Remaining         *int64   `json:"remaining"`
	Error             *string  `json:"error"`
	NeedsManualUpdate NameList `json:"needs_manual_update"`
}

// Flags is the flags group.
type Flags struct {
	RebootRequired          string  `json:"reboot_required" enum:"yes,no,unknown"`
	PackageTaskRunning      bool    `json:"package_task_running"`
	PackageSystemBroken     bool    `json:"package_system_broken"`
	PackageSystemFixCommand *string `json:"package_system_fix_command"`
	LastBoot                *string `json:"last_boot" format:"time"`
}

// Groups holds state groups. A nil group is not in the message.
type Groups struct {
	Agent            *AgentInfo        `json:"agent,omitempty"`
	System           *System           `json:"system,omitempty"`
	Disks            *Disks            `json:"disks,omitempty"`
	Network          *Network          `json:"network,omitempty"`
	Temperatures     *Temperatures     `json:"temperatures,omitempty"`
	FailedServices   *NameList         `json:"failed_services,omitempty"`
	Containers       *Containers       `json:"containers,omitempty"`
	Smart            *Smart            `json:"smart,omitempty"`
	AvailableUpdates *AvailableUpdates `json:"available_updates,omitempty"`
	UpdateRun        *UpdateRun        `json:"update_run,omitempty"`
	Flags            *Flags            `json:"flags,omitempty"`
}

// --- Messages ---

// Distro is read from /etc/os-release.
type Distro struct {
	ID      *string `json:"id"`
	Name    *string `json:"name"`
	Version *string `json:"version"`
}

// HelloRequest is sent by the Agent after TLS and the key check.
type HelloRequest struct {
	ID                 string   `json:"id"`
	ProtocolVersion    string   `json:"protocol_version"`
	ProtocolMajors     []int    `json:"protocol_majors"`
	InstanceID         string   `json:"instance_id" format:"uuid"`
	RunID              string   `json:"run_id" format:"uuid"`
	CopiedFrom         []string `json:"copied_from" format:"uuid"`
	Hostname           string   `json:"hostname"`
	AgentVersion       string   `json:"agent_version"`
	NewestAgentVersion *string  `json:"newest_agent_version"`
	Capabilities       []string `json:"capabilities"`
	EnabledActions     []Action `json:"enabled_actions" enum:"reboot,update_run,agent_update"`
	Environment        *string  `json:"environment" enum:"bare_metal,vm,lxc"`
	Distro             Distro   `json:"distro"`
	Architecture       string   `json:"architecture"`
	Kernel             *string  `json:"kernel"`
}

// HelloReply is the Integration's answer to hello.
type HelloReply struct {
	ID                 string `json:"id"`
	ReplyTo            string `json:"reply_to"`
	IntegrationVersion string `json:"integration_version"`
	ProtocolVersion    string `json:"protocol_version"`
	ProtocolMajors     []int  `json:"protocol_majors"`
	// HostID is the Host ID Home Assistant knows this Agent as. Since 1.1.
	HostID *string `json:"host_id,omitempty" format:"uuid"`
}

// Snapshot is the full state, sent after hello.
type Snapshot struct {
	ID     string `json:"id"`
	Groups Groups `json:"groups"`
}

// Delta carries only the changed groups. Each group in it is complete.
type Delta struct {
	ID     string `json:"id"`
	Groups Groups `json:"groups" min:"1"`
}

// ActionRequest asks the Agent to run an Action.
type ActionRequest struct {
	ID       string  `json:"id"`
	ActionID string  `json:"action_id" format:"uuid"`
	Action   Action  `json:"action" enum:"reboot,update_run,agent_update"`
	User     *string `json:"user"` // nil means no HA user
}

// ActionOutcome is the result of an Action.
type ActionOutcome struct {
	Result string  `json:"result" enum:"ok,failed"`
	Error  *string `json:"error"`
}

// ActionAck is the Agent's reply to an ActionRequest.
type ActionAck struct {
	ID          string         `json:"id"`
	ReplyTo     string         `json:"reply_to"`
	ActionID    string         `json:"action_id" format:"uuid"`
	Status      string         `json:"status" enum:"accepted,refused"`
	Reason      *RefusalReason `json:"reason" enum:"disabled,busy,update_run_running,too_soon_after_boot,cannot_log,duplicate,not_allowed,unknown_target"`
	FirstResult *ActionOutcome `json:"first_result"` // only for ReasonDuplicate
}

// ActionResult is the later result of an accepted Action.
type ActionResult struct {
	ID       string  `json:"id"`
	ActionID string  `json:"action_id" format:"uuid"`
	Action   Action  `json:"action" enum:"reboot,update_run,agent_update"`
	Result   string  `json:"result" enum:"ok,failed"`
	Error    *string `json:"error"`
}

// PairingRemoveRequest removes the Pairing the connection logged in with.
type PairingRemoveRequest struct {
	ID string `json:"id"`
}

// PairingRemoveReply confirms that the Pairing key is deleted.
type PairingRemoveReply struct {
	ID      string `json:"id"`
	ReplyTo string `json:"reply_to"`
}

// Unsupported is the reply to a request whose type the receiver does not know.
type Unsupported struct {
	ID          string `json:"id"`
	ReplyTo     string `json:"reply_to"`
	RequestType string `json:"request_type"`
}

// Unknown is a message whose type this version does not know.
// Only the envelope is kept.
type Unknown struct {
	Type string
	ID   string
	Kind Kind
}

// Rules that tags cannot say. Decode and Encode call them.

func (s *Snapshot) checkRules() error {
	if s.Groups.Agent == nil || s.Groups.System == nil || s.Groups.UpdateRun == nil || s.Groups.Flags == nil {
		return errors.New("a snapshot needs the agent, system, update_run, and flags groups")
	}
	return nil
}

func (a *ActionAck) checkRules() error {
	if (a.Status == "refused") != (a.Reason != nil) {
		return errors.New("a refusal needs a reason, and an acceptance has none")
	}
	if a.FirstResult != nil && (a.Reason == nil || *a.Reason != ReasonDuplicate) {
		return errors.New("only a duplicate carries the first result")
	}
	return nil
}

func (*HelloRequest) header() (string, Kind)         { return "hello", KindRequest }
func (*HelloReply) header() (string, Kind)           { return "hello", KindReply }
func (*Snapshot) header() (string, Kind)             { return "snapshot", KindEvent }
func (*Delta) header() (string, Kind)                { return "delta", KindEvent }
func (*ActionRequest) header() (string, Kind)        { return "action_request", KindRequest }
func (*ActionAck) header() (string, Kind)            { return "action_ack", KindReply }
func (*ActionResult) header() (string, Kind)         { return "action_result", KindEvent }
func (*PairingRemoveRequest) header() (string, Kind) { return "pairing_remove", KindRequest }
func (*PairingRemoveReply) header() (string, Kind)   { return "pairing_remove", KindReply }
func (*Unsupported) header() (string, Kind)          { return "unsupported", KindReply }
func (u *Unknown) header() (string, Kind)            { return u.Type, u.Kind }

// knownMessages makes an empty message for each known type and kind.
var knownMessages = func() map[[2]string]func() Message {
	byHeader := make(map[[2]string]func() Message)
	for _, newMessage := range []func() Message{
		func() Message { return new(HelloRequest) },
		func() Message { return new(HelloReply) },
		func() Message { return new(Snapshot) },
		func() Message { return new(Delta) },
		func() Message { return new(ActionRequest) },
		func() Message { return new(ActionAck) },
		func() Message { return new(ActionResult) },
		func() Message { return new(PairingRemoveRequest) },
		func() Message { return new(PairingRemoveReply) },
		func() Message { return new(Unsupported) },
	} {
		messageType, kind := newMessage().header()
		byHeader[[2]string{messageType, string(kind)}] = newMessage
	}
	return byHeader
}()
