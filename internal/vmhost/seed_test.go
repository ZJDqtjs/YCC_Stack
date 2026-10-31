package vmhost

import (
	"bytes"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"starstack/internal/vm"
)

func TestBuildSeedCarriesSSHAndIPv6(t *testing.T) {
	plan := &vm.Plan{
		InstanceID: "vmiabcdef1234", Hostname: "My VM", SSHUser: "starstack",
		RootPassword: "RootPass123", SSHAuthorizedKey: "ssh-rsa AAAAB3NzaC1yc2E test@buyer",
		Network: vm.Network{
			Mode: vm.NetRouted, GuestIPv6: "2001:db8:42::a", PrefixLen: 64,
			Gateway: "2001:db8:42::1", HostIPv6: "2001:db8:1::10", ConsolePort: 5901,
		},
	}
	iso, err := BuildSeed(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config", "hostname: my-vm", "ssh-rsa AAAAB3NzaC1yc2E",
		"root:RootPass123", "2001:db8:42::a/64", `via: "2001:db8:42::1"`,
		"instance-id: vmiabcdef1234",
	} {
		if !bytes.Contains(iso, []byte(want)) {
			t.Errorf("seed does not contain %q", want)
		}
	}
	if bytes.Contains(iso, []byte("test@buyer")) {
		t.Error("SSH key comment should be stripped from cloud-init YAML")
	}

	plan.SSHAuthorizedKey = "ssh-rsa AAAA\nmalicious: true"
	if _, err := BuildSeed(plan); err == nil {
		t.Fatal("newline in SSH key must be refused")
	}
}

func TestQEMUArgsSlirpAndRouted(t *testing.T) {
	h := New(t.TempDir(), "qemu-system-x86_64", "qemu-img", "br0", "eth0", "tcg", 2)
	plan := &vm.Plan{
		InstanceID: "vmialpha", CPUs: 2, MemMB: 2048, DiskGB: 20,
		ImageURL: "https://example.com/a.qcow2", ImageFormat: "qcow2",
		Network: vm.Network{Mode: vm.NetSlirp, HostIPv6: "2001:db8::1", ConsolePort: 5900,
			Forwards: []vm.PortMap{{Proto: "tcp", HostPort: 20000, GuestPort: 22}}},
	}
	args := strings.Join(h.qemuArgs(plan, "/tmp/vmi", "", "", false, 4444), " ")
	for _, want := range []string{
		"hostfwd=tcp:127.0.0.1:20000-:22", "-vnc 127.0.0.1:0,password=on",
		"qmp tcp:127.0.0.1:4444", "seed.iso", "-boot order=c,menu=off",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("slirp args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "tap,id=net0") {
		t.Error("slirp plan must not attach a tap")
	}

	plan.Network = vm.Network{Mode: vm.NetRouted, GuestIPv6: "2001:db8:42::a",
		PrefixLen: 64, Gateway: "2001:db8:42::1", HostIPv6: "2001:db8::1", ConsolePort: 5900}
	args = strings.Join(h.qemuArgs(plan, "/tmp/vmi", "ssdeadbeef", "", false, 4444), " ")
	if !strings.Contains(args, "tap,id=net0,ifname=ssdeadbeef,script=no,downscript=no") {
		t.Fatalf("routed args missing tap:\n%s", args)
	}
	if strings.Contains(args, "hostfwd=") {
		t.Fatal("routed plan must not NAT ports")
	}
}

func TestAccelFailureRecognition(t *testing.T) {
	for _, msg := range []string{
		"WHPX: failed to initialize", "Could not access KVM kernel module",
		"HVF: hypervisor not available", "accelerator initialization failed",
	} {
		if !accelFailure(errors.New(msg)) {
			t.Errorf("%q should be recognized as an accelerator failure", msg)
		}
	}
	if accelFailure(errors.New("could not open disk image: no such file")) {
		t.Fatal("a disk error must not trigger a TCG retry")
	}
	h := New(t.TempDir(), "qemu-system-x86_64", "qemu-img", "", "", "kvm", 2)
	args := strings.Join(h.qemuArgsAccel("kvm", &vm.Plan{InstanceID: "vmi1", Network: vm.Network{ConsolePort: 5900}}, "/tmp/x", "", "", false, 1234), " ")
	if !strings.Contains(args, "-accel kvm") || !strings.Contains(args, "-cpu host") {
		t.Fatalf("kvm args: %s", args)
	}
	args = strings.Join(h.qemuArgsAccel("tcg", &vm.Plan{InstanceID: "vmi1", Network: vm.Network{ConsolePort: 5900}}, "/tmp/x", "", "", false, 1234), " ")
	if !strings.Contains(args, "-accel tcg") || !strings.Contains(args, "-cpu max") {
		t.Fatalf("tcg args: %s", args)
	}
}

func TestForwarderTCPReachesLoopbackTarget(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer target.Close()
	port := target.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				c.Write(buf[:n])
			}(c)
		}
	}()

	plan := &vm.Plan{Network: vm.Network{
		Mode: vm.NetSlirp, HostIPv6: "2001:db8::1",
		Forwards: []vm.PortMap{{Proto: "tcp", HostPort: port, GuestPort: 22}},
	}}
	f, err := startForwarder(plan)
	if err != nil {
		t.Skipf("IPv6 listener unavailable: %v", err)
	}
	defer f.Close()

	c, err := net.DialTimeout("tcp", "[::1]:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := c.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
}
