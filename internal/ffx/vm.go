package ffx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"starstack/internal/vm"
)

// VMInventory is what a provider machine admits about its hypervisor. It is
// measured, not configured: the broker only offers the panel to sellers whose
// agent actually found qemu and an accelerator it can start.
type VMInventory struct {
	Driver  string `json:"driver"`
	Version string `json:"version"`
	Accel   string `json:"accel"`
	KVM     bool   `json:"kvm"`
	Bridge  string `json:"bridge,omitempty"`
	Uplink  string `json:"uplink,omitempty"`
	MaxVMs  int    `json:"max_vms"`
}

// GlobalIPv6s lists the interfaces' internet-reachable IPv6 addresses. ULA,
// link-local and temporary/loopback addresses are excluded because a buyer on
// the public internet cannot route to them.
func GlobalIPv6s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if addr.Is4() || !vm.PublicIPv6(addr) {
				continue
			}
			s := addr.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// DetectVM probes the hypervisor an agent was asked to expose. It returns an
// error instead of an empty capability so the operator knows why the console
// will not let them publish an offer.
func DetectVM(ctx context.Context, qemu, qemuImg, bridge, uplink string, maxVMs int) (*VMInventory, error) {
	bin, err := exec.LookPath(qemu)
	if err != nil {
		return nil, fmt.Errorf("找不到 %s：%w", qemu, err)
	}
	if _, err := exec.LookPath(qemuImg); err != nil {
		return nil, fmt.Errorf("找不到 %s：%w", qemuImg, err)
	}
	c2, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c2, bin, "-version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s -version 失败：%v", qemu, err)
	}
	version := firstLine(out)
	if i := strings.Index(version, "version "); i >= 0 {
		version = version[i+len("version "):]
	}
	if i := strings.IndexByte(version, '('); i > 0 {
		version = strings.TrimSpace(version[:i])
	}

	accel := detectAccel(ctx, bin)
	kvm := runtime.GOOS == "linux" && accel == "kvm"
	if maxVMs <= 0 {
		maxVMs = 2
	}
	return &VMInventory{
		Driver: "qemu", Version: version, Accel: accel, KVM: kvm,
		Bridge: strings.TrimSpace(bridge), Uplink: strings.TrimSpace(uplink),
		MaxVMs: maxVMs,
	}, nil
}

// detectAccel asks qemu itself which accelerators it was built with. Guessing
// from GOOS alone would advertise KVM on machines where /dev/kvm is absent.
func detectAccel(ctx context.Context, bin string) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(c, bin, "-accel", "help").CombinedOutput()
	text := strings.ToLower(string(out))
	order := []struct{ name, tag string }{
		{"kvm", "kvm"}, {"whpx", "whpx"}, {"hvf", "hvf"}, {"tcg", "tcg"},
	}
	for _, a := range order {
		if strings.Contains(text, a.tag) {
			// qemu's help lists "tcg" everywhere; only return an advertised
			// hardware accelerator, otherwise fall back to TCG explicitly.
			if a.name == "tcg" {
				continue
			}
			if a.name == "kvm" {
				if st, err := os.Stat("/dev/kvm"); err != nil || st.IsDir() {
					continue
				}
			}
			return a.name
		}
	}
	return "tcg"
}
