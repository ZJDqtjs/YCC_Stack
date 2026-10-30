package vm

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

func TestGlobalIPv6Only(t *testing.T) {
	good := []string{"2001:db8:1::10", "[2001:db8:1::10]", "2001:db8:1::10%eth0"}
	for _, s := range good {
		if _, err := ParseGlobalIPv6(s); err != nil {
			t.Errorf("%q should be accepted: %v", s, err)
		}
	}
	bad := []string{"", "::1", "fe80::1", "fd00::1", "ff02::1", "192.0.2.1", "not-an-ip"}
	for _, s := range bad {
		if _, err := ParseGlobalIPv6(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestAllocGuestIPv6(t *testing.T) {
	prefix := "2001:db8:42::/64"
	gw := "2001:db8:42::1"
	used := map[string]bool{}
	a1, bits, err := AllocGuestIPv6(prefix, "vm-one", gw, used)
	if err != nil {
		t.Fatal(err)
	}
	if bits != 64 {
		t.Fatalf("prefix len = %d, want 64", bits)
	}
	p, _ := netip.ParsePrefix(prefix)
	addr, _ := netip.ParseAddr(a1)
	if !p.Contains(addr) {
		t.Fatalf("%s not inside %s", a1, prefix)
	}
	if a1 == gw {
		t.Fatal("allocated the gateway address")
	}
	used[a1] = true
	a2, _, err := AllocGuestIPv6(prefix, "vm-two", gw, used)
	if err != nil {
		t.Fatal(err)
	}
	if a2 == a1 || used[a2] {
		t.Fatalf("allocator handed out %s twice", a2)
	}
	if !strings.HasPrefix(a2, "2001:db8:42:") {
		t.Fatalf("%s left the prefix", a2)
	}
}

func TestAllocHostPorts(t *testing.T) {
	used := map[int]bool{2200: true}
	out, err := AllocHostPorts([]PortMap{
		{Proto: "tcp", HostPort: 0, GuestPort: 80},
		{Proto: "tcp", HostPort: 2201, GuestPort: 443},
	}, used)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 { // two requested + implicit SSH
		t.Fatalf("got %d maps, want 3: %+v", len(out), out)
	}
	seenSSH := false
	seenHost := map[int]bool{}
	for _, p := range out {
		if p.HostPort == 2200 || seenHost[p.HostPort] {
			t.Fatalf("port %d reused: %+v", p.HostPort, out)
		}
		seenHost[p.HostPort] = true
		if p.GuestPort == 22 && p.Proto == "tcp" {
			seenSSH = true
		}
	}
	if !seenSSH {
		t.Fatal("implicit SSH mapping missing")
	}
	if _, err := AllocHostPorts([]PortMap{{Proto: "tcp", HostPort: 2201, GuestPort: 80}}, used); err == nil {
		t.Fatal("explicit conflict must be refused")
	}
}

func TestBillableCapsAtHold(t *testing.T) {
	// 2 points/hour, 0.5 point hold => 15 minutes of runtime
	secs, spent := Billable(2, 1000, 1000+3600, 0.5)
	if secs != 900 || spent != 0.5 {
		t.Fatalf("secs=%v spent=%v, want 900 / 0.5", secs, spent)
	}
	secs, spent = Billable(2, 1000, 1300, 10)
	if secs != 300 || spent != round4(2*300/3600.0) {
		t.Fatalf("short run: secs=%v spent=%v", secs, spent)
	}
	if _, spent := Billable(2, 0, 5000, 10); spent != 0 {
		t.Fatalf("unstarted instance billed %v", spent)
	}
}

func TestOfferNormalize(t *testing.T) {
	o := Offer{
		Name: "  test vm ", ImageURL: "https://example.com/ubuntu.qcow2",
		ImageFormat: "QCOW2", CPUs: 2, MemMB: 2048, DiskGB: 20,
		NetMode: "slirp", HostIPv6: "2001:db8::10", PriceHour: 0.5,
		Ports: []PortMap{{GuestPort: 80}},
	}
	if err := o.Normalize(); err != nil {
		t.Fatal(err)
	}
	if o.ImageFormat != "qcow2" || o.MaxHours != 24 || o.Ports[0].Proto != "tcp" {
		t.Fatalf("defaults not applied: %+v", o)
	}
	o.ImageURL = "file:///etc/passwd"
	if err := o.Normalize(); err == nil {
		t.Fatal("file:// image URL must be refused")
	}
	o.ImageURL = "https://example.com/a.qcow2"
	o.HostIPv6 = "fd00::1"
	if err := o.Normalize(); err == nil {
		t.Fatal("ULA host address must be refused")
	}
}

func TestBuildSeedISO(t *testing.T) {
	files := map[string][]byte{
		"meta-data":      []byte("instance-id: vm-1\nlocal-hostname: vm-1\n"),
		"user-data":      []byte("#cloud-config\nhostname: vm-1\n"),
		"network-config": []byte("version: 2\n"),
	}
	iso, err := BuildSeedISO(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(iso)%isoSector != 0 {
		t.Fatalf("image size %d is not sector aligned", len(iso))
	}
	if iso[16*isoSector] != 1 || string(iso[16*isoSector+1:16*isoSector+6]) != "CD001" {
		t.Fatal("primary volume descriptor missing")
	}
	if label := bytesTrim(iso[16*isoSector+40 : 16*isoSector+72]); label != "cidata" {
		t.Fatalf("volume label = %q, want cidata", label)
	}
	if iso[17*isoSector] != 2 || string(iso[17*isoSector+88:17*isoSector+91]) != "%/@" {
		t.Fatal("Joliet supplementary descriptor missing")
	}
	if iso[18*isoSector] != 255 {
		t.Fatal("volume descriptor terminator missing")
	}
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		u := utf16be(name)
		if !bytes.Contains(iso, u) {
			t.Fatalf("Joliet name %q not found in image", name)
		}
	}
	// deterministic output: same files, same bytes
	again, _ := BuildSeedISO(files)
	if !bytes.Equal(iso, again) {
		t.Fatal("seed image is not reproducible")
	}
}

func bytesTrim(b []byte) string {
	return strings.TrimRight(string(b), " ")
}

func utf16be(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}
