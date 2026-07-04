//go:build unit

package firecracker

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestWarmBootArgs(t *testing.T) {
	got := warmBootArgs(warmGuestIP, warmHostIP, warmNetmask)
	want := "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/warm-init ip=172.30.0.2::172.30.0.1:255.255.255.252::eth0:off"
	if got != want {
		t.Fatalf("warmBootArgs:\n got %q\nwant %q", got, want)
	}
}

func TestWarmBootSourceBody(t *testing.T) {
	body := warmBootSourceBody("/var/lib/firecracker/kernel/vmlinux")
	wantArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/warm-init ip=172.30.0.2::172.30.0.1:255.255.255.252::eth0:off"
	if body["kernel_image_path"] != "/var/lib/firecracker/kernel/vmlinux" {
		t.Fatalf("kernel_image_path = %v", body["kernel_image_path"])
	}
	if body["boot_args"] != wantArgs {
		t.Fatalf("boot_args = %v", body["boot_args"])
	}
}

func TestWarmMachineConfigBody(t *testing.T) {
	body := warmMachineConfigBody()
	if body["vcpu_count"] != 1 {
		t.Fatalf("vcpu_count = %v, want 1", body["vcpu_count"])
	}
	if body["mem_size_mib"] != 256 {
		t.Fatalf("mem_size_mib = %v, want 256", body["mem_size_mib"])
	}
}

func TestWarmRootfsDriveBody(t *testing.T) {
	body := warmRootfsDriveBody("/var/lib/firecracker/rootfs/python-warm.ext4")
	want := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   "/var/lib/firecracker/rootfs/python-warm.ext4",
		"is_root_device": true,
		"is_read_only":   false,
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("warmRootfsDriveBody:\n got %#v\nwant %#v", body, want)
	}
}

func TestWarmNetIfaceBody(t *testing.T) {
	body := warmNetIfaceBody("tap-deadbeef")
	want := map[string]any{
		"iface_id":      "eth0",
		"host_dev_name": "tap-deadbeef",
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("warmNetIfaceBody:\n got %#v\nwant %#v", body, want)
	}
}

func TestWarmEntropyBody(t *testing.T) {
	body := warmEntropyBody()
	// virtio-rng needs no required fields; the minimal valid /entropy body is
	// an empty object so Firecracker uses the device with no rate limiter.
	if len(body) != 0 {
		t.Fatalf("warmEntropyBody() = %#v, want empty map", body)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal warmEntropyBody: %v", err)
	}
	if string(b) != `{}` {
		t.Fatalf("warmEntropyBody JSON = %s, want {}", string(b))
	}
}

func TestVMPausedBody(t *testing.T) {
	body := vmPausedBody()
	b, _ := json.Marshal(body)
	if string(b) != `{"state":"Paused"}` {
		t.Fatalf("vmPausedBody JSON = %s, want {\"state\":\"Paused\"}", string(b))
	}
}

func TestSnapshotCreateBody(t *testing.T) {
	body := snapshotCreateBody("/snap/x.state", "/snap/x.mem")
	want := map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": "/snap/x.state",
		"mem_file_path": "/snap/x.mem",
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("snapshotCreateBody:\n got %#v\nwant %#v", body, want)
	}
}

func TestSnapshotLoadBody(t *testing.T) {
	body := snapshotLoadBody("/snap/x.state", "/snap/x.mem")

	if body["snapshot_path"] != "/snap/x.state" {
		t.Fatalf("snapshot_path = %v", body["snapshot_path"])
	}
	if body["resume_vm"] != true {
		t.Fatalf("resume_vm = %v, want true", body["resume_vm"])
	}
	// mem_backend File (mmap = CoW). Must NOT use mem_file_path.
	if _, hasMemFilePath := body["mem_file_path"]; hasMemFilePath {
		t.Fatalf("snapshotLoadBody must use mem_backend, not mem_file_path")
	}
	mb, ok := body["mem_backend"].(map[string]any)
	if !ok {
		t.Fatalf("mem_backend missing or wrong type: %#v", body["mem_backend"])
	}
	if mb["backend_type"] != "File" {
		t.Fatalf("backend_type = %v, want File", mb["backend_type"])
	}
	if mb["backend_path"] != "/snap/x.mem" {
		t.Fatalf("backend_path = %v", mb["backend_path"])
	}
}

func TestCloneNaming(t *testing.T) {
	tests := []struct {
		n           int
		namespace   string
		vethHost    string
		vethGuest   string
		hostVethIP  string
		nsVethIP    string
		hostReachIP string
	}{
		{1, "fc-clone1", "vh1", "vg1", "10.200.1.1", "10.200.1.2", "10.200.1.2"},
		{42, "fc-clone42", "vh42", "vg42", "10.200.42.1", "10.200.42.2", "10.200.42.2"},
		{254, "fc-clone254", "vh254", "vg254", "10.200.254.1", "10.200.254.2", "10.200.254.2"},
	}
	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			d := cloneNaming(tt.n)
			if d.namespace != tt.namespace {
				t.Errorf("namespace = %q, want %q", d.namespace, tt.namespace)
			}
			if d.vethHost != tt.vethHost {
				t.Errorf("vethHost = %q, want %q", d.vethHost, tt.vethHost)
			}
			if d.vethGuest != tt.vethGuest {
				t.Errorf("vethGuest = %q, want %q", d.vethGuest, tt.vethGuest)
			}
			if d.hostVethIP != tt.hostVethIP {
				t.Errorf("hostVethIP = %q, want %q", d.hostVethIP, tt.hostVethIP)
			}
			if d.nsVethIP != tt.nsVethIP {
				t.Errorf("nsVethIP = %q, want %q", d.nsVethIP, tt.nsVethIP)
			}
			if d.hostReachIP != tt.hostReachIP {
				t.Errorf("hostReachIP = %q, want %q", d.hostReachIP, tt.hostReachIP)
			}
		})
	}
}

func TestWarmVMEndpoint(t *testing.T) {
	w := &WarmVM{GuestIP: "172.30.0.2"}
	if got := w.Endpoint(); got != "172.30.0.2:8000" {
		t.Fatalf("Endpoint() = %q, want 172.30.0.2:8000", got)
	}
}
