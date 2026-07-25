//go:build unit

package firecracker

import (
	"reflect"
	"testing"
)

// Gate condition A2 (#p19-firecracker-cache-type-fsync): Firecracker's default
// cache_type is `Unsafe`, which does not turn a guest FLUSH into a host fsync —
// every Postgres commit inside the guest would be acked without the bytes
// reaching stable storage. EVERY drive this platform configures goes through
// driveConfigBody precisely so no future site can omit the field, and this test
// is what fails if the field is dropped from the helper.
func TestDriveConfigBodyAlwaysSetsWriteback(t *testing.T) {
	tests := []struct {
		name       string
		driveID    string
		path       string
		root       bool
		readOnly   bool
		wantConfig map[string]any
	}{
		{
			name: "image-boot rootfs (where a failed /dev/vdb mount lands PGDATA)",
			// The rationale this case pins: PGDATA is not only ever on the data drive.
			driveID: "rootfs", path: "/srv/jail/rootfs.ext4", root: true, readOnly: false,
			wantConfig: map[string]any{
				"drive_id": "rootfs", "path_on_host": "/srv/jail/rootfs.ext4",
				"is_root_device": true, "is_read_only": false, "cache_type": "Writeback",
			},
		},
		{
			name:    "persistent data volume",
			driveID: "data", path: "/srv/volumes/v1.ext4", root: false, readOnly: false,
			wantConfig: map[string]any{
				"drive_id": "data", "path_on_host": "/srv/volumes/v1.ext4",
				"is_root_device": false, "is_read_only": false, "cache_type": "Writeback",
			},
		},
		{
			name:    "read-only shared warm rootfs",
			driveID: "rootfs", path: "/srv/rootfs/python-warm.ext4", root: true, readOnly: true,
			wantConfig: map[string]any{
				"drive_id": "rootfs", "path_on_host": "/srv/rootfs/python-warm.ext4",
				"is_root_device": true, "is_read_only": true, "cache_type": "Writeback",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := driveConfigBody(tt.driveID, tt.path, tt.root, tt.readOnly)
			if !reflect.DeepEqual(got, tt.wantConfig) {
				t.Fatalf("driveConfigBody:\n got %#v\nwant %#v", got, tt.wantConfig)
			}
			if got["cache_type"] != "Writeback" {
				t.Fatalf("cache_type = %v — a drive without Writeback silently drops every guest fsync", got["cache_type"])
			}
		})
	}
}
