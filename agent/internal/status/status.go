package status

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"vps-tool/agent/internal/model"
)

const (
	maxOSReleaseBytes = 16 * 1024
	maxProcBytes      = 64 * 1024
)

type SystemReport struct {
	Hostname         string    `json:"hostname"`
	OSName           string    `json:"os_name"`
	OSVersion        string    `json:"os_version"`
	Kernel           string    `json:"kernel"`
	Architecture     string    `json:"architecture"`
	CPUPercent       float64   `json:"cpu_percent"`
	MemoryUsedBytes  uint64    `json:"memory_used_bytes"`
	MemoryTotalBytes uint64    `json:"memory_total_bytes"`
	MemoryPercent    float64   `json:"memory_percent"`
	RootUsedBytes    uint64    `json:"root_used_bytes"`
	RootTotalBytes   uint64    `json:"root_total_bytes"`
	RootPercent      float64   `json:"root_percent"`
	UptimeSeconds    uint64    `json:"uptime_seconds"`
	CollectedAt      time.Time `json:"collected_at"`
	Errors           []string  `json:"errors,omitempty"`
}

type cpuSample struct {
	total uint64
	idle  uint64
}

// Collector has no ticker and performs one bounded read per call. The agent
// controls its low-frequency invocation, so idle CPU remains negligible.
type Collector struct {
	mu       sync.Mutex
	previous cpuSample
	procRoot string
	sysRoot  string
}

type Backend interface {
	WarpStatus(context.Context) (model.WarpSnapshot, error)
	XUIStatus(context.Context) (model.XUISnapshot, error)
	GetIP(context.Context) (string, error)
}

type Report struct {
	NodeID           string            `json:"node_id"`
	AgentVersion     string            `json:"agent_version"`
	Hostname         string            `json:"hostname"`
	OSName           string            `json:"os_name"`
	OSVersion        string            `json:"os_version"`
	Kernel           string            `json:"kernel"`
	Architecture     string            `json:"architecture"`
	CPUPercent       float64           `json:"cpu_percent"`
	MemoryUsedBytes  uint64            `json:"memory_used_bytes"`
	MemoryTotalBytes uint64            `json:"memory_total_bytes"`
	MemoryPercent    float64           `json:"memory_percent"`
	RootUsedBytes    uint64            `json:"root_used_bytes"`
	RootTotalBytes   uint64            `json:"root_total_bytes"`
	RootPercent      float64           `json:"root_percent"`
	UptimeSeconds    uint64            `json:"uptime_seconds"`
	WarpState        model.WarpState   `json:"warp_state"`
	XUIState         model.XUIState    `json:"xui_state"`
	EgressIPv4       string            `json:"egress_ipv4,omitempty"`
	EgressIPv6       string            `json:"egress_ipv6,omitempty"`
	CollectedAt      time.Time         `json:"collected_at"`
	Errors           map[string]string `json:"errors,omitempty"`
}

type FullCollector struct {
	system  *Collector
	nodeID  string
	version string
	backend Backend
	mu      sync.RWMutex
	last    Report
	hasLast bool
}

func NewFullCollector(nodeID, version string, backend Backend) *FullCollector {
	return &FullCollector{
		system:  NewCollector(),
		nodeID:  nodeID,
		version: version,
		backend: backend,
	}
}

func (c *FullCollector) Collect(ctx context.Context) (Report, error) {
	system, systemErr := c.system.Collect(ctx)
	report := Report{
		NodeID:           c.nodeID,
		AgentVersion:     c.version,
		Hostname:         system.Hostname,
		OSName:           system.OSName,
		OSVersion:        system.OSVersion,
		Kernel:           system.Kernel,
		Architecture:     system.Architecture,
		CPUPercent:       system.CPUPercent,
		MemoryUsedBytes:  system.MemoryUsedBytes,
		MemoryTotalBytes: system.MemoryTotalBytes,
		MemoryPercent:    system.MemoryPercent,
		RootUsedBytes:    system.RootUsedBytes,
		RootTotalBytes:   system.RootTotalBytes,
		RootPercent:      system.RootPercent,
		UptimeSeconds:    system.UptimeSeconds,
		CollectedAt:      system.CollectedAt,
		WarpState:        model.WarpUnknown,
		XUIState:         model.XUIUnknown,
		Errors:           make(map[string]string),
	}
	for _, item := range system.Errors {
		report.Errors[item] = item
	}
	if c.backend == nil {
		report.Errors["backend_unavailable"] = "backend_unavailable"
		return report, fmt.Errorf("status backend is unavailable")
	}

	var group sync.WaitGroup
	var warpState model.WarpSnapshot
	var xuiState model.XUISnapshot
	var ip string
	var warpErr, xuiErr, ipErr error
	group.Add(3)
	go func() {
		defer group.Done()
		warpState, warpErr = c.backend.WarpStatus(ctx)
	}()
	go func() {
		defer group.Done()
		xuiState, xuiErr = c.backend.XUIStatus(ctx)
	}()
	go func() {
		defer group.Done()
		ip, ipErr = c.backend.GetIP(ctx)
	}()
	group.Wait()

	if warpErr != nil {
		report.Errors["warp_status"] = errorText(warpErr)
	} else {
		report.WarpState = warpState.State
		report.EgressIPv4 = warpState.IPv4
		report.EgressIPv6 = warpState.IPv6
	}
	if xuiErr != nil {
		report.Errors["xui_status"] = errorText(xuiErr)
	} else {
		report.XUIState = xuiState.State
	}
	if ipErr != nil {
		report.Errors["egress_ip"] = errorText(ipErr)
	} else if ip != "" {
		report.EgressIPv4 = ip
	}
	if len(report.Errors) == 0 {
		report.Errors = nil
	}
	c.mu.Lock()
	c.last = report
	c.hasLast = true
	c.mu.Unlock()
	if systemErr != nil {
		return report, systemErr
	}
	if warpErr != nil {
		return report, warpErr
	}
	if xuiErr != nil {
		return report, xuiErr
	}
	if ipErr != nil {
		return report, ipErr
	}
	return report, nil
}

func (c *FullCollector) Last() (Report, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last, c.hasLast
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func NewCollector() *Collector {
	return &Collector{procRoot: "/proc", sysRoot: "/sys"}
}

func (c *Collector) Collect(ctx context.Context) (SystemReport, error) {
	if err := ctx.Err(); err != nil {
		return SystemReport{}, err
	}
	report := SystemReport{
		Architecture: runtime.GOARCH,
		CollectedAt:  time.Now().UTC(),
	}
	if hostname, err := os.Hostname(); err == nil {
		report.Hostname = hostname
	} else {
		report.Errors = append(report.Errors, "hostname_unavailable")
	}
	c.collectOS(&report)
	c.collectCPU(&report)
	c.collectMemory(&report)
	c.collectUptime(&report)
	if total, used, err := rootDiskUsage(); err == nil {
		report.RootTotalBytes = total
		report.RootUsedBytes = used
		report.RootPercent = percentage(used, total)
	} else {
		report.Errors = append(report.Errors, "root_disk_unavailable")
	}
	return report, nil
}

func (c *Collector) collectOS(report *SystemReport) {
	if data, err := readLimited(c.procRoot+"/sys/kernel/osrelease", maxProcBytes); err == nil {
		report.Kernel = strings.TrimSpace(string(data))
	} else {
		report.Errors = append(report.Errors, "kernel_unavailable")
	}
	data, err := readLimited(c.procRoot+"/version", maxProcBytes)
	if err == nil && report.Kernel == "" {
		report.Kernel = strings.TrimSpace(string(data))
	}
	release, err := readLimited(c.procRoot+"/etc/os-release", maxOSReleaseBytes)
	if err != nil {
		report.OSName = runtime.GOOS
		report.OSVersion = runtime.GOOS
		report.Errors = append(report.Errors, "os_release_unavailable")
		return
	}
	values := parseKeyValueFile(release)
	report.OSName = values["PRETTY_NAME"]
	if report.OSName == "" {
		report.OSName = values["NAME"]
	}
	report.OSVersion = values["VERSION_ID"]
	if report.OSVersion == "" {
		report.OSVersion = values["VERSION"]
	}
	if report.OSName == "" {
		report.OSName = runtime.GOOS
	}
}

func (c *Collector) collectCPU(report *SystemReport) {
	data, err := readLimited(c.procRoot+"/stat", maxProcBytes)
	if err != nil {
		report.Errors = append(report.Errors, "cpu_unavailable")
		return
	}
	line := firstLineWithPrefix(string(data), "cpu ")
	fields := strings.Fields(line)
	if len(fields) < 5 {
		report.Errors = append(report.Errors, "cpu_unavailable")
		return
	}
	var values []uint64
	for _, field := range fields[1:] {
		value, parseErr := strconv.ParseUint(field, 10, 64)
		if parseErr != nil {
			report.Errors = append(report.Errors, "cpu_unavailable")
			return
		}
		values = append(values, value)
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	c.mu.Lock()
	previous := c.previous
	c.previous = cpuSample{total: total, idle: idle}
	c.mu.Unlock()
	if previous.total != 0 && total > previous.total && idle >= previous.idle {
		busy := (total - previous.total) - (idle - previous.idle)
		report.CPUPercent = clampPercentage(float64(busy) * 100 / float64(total-previous.total))
	}
}

func (c *Collector) collectMemory(report *SystemReport) {
	data, err := readLimited(c.procRoot+"/meminfo", maxProcBytes)
	if err != nil {
		report.Errors = append(report.Errors, "memory_unavailable")
		return
	}
	values := parseMeminfo(data)
	report.MemoryTotalBytes = values["MemTotal"] * 1024
	available := values["MemAvailable"]
	if available == 0 {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if report.MemoryTotalBytes >= available*1024 {
		report.MemoryUsedBytes = report.MemoryTotalBytes - available*1024
	}
	report.MemoryPercent = percentage(report.MemoryUsedBytes, report.MemoryTotalBytes)
}

func (c *Collector) collectUptime(report *SystemReport) {
	data, err := readLimited(c.procRoot+"/uptime", 256)
	if err != nil {
		report.Errors = append(report.Errors, "uptime_unavailable")
		return
	}
	field := strings.Fields(string(data))
	if len(field) == 0 {
		report.Errors = append(report.Errors, "uptime_unavailable")
		return
	}
	seconds, err := strconv.ParseFloat(field[0], 64)
	if err != nil || seconds < 0 {
		report.Errors = append(report.Errors, "uptime_unavailable")
		return
	}
	report.UptimeSeconds = uint64(seconds)
}

func readLimited(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("file exceeds limit")
	}
	return data, nil
}

func parseKeyValueFile(data []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		values[strings.TrimSpace(key)] = value
	}
	return values
}

func parseMeminfo(data []byte) map[string]uint64 {
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		parsed, err := strconv.ParseUint(fields[0], 10, 64)
		if err == nil {
			values[strings.TrimSpace(key)] = parsed
		}
	}
	return values
}

func firstLineWithPrefix(data, prefix string) string {
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func percentage(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return clampPercentage(float64(used) * 100 / float64(total))
}

func clampPercentage(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
