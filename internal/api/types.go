// Package api holds the JSON types shared by clusterd and labctl. They mirror
// api/openapi.yaml; change both together.
package api

import "time"

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// Health is the body of GET /v1/health.
type Health struct {
	Status string `json:"status"`
}

// Whoami is the body of GET /v1/whoami.
type Whoami struct {
	Site    string `json:"site"`
	Host    string `json:"host"`
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Version string `json:"version"`
}

// Node is what one clusterd instance knows about its own host (GET /v1/node).
type Node struct {
	Host              string        `json:"host"`
	Version           string        `json:"version"`
	UptimeSeconds     int64         `json:"uptime_seconds"`
	Load1             float64       `json:"load1"`
	MemTotalBytes     uint64        `json:"mem_total_bytes"`
	MemAvailableBytes uint64        `json:"mem_available_bytes"`
	VIPs              []string      `json:"vips"`
	VMs               []VMUnit      `json:"vms"`
	Stores            []StoreHealth `json:"stores"`
	Errors            []string      `json:"errors,omitempty"`
}

// VMUnit is the systemd state of one microvm@ unit.
type VMUnit struct {
	Name   string `json:"name"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
}

// StoreHealth says whether a local monitoring store answers.
type StoreHealth struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

// Status is the body of GET /v1/status: the whole site at a glance.
type Status struct {
	Site        string       `json:"site"`
	GeneratedAt time.Time    `json:"generated_at"`
	ServedBy    string       `json:"served_by"`
	Hosts       []HostStatus `json:"hosts"`
	VMs         []VMStatus   `json:"vms"`
	K3s         K3sStatus    `json:"k3s"`
	Ceph        CephStatus   `json:"ceph"`
	VIPs        []VIPStatus  `json:"vips"`
	Disks       DisksStatus  `json:"disks"`
	S3          S3Status     `json:"s3"`
}

// HostsStatus is GET /v1/status/hosts.
type HostsStatus struct {
	Hosts []HostStatus `json:"hosts"`
}

// VMsStatus is GET /v1/status/vms.
type VMsStatus struct {
	VMs []VMStatus `json:"vms"`
}

// VIPsStatus is GET /v1/status/vips.
type VIPsStatus struct {
	VIPs []VIPStatus `json:"vips"`
}

// HostStatus is one hypervisor in /v1/status.
type HostStatus struct {
	Name           string        `json:"name"`
	Reachable      bool          `json:"reachable"`
	Version        string        `json:"version,omitempty"`
	UptimeSeconds  int64         `json:"uptime_seconds,omitempty"`
	Load1          float64       `json:"load1"`
	MemUsedPercent float64       `json:"mem_used_percent"`
	Stores         []StoreHealth `json:"stores,omitempty"`
	Error          string        `json:"error,omitempty"`
}

// VM states in VMStatus.State.
const (
	VMRunning  = "running"
	VMStopped  = "stopped"
	VMFailed   = "failed"
	VMStarting = "starting"
	VMMissing  = "missing"
	VMDisabled = "disabled"
	VMUnknown  = "unknown"
)

// VMStatus is one VM from the inventory in /v1/status.
type VMStatus struct {
	Name  string `json:"name"`
	Host  string `json:"host"`
	State string `json:"state"`
}

// K3sStatus is the Kubernetes part of /v1/status.
type K3sStatus struct {
	Reachable bool      `json:"reachable"`
	Nodes     []K3sNode `json:"nodes"`
	Error     string    `json:"error,omitempty"`
}

// K3sNode is one Kubernetes node.
type K3sNode struct {
	Name        string   `json:"name"`
	Ready       bool     `json:"ready"`
	Schedulable bool     `json:"schedulable"`
	Roles       []string `json:"roles"`
	Version     string   `json:"version"`
	Zone        string   `json:"zone,omitempty"`
	BootID      string   `json:"-"`
}

// CephStatus is the Ceph part of /v1/status.
type CephStatus struct {
	Health         string   `json:"health"`
	Checks         []string `json:"checks"`
	OSDsTotal      int      `json:"osds_total"`
	OSDsUp         int      `json:"osds_up"`
	OSDsIn         int      `json:"osds_in"`
	PGsTotal       int      `json:"pgs_total"`
	PGsActiveClean int      `json:"pgs_active_clean"`
	// Raw capacity over all OSDs (every object is stored three times).
	BytesTotal int64  `json:"bytes_total"`
	BytesUsed  int64  `json:"bytes_used"`
	BytesAvail int64  `json:"bytes_avail"`
	Error      string `json:"error,omitempty"`
	// AsOf is when Ceph was asked; the API refreshes it every 10 s.
	AsOf time.Time `json:"as_of"`
}

// VIPStatus says which host holds a floating address. Owners has more than
// one entry only during a split brain, and none when nobody holds it.
type VIPStatus struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Owners  []string `json:"owners"`
}

// ActionEvent is one line of an action's progress stream
// (application/x-ndjson). The last line has Status "done" or "error".
type ActionEvent struct {
	Time    time.Time `json:"time"`
	Status  string    `json:"status"` // progress, done, error
	Message string    `json:"message"`
}

// Action event statuses.
const (
	EventProgress = "progress"
	EventDone     = "done"
	EventError    = "error"
)

// RestartPlan is what a VM restart would do (POST …/restart?dry_run=1).
type RestartPlan struct {
	VM    string   `json:"vm"`
	Host  string   `json:"host"`
	Roles []string `json:"roles"`
	// Evict lists the pods a drain moves away ("namespace/name").
	Evict []string `json:"evict"`
	// Skipped counts DaemonSet and static pods, which stay.
	Skipped int      `json:"skipped"`
	Steps   []string `json:"steps"`
	// LockHolder is set when another action holds the lock right now.
	LockHolder string `json:"lock_holder,omitempty"`
}

// DisksStatus is GET /v1/status/disks: physical disks with how full they
// are, filesystems of hosts and VMs, and Ceph OSDs. From the monitoring
// stores (SMART and node metrics, at most ~30 s old) and Ceph (~60 s).
type DisksStatus struct {
	Disks       []Disk       `json:"disks"`
	Filesystems []Filesystem `json:"filesystems"`
	OSDs        []OSD        `json:"osds"`
	Errors      []string     `json:"errors,omitempty"`
}

// Disk is one physical drive.
type Disk struct {
	Host   string `json:"host"`
	Device string `json:"device"` // nvme0, sda
	Type   string `json:"type"`   // nvme, hdd, ssd
	Model  string `json:"model"`
	Serial string `json:"serial"`
	// Role is what the drive holds: "system", "osd.N" or "" when unknown.
	Role        string  `json:"role"`
	SizeBytes   int64   `json:"size_bytes"`
	UsedBytes   int64   `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
	// WearPercent is the NVMe "percentage used" (endurance), absent for HDDs.
	WearPercent  *float64 `json:"wear_percent,omitempty"`
	TemperatureC float64  `json:"temperature_c"`
	Healthy      bool     `json:"healthy"` // SMART overall assessment
	PowerOnHours int64    `json:"power_on_hours"`
	MediaErrors  *int64   `json:"media_errors,omitempty"`
}

// Filesystem is one mounted filesystem on a host or a VM.
type Filesystem struct {
	Host        string  `json:"host"` // host or VM name
	Mountpoint  string  `json:"mountpoint"`
	Device      string  `json:"device"`
	FSType      string  `json:"fstype"`
	SizeBytes   int64   `json:"size_bytes"`
	UsedBytes   int64   `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// OSD is one Ceph OSD.
type OSD struct {
	ID          int     `json:"id"`
	Host        string  `json:"host"`
	Device      string  `json:"device"`
	DeviceClass string  `json:"device_class"`
	SizeBytes   int64   `json:"size_bytes"`
	UsedBytes   int64   `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
	Up          bool    `json:"up"`
	In          bool    `json:"in"`
}

// S3Status is GET /v1/status/s3: the object store behind the S3 endpoint.
type S3Status struct {
	Endpoint string      `json:"endpoint"`
	Gateways []S3Gateway `json:"gateways"`
	Users    int         `json:"users"`
	Buckets  []S3Bucket  `json:"buckets"`
	Objects  int64       `json:"objects"`
	// SizeBytes is what the buckets hold (one copy).
	SizeBytes int64  `json:"size_bytes"`
	Pool      S3Pool `json:"pool"`
	// CertExpires is when the endpoint's certificate expires.
	CertExpires *time.Time `json:"cert_expires,omitempty"`
	Errors      []string   `json:"errors,omitempty"`
}

// S3Gateway is one RGW behind the endpoint.
type S3Gateway struct {
	Host string `json:"host"`
	Up   bool   `json:"up"`
}

// S3Bucket is one bucket. Quotas of 0 mean none.
type S3Bucket struct {
	Name         string     `json:"name"`
	Owner        string     `json:"owner"`
	Objects      int64      `json:"objects"`
	SizeBytes    int64      `json:"size_bytes"`
	QuotaBytes   int64      `json:"quota_bytes"`
	QuotaObjects int64      `json:"quota_objects"`
	Created      *time.Time `json:"created,omitempty"`
}

// S3Pool is the Ceph pool holding the object data: how full the store is.
type S3Pool struct {
	Name          string  `json:"name"`
	StoredBytes   int64   `json:"stored_bytes"`
	MaxAvailBytes int64   `json:"max_avail_bytes"`
	UsedPercent   float64 `json:"used_percent"`
}
