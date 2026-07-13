package firecracker

import "testing"

func TestImageBootArgs(t *testing.T) {
	got := imageBootArgs("10.201.0.6", "10.201.0.5")
	want := "console=ttyS0 reboot=k panic=1 pci=off init=/sentiae/init ip=10.201.0.6::10.201.0.5:255.255.255.252::eth0:off"
	if got != want {
		t.Errorf("imageBootArgs =\n %q\nwant\n %q", got, want)
	}
}

func TestDeriveNet(t *testing.T) {
	tests := []struct {
		n                int
		tap, host, guest string
	}{
		{1, "img1", "10.201.0.5", "10.201.0.6"},       // base = 4
		{2, "img2", "10.201.0.9", "10.201.0.10"},      // base = 8
		{63, "img63", "10.201.0.253", "10.201.0.254"}, // base = 252
		{64, "img64", "10.201.1.1", "10.201.1.2"},     // base = 256 → octet3 rolls
	}
	for _, tt := range tests {
		nw := deriveNet(tt.n)
		if nw.tapName != tt.tap || nw.hostIP != tt.host || nw.guestIP != tt.guest {
			t.Errorf("deriveNet(%d) = {tap:%s host:%s guest:%s}, want {tap:%s host:%s guest:%s}",
				tt.n, nw.tapName, nw.hostIP, nw.guestIP, tt.tap, tt.host, tt.guest)
		}
	}
}

func TestDeriveNetUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	for n := 1; n <= imgMaxIndex; n++ {
		nw := deriveNet(n)
		if seen[nw.guestIP] {
			t.Fatalf("duplicate guest IP %s at index %d", nw.guestIP, n)
		}
		seen[nw.guestIP] = true
	}
}

func TestParseExitCode(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"0\n", 0},
		{"42", 42},
		{" 7 \n", 7},
		{"-1", -1},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		if got := parseExitCode(tt.in); got != tt.want {
			t.Errorf("parseExitCode(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeResources(t *testing.T) {
	if v, m := normalizeResources(0, 0); v != 1 || m != 512 {
		t.Errorf("defaults = (%d,%d), want (1,512)", v, m)
	}
	if v, m := normalizeResources(4, 2048); v != 4 || m != 2048 {
		t.Errorf("passthrough = (%d,%d), want (4,2048)", v, m)
	}
}

func TestAllocIndex(t *testing.T) {
	// rt#8 retired per-VM host-port DNAT: only the /30 network index is allocated.
	b := NewImageBooter(nil, "10.0.0.1")
	b.Seed([]int{1})

	n, err := b.allocIndex()
	if err != nil || n != 2 {
		t.Fatalf("allocIndex = %d,%v; want 2 (1 seeded used)", n, err)
	}
	b.freeIndex(2)
	if n, _ := b.allocIndex(); n != 2 {
		t.Fatalf("allocIndex after free = %d, want 2", n)
	}
}
