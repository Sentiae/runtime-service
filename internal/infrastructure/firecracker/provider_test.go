package firecracker

import (
	"testing"
)

func TestParseCPUTime(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
	}{
		{
			name:     "typical cpu line",
			input:    "cpu  1000 200 300 5000 100 0 0 0 0 0",
			expected: 15000, // (1000+200+300) * 10
		},
		{
			name:     "zeros",
			input:    "cpu  0 0 0 0 0 0 0 0 0 0",
			expected: 0,
		},
		{
			name:     "empty string",
			input:    "",
			expected: 0,
		},
		{
			name:     "invalid format",
			input:    "not a cpu line",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseCPUTime(tt.input)
			if result != tt.expected {
				t.Errorf("parseCPUTime(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseMemInfo(t *testing.T) {
	meminfo := `MemTotal:        1024000 kB
MemFree:          512000 kB
MemAvailable:     700000 kB
Buffers:           51200 kB
Cached:           102400 kB
SwapCached:            0 kB
`
	peakMB, avgMB := parseMemInfo(meminfo)

	// Used = 1024000 - 512000 - 51200 - 102400 = 358400 kB = ~350 MB
	expectedMB := 358400.0 / 1024.0
	if peakMB < expectedMB-1 || peakMB > expectedMB+1 {
		t.Errorf("parseMemInfo peak = %f, want ~%f", peakMB, expectedMB)
	}
	if avgMB != peakMB {
		t.Errorf("parseMemInfo avg should equal peak for single sample, got %f vs %f", avgMB, peakMB)
	}
}

func TestParseMemInfo_Empty(t *testing.T) {
	peakMB, avgMB := parseMemInfo("")
	if peakMB != 0 || avgMB != 0 {
		t.Errorf("parseMemInfo empty should be 0, got peak=%f avg=%f", peakMB, avgMB)
	}
}

func TestParseDiskStats(t *testing.T) {
	diskstats := `   8       0 sda 100 0 2000 0 50 0 1000 0 0 0 0 0 0 0 0
   8       1 sda1 10 0 200 0 5 0 100 0 0 0 0 0 0 0 0
`
	readBytes, writeBytes := parseDiskStats(diskstats)

	// First line: read_sectors=2000, write_sectors=1000
	// Second line: read_sectors=200, write_sectors=100
	// Total: (2000+200)*512 = 1126400, (1000+100)*512 = 563200
	expectedRead := int64(2200 * 512)
	expectedWrite := int64(1100 * 512)

	if readBytes != expectedRead {
		t.Errorf("parseDiskStats read = %d, want %d", readBytes, expectedRead)
	}
	if writeBytes != expectedWrite {
		t.Errorf("parseDiskStats write = %d, want %d", writeBytes, expectedWrite)
	}
}

func TestParseDiskStats_Empty(t *testing.T) {
	readBytes, writeBytes := parseDiskStats("")
	if readBytes != 0 || writeBytes != 0 {
		t.Errorf("parseDiskStats empty should be 0, got read=%d write=%d", readBytes, writeBytes)
	}
}

func TestParseNetDev(t *testing.T) {
	netdev := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000       10    0    0    0     0          0         0     1000       10    0    0    0     0       0          0
  eth0: 5000       50    0    0    0     0          0         0     3000       30    0    0    0     0       0          0
`
	rxBytes, txBytes := parseNetDev(netdev)

	// Should only include eth0 (skip lo)
	if rxBytes != 5000 {
		t.Errorf("parseNetDev rx = %d, want 5000", rxBytes)
	}
	if txBytes != 3000 {
		t.Errorf("parseNetDev tx = %d, want 3000", txBytes)
	}
}

func TestParseNetDev_Empty(t *testing.T) {
	rxBytes, txBytes := parseNetDev("")
	if rxBytes != 0 || txBytes != 0 {
		t.Errorf("parseNetDev empty should be 0, got rx=%d tx=%d", rxBytes, txBytes)
	}
}

func TestInferGuestIP(t *testing.T) {
	tests := []struct {
		hostIP   string
		expected string
		wantErr  bool
	}{
		{"172.16.0.1", "172.16.0.2", false},
		{"192.168.1.1", "192.168.1.2", false},
		{"10.0.0.1", "10.0.0.2", false},
		{"invalid", "", true},
		{"1.2.3", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.hostIP, func(t *testing.T) {
			result, err := inferGuestIP(tt.hostIP)
			if tt.wantErr && err == nil {
				t.Error("Expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("inferGuestIP(%q) = %q, want %q", tt.hostIP, result, tt.expected)
			}
		})
	}
}
