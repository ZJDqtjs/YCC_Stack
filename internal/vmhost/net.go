package vmhost

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"starstack/internal/vm"
)

// setupRouted attaches a tap to the provider's bridge and routes the guest's
// /128 across it. When the provider's uplink has the guest prefix on-link, the
// host also acts as an NDP proxy: the upstream switch/router sees the host
// answer for the guest address, and the kernel routes the packets to the tap.
func (h *Host) setupRouted(ctx context.Context, plan *vm.Plan) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("routed IPv6 模式只支持 Linux；这台机器请用 slirp 模式")
	}
	if h.Bridge == "" {
		return "", fmt.Errorf("routed IPv6 模式需要 Agent 以 --vm-bridge <网桥> 启动")
	}
	tap := tapName(plan.InstanceID)
	if err := h.ip(ctx, "tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		return "", fmt.Errorf("创建 tap %s：%w", tap, err)
	}
	if err := h.ip(ctx, "link", "set", "dev", tap, "master", h.Bridge); err != nil {
		h.cleanupRouted(plan, tap)
		return "", fmt.Errorf("把 %s 接到网桥 %s：%w", tap, h.Bridge, err)
	}
	if err := h.ip(ctx, "link", "set", "dev", tap, "up"); err != nil {
		h.cleanupRouted(plan, tap)
		return "", fmt.Errorf("启用 %s：%w", tap, err)
	}
	guest := plan.Network.GuestIPv6
	if err := h.ip(ctx, "-6", "route", "replace", guest+"/128", "dev", tap); err != nil {
		h.cleanupRouted(plan, tap)
		return "", fmt.Errorf("添加 %s 的路由：%w", guest, err)
	}
	if h.Uplink != "" {
		if err := enableProxyNDP(h.Uplink); err != nil {
			h.cleanupRouted(plan, tap)
			return "", fmt.Errorf("在 %s 上启用 NDP 代理：%w", h.Uplink, err)
		}
		if err := h.ip(ctx, "-6", "neigh", "replace", "proxy", guest, "dev", h.Uplink); err != nil {
			h.cleanupRouted(plan, tap)
			return "", fmt.Errorf("添加 NDP 代理：%w", err)
		}
	}
	return tap, nil
}

func (h *Host) cleanupRouted(plan *vm.Plan, tap string) {
	if plan != nil && plan.Network.GuestIPv6 != "" && h.Uplink != "" {
		h.ip(context.Background(), "-6", "neigh", "del", "proxy", plan.Network.GuestIPv6, "dev", h.Uplink)
	}
	if plan != nil && plan.Network.GuestIPv6 != "" {
		h.ip(context.Background(), "-6", "route", "del", plan.Network.GuestIPv6+"/128", "dev", tap)
	}
	h.ip(context.Background(), "link", "del", tap)
}

// cleanupStateNet uses the saved tap name and plan, so a stop after an agent
// restart can still remove routes left by the previous process.
func (h *Host) cleanupStateNet(st *State) {
	if st == nil || st.Tap == "" {
		return
	}
	h.cleanupRouted(st.Plan, st.Tap)
}

func (h *Host) ip(ctx context.Context, args ...string) error {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "ip", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func enableProxyNDP(uplink string) error {
	if !safeIface(uplink) {
		return fmt.Errorf("接口名 %q 不合法", uplink)
	}
	p := "/proc/sys/net/ipv6/conf/" + uplink + "/proxy_ndp"
	if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
		return err
	}
	return nil
}

func safeIface(s string) bool {
	if s == "" || len(s) > 15 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func tapName(id string) string {
	tail := id
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return "ss" + tail
}
