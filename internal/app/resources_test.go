package app

import (
	"os"
	"testing"
	"time"
)

func TestParseHostCounters(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	counters, err := parseHostCounters(
		now,
		[]byte("cpu  100 20 30 400 50 10 5 1 9 8\n"),
		[]byte("MemTotal:        8192 kB\nMemAvailable:    3072 kB\n"),
		[]byte("Inter-| Receive | Transmit\n lo: 999 0 0 0 0 0 0 0 888 0 0 0 0 0 0 0\n eth0: 2048 0 0 0 0 0 0 0 1024 0 0 0 0 0 0 0\n wlan0: 512 0 0 0 0 0 0 0 256 0 0 0 0 0 0 0\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if counters.At != now || counters.CPUTotal != 616 || counters.CPUIdle != 450 {
		t.Fatalf("CPU counters = %+v", counters)
	}
	if counters.MemoryTotal != 8192*1024 || counters.MemoryAvailable != 3072*1024 {
		t.Fatalf("memory counters = %+v", counters)
	}
	if counters.NetworkReceived != 2560 || counters.NetworkSent != 1280 {
		t.Fatalf("network counters = %+v", counters)
	}
}

func TestHostResourceUsage(t *testing.T) {
	start := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	previous := hostCounters{At: start, CPUTotal: 1000, CPUIdle: 700, NetworkReceived: 1000, NetworkSent: 2000}
	current := hostCounters{
		At:              start.Add(2 * time.Second),
		CPUTotal:        1200,
		CPUIdle:         750,
		MemoryTotal:     8 << 30,
		MemoryAvailable: 3 << 30,
		NetworkReceived: 5096,
		NetworkSent:     4048,
	}

	usage := hostResourceUsage(previous, current)
	if !usage.CPUReady || usage.CPUPercent != 75 {
		t.Fatalf("CPU usage = %+v", usage)
	}
	if usage.MemoryUsed != 5<<30 || usage.MemoryTotal != 8<<30 {
		t.Fatalf("memory usage = %+v", usage)
	}
	if !usage.NetworkReady || usage.NetworkReceivedPerSecond != 2048 || usage.NetworkSentPerSecond != 1024 {
		t.Fatalf("network usage = %+v", usage)
	}

	metrics := dashboardResourceMetrics(usage)
	want := []dashboardMetric{
		{Name: "CPU", Value: "75.0%"},
		{Name: "RAM", Value: "5.37 / 8.59 GB"},
		{Name: "network in", Value: "0.02 Mbps"},
		{Name: "network out", Value: "0.01 Mbps"},
	}
	if len(metrics) != len(want) {
		t.Fatalf("resource metrics = %+v", metrics)
	}
	for i := range want {
		if metrics[i] != want[i] {
			t.Fatalf("resource metric %d = %+v, want %+v", i, metrics[i], want[i])
		}
	}
}

func BenchmarkResourceMonitorUnavailable(b *testing.B) {
	m := newResourceMonitor()
	m.readFile = func(string) ([]byte, error) { return os.ReadFile("/nonexistent-codex-balancer-proc/stat") }
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		m.usage(now)
	}
}

func TestResourceMonitorRetriesAfterInterval(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	calls := 0
	m := &resourceMonitor{readFile: func(path string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, os.ErrNotExist
		}
		switch path {
		case "/proc/stat":
			return []byte("cpu 100 0 0 100 0 0 0 0\n"), nil
		case "/proc/meminfo":
			return []byte("MemTotal: 100 kB\nMemAvailable: 50 kB\n"), nil
		default:
			return []byte("eth0: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n"), nil
		}
	}}
	if got := m.usage(now); got.MemoryTotal != 0 {
		t.Fatalf("failed sample = %+v", got)
	}
	m.usage(now.Add(resourceSampleInterval - 1))
	if calls != 1 {
		t.Fatalf("read calls before retry = %d, want 1", calls)
	}
	if got := m.usage(now.Add(resourceSampleInterval)); got.MemoryTotal != 100*1024 {
		t.Fatalf("recovered sample = %+v", got)
	}
	if calls != 4 {
		t.Fatalf("read calls after retry = %d, want 4", calls)
	}
	m.usage(now.Add(resourceSampleInterval + 1))
	if calls != 4 {
		t.Fatalf("successful sample not cached: %d calls", calls)
	}
}
