package domain

import (
	"errors"
	"strings"
	"testing"
)

// The live fleet host: a 40GB disk with ~22GB free, 8 cpus, 16GB RAM.
var liveHost = HostCapacityMeasurement{
	VCPU:            8,
	MemTotalMB:      16384,
	DiskTotalMB:     40960,
	DiskAvailableMB: 22528,
}

func TestResolveHostCapacity(t *testing.T) {
	tests := []struct {
		name     string
		measured HostCapacityMeasurement
		override HostCapacityOverride
		want     HostCapacity
		wantErr  error
	}{
		{
			// No override at all: the measurement IS the advertisement (minus reserve).
			// This is the case that matters most — nothing in the fleet provisioning
			// sets these keys, so this is the path a real host takes.
			name:     "measurement is used when nothing is configured",
			measured: liveHost,
			override: HostCapacityOverride{DiskReserveMB: 4096},
			want:     HostCapacity{VCPU: 8, MemMB: 16384, DiskMB: 18432},
		},
		{
			name:     "reserve is subtracted from measured free disk",
			measured: liveHost,
			override: HostCapacityOverride{DiskReserveMB: 2048},
			want:     HostCapacity{VCPU: 8, MemMB: 16384, DiskMB: 20480},
		},
		{
			name:     "zero reserve advertises all free disk",
			measured: liveHost,
			override: HostCapacityOverride{},
			want:     HostCapacity{VCPU: 8, MemMB: 16384, DiskMB: 22528},
		},
		{
			// Under-advertising is a deliberate reservation and must stay allowed.
			name:     "configured below measured is accepted for all three",
			measured: liveHost,
			override: HostCapacityOverride{VCPU: 4, MemMB: 8192, DiskMB: 10240, DiskReserveMB: 1024},
			want:     HostCapacity{VCPU: 4, MemMB: 8192, DiskMB: 9216},
		},
		{
			name:     "configured equal to measured is accepted",
			measured: liveHost,
			override: HostCapacityOverride{VCPU: 8, MemMB: 16384, DiskMB: 22528},
			want:     HostCapacity{VCPU: 8, MemMB: 16384, DiskMB: 22528},
		},
		{
			// The old default: 50GB asserted against 22GB free.
			name:     "configured disk above measured free disk refuses",
			measured: liveHost,
			override: HostCapacityOverride{DiskMB: 51200},
			wantErr:  ErrHostCapacityOverAdvertised,
		},
		{
			// Free, not total: 30GB is under the 40GB disk and still more than exists.
			name:     "configured disk above free but below total refuses",
			measured: liveHost,
			override: HostCapacityOverride{DiskMB: 30720},
			wantErr:  ErrHostCapacityOverAdvertised,
		},
		{
			name:     "configured vcpu above measured refuses",
			measured: liveHost,
			override: HostCapacityOverride{VCPU: 16},
			wantErr:  ErrHostCapacityOverAdvertised,
		},
		{
			name:     "configured memory above measured refuses",
			measured: liveHost,
			override: HostCapacityOverride{MemMB: 32768},
			wantErr:  ErrHostCapacityOverAdvertised,
		},
		{
			name:     "reserve that consumes the whole disk refuses",
			measured: liveHost,
			override: HostCapacityOverride{DiskReserveMB: 22528},
			wantErr:  ErrHostDiskReserveInvalid,
		},
		{
			// A negative reserve would ADD capacity the host does not have.
			name:     "negative reserve refuses",
			measured: liveHost,
			override: HostCapacityOverride{DiskReserveMB: -4096},
			wantErr:  ErrHostDiskReserveInvalid,
		},
		{
			// The reserve is applied AFTER the override, so it can still exhaust a
			// deliberately narrowed disk.
			name:     "reserve larger than the configured disk refuses",
			measured: liveHost,
			override: HostCapacityOverride{DiskMB: 2048, DiskReserveMB: 4096},
			wantErr:  ErrHostDiskReserveInvalid,
		},
		{
			name:     "unmeasured disk refuses",
			measured: HostCapacityMeasurement{VCPU: 8, MemTotalMB: 16384},
			wantErr:  ErrHostCapacityUnmeasured,
		},
		{
			name:     "unmeasured cpu refuses",
			measured: HostCapacityMeasurement{MemTotalMB: 16384, DiskTotalMB: 40960, DiskAvailableMB: 22528},
			wantErr:  ErrHostCapacityUnmeasured,
		},
		{
			name:     "unmeasured memory refuses",
			measured: HostCapacityMeasurement{VCPU: 8, DiskTotalMB: 40960, DiskAvailableMB: 22528},
			wantErr:  ErrHostCapacityUnmeasured,
		},
		{
			// A full filesystem measures total but zero available.
			name:     "full filesystem refuses",
			measured: HostCapacityMeasurement{VCPU: 8, MemTotalMB: 16384, DiskTotalMB: 40960, DiskAvailableMB: 0},
			wantErr:  ErrHostCapacityUnmeasured,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveHostCapacity(tt.measured, tt.override)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if got != (HostCapacity{}) {
					t.Errorf("a refused resolution must advertise nothing, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("capacity = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A refusal has to name BOTH numbers: an operator reading only "capacity
// refused" cannot tell whether to fix the config or the machine.
func TestResolveHostCapacity_RefusalNamesBothNumbers(t *testing.T) {
	_, err := ResolveHostCapacity(liveHost, HostCapacityOverride{DiskMB: 51200})
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"51200", "22528", "40960"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}
