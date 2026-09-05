package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const resourceSampleInterval = time.Second

type hostCounters struct {
	At              time.Time
	CPUTotal        uint64
	CPUIdle         uint64
	MemoryTotal     uint64
	MemoryAvailable uint64
	NetworkReceived uint64
	NetworkSent     uint64
}

type resourceUsage struct {
	CPUPercent               float64
	CPUReady                 bool
	MemoryUsed               uint64
	MemoryTotal              uint64
	NetworkReceivedPerSecond float64
	NetworkSentPerSecond     float64
	NetworkReady             bool
}

type resourceMonitor struct {
	mu          sync.Mutex
	readFile    func(string) ([]byte, error)
	lastAttempt time.Time
	previous    hostCounters
	current     hostCounters
}

func newResourceMonitor() *resourceMonitor {
	return &resourceMonitor{readFile: os.ReadFile}
}

func (m *resourceMonitor) usage(now time.Time) resourceUsage {
	if m == nil {
		return resourceUsage{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastAttempt.IsZero() || !now.Before(m.lastAttempt.Add(resourceSampleInterval)) {
		m.lastAttempt = now
		current, err := readHostCounters(now, m.readFile)
		if err == nil {
			m.previous = m.current
			m.current = current
		}
	}
	return hostResourceUsage(m.previous, m.current)
}

func readHostCounters(at time.Time, readFile func(string) ([]byte, error)) (hostCounters, error) {
	stat, err := readFile("/proc/stat")
	if err != nil {
		return hostCounters{}, err
	}
	memory, err := readFile("/proc/meminfo")
	if err != nil {
		return hostCounters{}, err
	}
	network, err := readFile("/proc/net/dev")
	if err != nil {
		return hostCounters{}, err
	}
	return parseHostCounters(at, stat, memory, network)
}

func parseHostCounters(at time.Time, stat, memory, network []byte) (hostCounters, error) {
	cpuTotal, cpuIdle, err := parseCPU(stat)
	if err != nil {
		return hostCounters{}, err
	}
	memoryTotal, memoryAvailable, err := parseMemory(memory)
	if err != nil {
		return hostCounters{}, err
	}
	received, sent, err := parseNetwork(network)
	if err != nil {
		return hostCounters{}, err
	}
	return hostCounters{
		At:              at,
		CPUTotal:        cpuTotal,
		CPUIdle:         cpuIdle,
		MemoryTotal:     memoryTotal,
		MemoryAvailable: memoryAvailable,
		NetworkReceived: received,
		NetworkSent:     sent,
	}, nil
}

func parseCPU(data []byte) (uint64, uint64, error) {
	fields := strings.Fields(strings.SplitN(string(data), "\n", 2)[0])
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("invalid /proc/stat")
	}
	values := make([]uint64, 8)
	for i := range values {
		value, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse /proc/stat: %w", err)
		}
		values[i] = value
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	return total, values[3] + values[4], nil
}

func parseMemory(data []byte) (uint64, uint64, error) {
	var total uint64
	var available uint64
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[2] != "kB" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse /proc/meminfo: %w", err)
		}
		switch fields[0] {
		case "MemTotal:":
			total = value * 1024
		case "MemAvailable:":
			available = value * 1024
		}
	}
	if total == 0 || available > total {
		return 0, 0, fmt.Errorf("invalid /proc/meminfo")
	}
	return total, available, nil
}

func parseNetwork(data []byte) (uint64, uint64, error) {
	var received uint64
	var sent uint64
	for line := range strings.Lines(string(data)) {
		name, values, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(values)
		if len(fields) < 16 {
			return 0, 0, fmt.Errorf("invalid /proc/net/dev")
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse /proc/net/dev: %w", err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("parse /proc/net/dev: %w", err)
		}
		received += rx
		sent += tx
	}
	return received, sent, nil
}

func hostResourceUsage(previous, current hostCounters) resourceUsage {
	usage := resourceUsage{
		MemoryUsed:  current.MemoryTotal - current.MemoryAvailable,
		MemoryTotal: current.MemoryTotal,
	}
	if !previous.At.IsZero() && current.CPUTotal > previous.CPUTotal && current.CPUIdle >= previous.CPUIdle {
		total := current.CPUTotal - previous.CPUTotal
		idle := current.CPUIdle - previous.CPUIdle
		if idle <= total {
			usage.CPUPercent = float64(total-idle) * 100 / float64(total)
			usage.CPUReady = true
		}
	}
	duration := current.At.Sub(previous.At).Seconds()
	if !previous.At.IsZero() && duration > 0 && current.NetworkReceived >= previous.NetworkReceived && current.NetworkSent >= previous.NetworkSent {
		usage.NetworkReceivedPerSecond = float64(current.NetworkReceived-previous.NetworkReceived) / duration
		usage.NetworkSentPerSecond = float64(current.NetworkSent-previous.NetworkSent) / duration
		usage.NetworkReady = true
	}
	return usage
}

func dashboardResourceMetrics(usage resourceUsage) []dashboardMetric {
	cpu := "--"
	if usage.CPUReady {
		cpu = fmt.Sprintf("%.1f%%", usage.CPUPercent)
	}
	memory := "--"
	if usage.MemoryTotal > 0 {
		memory = fmt.Sprintf("%.2f / %.2f GB", float64(usage.MemoryUsed)/1e9, float64(usage.MemoryTotal)/1e9)
	}
	received := "--"
	sent := "--"
	if usage.NetworkReady {
		received = fmt.Sprintf("%.2f Mbps", usage.NetworkReceivedPerSecond*8/1e6)
		sent = fmt.Sprintf("%.2f Mbps", usage.NetworkSentPerSecond*8/1e6)
	}
	return []dashboardMetric{
		{Name: "CPU", Value: cpu},
		{Name: "RAM", Value: memory},
		{Name: "network in", Value: received},
		{Name: "network out", Value: sent},
	}
}
