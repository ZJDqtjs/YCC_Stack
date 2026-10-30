// Package vm holds the contract between the broker, the provider agent and the
// buyer for the IPv6 virtual-machine market. It is deliberately free of
// database and HTTP imports: the same types are written to SQLite, put on the
// agent's long-poll inbox and rendered by the console.
package vm

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	NetRouted = "routed" // Linux tap attached to a bridge; guest holds a real GUA
	NetSlirp  = "slirp"  // QEMU user-mode NAT; guest reached through host IPv6 port forwards

	StatusQueued       = "queued"
	StatusProvisioning = "provisioning"
	StatusRunning      = "running"
	StatusUnreachable  = "unreachable"
	StatusStopping     = "stopping"
	StatusStopped      = "stopped"
	StatusFailed       = "failed"

	OpStart = "start"
	OpStop  = "stop"
	OpSync  = "sync"
)

// PortMap is one published service. HostPort 0 asks the broker to allocate a
// free port on the provider's machine; GuestPort is the service inside the VM.
type PortMap struct {
	Proto     string `json:"proto"`
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
}

// Offer is a seller's published VM shape. It snapshots everything a buyer is
// promised; changing the machine later must not rewrite an existing instance.
type Offer struct {
	ID          string    `json:"id"`
	NodeID      string    `json:"node_id"`
	OwnerID     string    `json:"owner_id,omitempty"`
	Name        string    `json:"name"`
	ImageURL    string    `json:"image_url"`
	ImageSHA256 string    `json:"image_sha256,omitempty"`
	ImageFormat string    `json:"image_format"`
	OS          string    `json:"os"`
	CPUs        int       `json:"cpus"`
	MemMB       int       `json:"mem_mb"`
	DiskGB      int       `json:"disk_gb"`
	NetMode     string    `json:"net_mode"`
	IPv6Prefix  string    `json:"ipv6_prefix,omitempty"`
	IPv6Gateway string    `json:"ipv6_gateway,omitempty"`
	HostIPv6    string    `json:"host_ipv6"`
	Ports       []PortMap `json:"ports"`
	PriceHour   float64   `json:"price_hour"`
	MaxHours    int       `json:"max_hours"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   int64     `json:"created_at"`

	// AllocPorts/Capacity are filled by the broker from the live node, never
	// trusted from the request body.
	MaxCPUs    int   `json:"-"`
	MaxMemMB   int   `json:"-"`
	MaxVMs     int   `json:"-"`
	DiskFreeMB int64 `json:"-"`
}

// Instance is one buyer's VM, including the provider-side allocation that must
// survive a broker restart.
type Instance struct {
	ID           string     `json:"id"`
	OfferID      string     `json:"offer_id"`
	NodeID       string     `json:"node_id"`
	BuyerID      string     `json:"buyer_id,omitempty"`
	SellerID     string     `json:"seller_id,omitempty"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	CPUs         int        `json:"cpus"`
	MemMB        int        `json:"mem_mb"`
	DiskGB       int        `json:"disk_gb"`
	ImageURL     string     `json:"image_url"`
	ImageSHA256  string     `json:"image_sha256,omitempty"`
	ImageFormat  string     `json:"image_format"`
	OS           string     `json:"os"`
	NetMode      string     `json:"net_mode"`
	GuestIPv6    string     `json:"guest_ipv6,omitempty"`
	PrefixLen    int        `json:"prefix_len,omitempty"`
	Gateway      string     `json:"gateway,omitempty"`
	HostIPv6     string     `json:"host_ipv6"`
	Ports        []PortMap  `json:"ports,omitempty"`
	SSHUser      string     `json:"ssh_user"`
	SSHPort      int        `json:"ssh_port"`
	ConsolePort  int        `json:"console_port,omitempty"`
	SSHPublicKey string     `json:"ssh_public_key,omitempty"`
	PriceHour    float64    `json:"price_hour"`
	Hold         float64    `json:"hold"`
	Spent        float64    `json:"spent"`
	MaxHours     int        `json:"max_hours"`
	Progress     float64    `json:"progress"`
	Endpoints    *Endpoints `json:"endpoints,omitempty"`
	Note         string     `json:"note,omitempty"`
	Err          string     `json:"err,omitempty"`
	CreatedAt    int64      `json:"created_at"`
	StartedAt    int64      `json:"started_at,omitempty"`
	StoppedAt    int64      `json:"stopped_at,omitempty"`
	LastSeen     int64      `json:"last_seen,omitempty"`
	ExpiresAt    int64      `json:"expires_at,omitempty"`
	// StoppingAt is when shutdown was ordered. Heartbeats keep LastSeen fresh,
	// so that field cannot bound how long a stop is allowed to take.
	StoppingAt int64 `json:"stopping_at,omitempty"`

	// Secrets stay server-side. The buyer sees the SSH private key and root
	// password once, in the order response; the agent receives the root password
	// inside the signed command, not through any buyer-visible endpoint.
	RootPasswordEnc string `json:"-"`
	VNCPasswordEnc  string `json:"-"`
	Cleanup         bool   `json:"-"`
	DispatchedAt    int64  `json:"-"`
}

// Endpoints is what a buyer actually types into ssh / an RDP client.
type Endpoints struct {
	IPv6     string    `json:"ipv6"`
	SSH      string    `json:"ssh"`
	SSHUser  string    `json:"ssh_user,omitempty"`
	VNC      string    `json:"vnc,omitempty"`
	Forwards []Forward `json:"forwards,omitempty"`
	Direct   bool      `json:"direct,omitempty"`
}

type Forward struct {
	Proto     string `json:"proto"`
	Listen    string `json:"listen"`
	GuestPort int    `json:"guest_port"`
}

// Network is the half of a plan the guest has to agree with the host about.
type Network struct {
	Mode        string    `json:"mode"`
	GuestIPv6   string    `json:"guest_ipv6,omitempty"`
	PrefixLen   int       `json:"prefix_len,omitempty"`
	Gateway     string    `json:"gateway,omitempty"`
	HostIPv6    string    `json:"host_ipv6"`
	Forwards    []PortMap `json:"forwards,omitempty"`
	ConsolePort int       `json:"console_port,omitempty"`
	VNCPassword string    `json:"vnc_password,omitempty"`
}

// Plan is the provisioning command an agent executes. It is self-contained so
// a broker restart can re-send it without the agent holding any earlier state.
type Plan struct {
	InstanceID       string   `json:"instance_id"`
	Name             string   `json:"name"`
	CPUs             int      `json:"cpus"`
	MemMB            int      `json:"mem_mb"`
	DiskGB           int      `json:"disk_gb"`
	ImageURL         string   `json:"image_url"`
	ImageSHA256      string   `json:"image_sha256,omitempty"`
	ImageFormat      string   `json:"image_format"`
	OS               string   `json:"os"`
	SSHUser          string   `json:"ssh_user"`
	SSHAuthorizedKey string   `json:"ssh_authorized_key"`
	RootPassword     string   `json:"root_password"`
	Hostname         string   `json:"hostname"`
	DNS              []string `json:"dns,omitempty"`
	Network          Network  `json:"network"`
}

type Command struct {
	Op       string `json:"op"`
	Instance string `json:"instance"`
	Plan     *Plan  `json:"plan,omitempty"`
}

type Report struct {
	Worker    string     `json:"worker"`
	Instance  string     `json:"instance"`
	State     string     `json:"state"`
	Pct       float64    `json:"pct,omitempty"`
	Msg       string     `json:"msg,omitempty"`
	Err       string     `json:"err,omitempty"`
	Endpoints *Endpoints `json:"endpoints,omitempty"`
	// Cleaned says the agent already removed the instance's disk/state. If it is
	// false, the broker keeps the instance eligible for a retried stop.
	Cleaned bool `json:"cleaned,omitempty"`
}

// Active reports whether this instance still occupies broker capacity. A
// stopped or failed instance does not, even though its row is kept forever.
func (i *Instance) Active() bool {
	switch i.Status {
	case StatusQueued, StatusProvisioning, StatusRunning, StatusUnreachable, StatusStopping:
		return true
	}
	return false
}

// Normalize validates and canonicalises an offer. It is the only place that
// decides what a seller may advertise, because every field here is later handed
// to a buyer or to a command line on the provider's machine.
func (o *Offer) Normalize() error {
	o.Name = CleanLabel(o.Name)
	if o.Name == "" {
		o.Name = "云主机"
	}
	if n := []rune(o.Name); len(n) > 40 {
		o.Name = string(n[:40])
	}
	o.ImageURL = strings.TrimSpace(o.ImageURL)
	if len(o.ImageURL) > 2048 {
		return errors.New("镜像地址过长")
	}
	u, err := url.Parse(o.ImageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("镜像地址必须是 http(s) URL")
	}
	if u.User != nil {
		return errors.New("镜像地址不能内嵌用户名/口令")
	}
	o.ImageFormat = strings.ToLower(strings.TrimSpace(o.ImageFormat))
	if o.ImageFormat == "" {
		o.ImageFormat = "qcow2"
	}
	switch o.ImageFormat {
	case "qcow2", "raw", "iso":
	default:
		return errors.New("镜像格式只支持 qcow2 / raw / iso")
	}
	o.ImageSHA256 = strings.ToLower(strings.TrimSpace(o.ImageSHA256))
	if o.ImageSHA256 != "" && !isHex64(o.ImageSHA256) {
		return errors.New("镜像 SHA-256 必须是 64 位十六进制")
	}
	o.OS = CleanLabel(o.OS)
	if o.OS == "" {
		o.OS = "linux"
	}
	if len([]rune(o.OS)) > 40 {
		return errors.New("系统名称过长")
	}
	if o.CPUs < 1 || o.CPUs > 256 {
		return errors.New("vCPU 数量需在 1 ~ 256 之间")
	}
	if o.MemMB < 256 || o.MemMB > 1<<20 {
		return errors.New("内存需在 256 MiB ~ 1 TiB 之间")
	}
	if o.DiskGB < 1 || o.DiskGB > 16384 {
		return errors.New("磁盘需在 1 ~ 16384 GiB 之间")
	}
	o.NetMode = strings.ToLower(strings.TrimSpace(o.NetMode))
	if o.NetMode == "" {
		o.NetMode = NetSlirp
	}
	if o.NetMode != NetSlirp && o.NetMode != NetRouted {
		return errors.New("网络模式只支持 slirp / routed")
	}
	host, err := ParseGlobalIPv6(o.HostIPv6)
	if err != nil {
		return fmt.Errorf("卖方 IPv6 地址无效：%w", err)
	}
	o.HostIPv6 = host.String()
	o.IPv6Prefix = strings.TrimSpace(o.IPv6Prefix)
	o.IPv6Gateway = strings.TrimSpace(o.IPv6Gateway)
	if o.NetMode == NetRouted {
		p, err := ParseGlobalPrefix(o.IPv6Prefix)
		if err != nil {
			return fmt.Errorf("routed 模式需要可路由的 IPv6 前缀：%w", err)
		}
		o.IPv6Prefix = p.String()
		gw, err := ParseGlobalIPv6(o.IPv6Gateway)
		if err != nil {
			return fmt.Errorf("routed 模式需要 IPv6 网关：%w", err)
		}
		o.IPv6Gateway = gw.String()
		if p.Bits() < 48 || p.Bits() > 64 {
			return errors.New("IPv6 前缀长度需在 /48 ~ /64 之间")
		}
	}
	ports, err := NormalizePorts(o.Ports)
	if err != nil {
		return err
	}
	o.Ports = ports
	if o.PriceHour < 0.01 || o.PriceHour > 10000 {
		return errors.New("每小时单价需在 0.01 ~ 10000 算力点之间")
	}
	if o.MaxHours == 0 {
		o.MaxHours = 24
	}
	if o.MaxHours < 1 || o.MaxHours > 720 {
		return errors.New("最长租期需在 1 ~ 720 小时之间")
	}
	return nil
}

// CleanLabel strips control characters and collapses whitespace from a label
// that ends up in logs, cloud-init and the console.
func CleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// NormalizePorts lowercases protocols, rejects duplicate host ports and caps
// the list so one offer cannot turn a host firewall into a sieve.
func NormalizePorts(in []PortMap) ([]PortMap, error) {
	if len(in) > 8 {
		return nil, errors.New("端口映射最多 8 条")
	}
	seen := map[string]bool{}
	out := make([]PortMap, 0, len(in))
	for _, p := range in {
		p.Proto = strings.ToLower(strings.TrimSpace(p.Proto))
		if p.Proto == "" {
			p.Proto = "tcp"
		}
		if p.Proto != "tcp" && p.Proto != "udp" {
			return nil, errors.New("端口协议只支持 tcp / udp")
		}
		if p.GuestPort < 1 || p.GuestPort > 65535 {
			return nil, errors.New("虚拟机内端口需在 1 ~ 65535 之间")
		}
		if p.HostPort < 0 || p.HostPort > 65535 {
			return nil, errors.New("宿主机端口需在 0 ~ 65535 之间")
		}
		if p.HostPort != 0 && (p.HostPort < 1024 || (p.HostPort >= 5900 && p.HostPort <= 5999)) {
			return nil, fmt.Errorf("宿主机端口 %d 不可用：请用 1024 以上且避开 5900-5999（VNC 保留）", p.HostPort)
		}
		if p.HostPort != 0 {
			k := p.Proto + "/" + strconv.Itoa(p.HostPort)
			if seen[k] {
				return nil, fmt.Errorf("宿主机端口 %d/%s 重复", p.HostPort, p.Proto)
			}
			seen[k] = true
		}
		out = append(out, p)
	}
	return out, nil
}

// ParseGlobalIPv6 accepts only addresses that can actually be reached from the
// internet: no loopback, link-local, ULA, multicast or unspecified addresses.
func ParseGlobalIPv6(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is6() || a.Is4In6() {
		return netip.Addr{}, errors.New("不是合法的 IPv6 地址")
	}
	if !PublicIPv6(a) {
		return netip.Addr{}, errors.New("只接受公网可路由的 IPv6 地址（GUA）")
	}
	return a, nil
}

func PublicIPv6(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() &&
		!a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsMulticast()
}

func ParseGlobalPrefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
		return netip.Prefix{}, errors.New("不是合法的 IPv6 前缀")
	}
	if !PublicIPv6(p.Addr()) {
		return netip.Prefix{}, errors.New("只接受公网可路由的 IPv6 前缀")
	}
	return p.Masked(), nil
}

// AllocGuestIPv6 picks an unused address inside a routed prefix. The seed is
// the instance id, so re-running allocation for the same instance after a
// broker restart tends to land on the same address; collision with a live
// instance still walks forward instead of returning a duplicate.
func AllocGuestIPv6(prefix, seed, gateway string, used map[string]bool) (string, int, error) {
	p, err := ParseGlobalPrefix(prefix)
	if err != nil {
		return "", 0, err
	}
	bits := p.Bits()
	if bits < 48 || bits > 64 {
		return "", 0, errors.New("IPv6 前缀长度需在 /48 ~ /64 之间")
	}
	base := p.Addr().As16()
	gw := strings.TrimSpace(gateway)
	for i := 0; i < 65536; i++ {
		h := sha256.Sum256([]byte(seed + ":" + strconv.Itoa(i)))
		var out [16]byte
		copy(out[:], base[:])
		hostBits := 128 - bits
		for j := 0; j < hostBits; j++ {
			if h[j/8]&(1<<(7-uint(j%8))) != 0 {
				bit := bits + j
				out[bit/8] |= 1 << (7 - uint(bit%8))
			}
		}
		// all-zero is the subnet-router anycast address in a /64, all-ones is
		// broadcast in the old site-local sense; neither belongs to a guest.
		allZero, allOne := true, true
		for j := 0; j < hostBits; j++ {
			bit := bits + j
			v := out[bit/8] & (1 << (7 - uint(bit%8)))
			if v != 0 {
				allZero = false
			} else {
				allOne = false
			}
		}
		if allZero || allOne {
			continue
		}
		addr := netip.AddrFrom16(out).String()
		if addr == gw || used[addr] {
			continue
		}
		return addr, bits, nil
	}
	return "", 0, errors.New("该 IPv6 前缀内没有可用地址")
}

// AllocHostPorts resolves every HostPort==0 against the node's used set and
// guarantees SSH (guest 22/tcp) exists, because a VM nobody can log into is
// not a rented server. used is updated in place.
func AllocHostPorts(in []PortMap, used map[int]bool) ([]PortMap, error) {
	out := make([]PortMap, 0, len(in)+1)
	hasSSH := false
	for _, p := range in {
		if p.Proto == "tcp" && p.GuestPort == 22 {
			hasSSH = true
		}
	}
	if !hasSSH {
		in = append(append([]PortMap(nil), in...), PortMap{Proto: "tcp", GuestPort: 22})
	} else {
		in = append([]PortMap(nil), in...)
	}
	next := 20000
	for _, p := range in {
		if p.HostPort != 0 {
			if used[p.HostPort] {
				return nil, fmt.Errorf("宿主机端口 %d 已被同一台机器上的其它云主机占用", p.HostPort)
			}
			used[p.HostPort] = true
			out = append(out, p)
			continue
		}
		for next <= 29999 && used[next] {
			next++
		}
		if next > 29999 {
			return nil, errors.New("没有可用的端口转发端口（20000-29999 已用尽）")
		}
		p.HostPort = next
		used[next] = true
		next++
		out = append(out, p)
	}
	return out, nil
}

// AllocConsolePort finds a free VNC port in 5900-5999 and marks it used.
func AllocConsolePort(used map[int]bool) (int, error) {
	for p := 5900; p <= 5999; p++ {
		if !used[p] {
			used[p] = true
			return p, nil
		}
	}
	return 0, errors.New("没有可用的控制台端口（5900-5999 已用尽）")
}

// Billable charges by the second against the prepaid hold. The hold is also the
// hard runtime ceiling: an instance can never cost more than the buyer
// authorised in advance, so a forgotten VM cannot drain an account.
func Billable(priceHour float64, startedAt, endAt int64, hold float64) (secs, spent float64) {
	if priceHour <= 0 || startedAt <= 0 || endAt <= startedAt {
		return 0, 0
	}
	secs = float64(endAt - startedAt)
	maxSecs := hold / priceHour * 3600
	if maxSecs > 0 && secs > maxSecs {
		secs = maxSecs
	}
	spent = round4(priceHour * secs / 3600)
	if spent > hold {
		spent = hold
	}
	return secs, spent
}

// ValidateSSHKey is a shape check, not a cryptographic one: cloud-init is the
// one that will parse it, but refusing obvious junk here keeps a failed boot
// from being blamed on the provider.
var sshKeyRE = regexp.MustCompile(`^(ssh-rsa|ssh-ed25519|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)[ \t]+[A-Za-z0-9+/]+={0,2}([ \t]+[^\r\n]*)?$`)

func ValidateSSHKey(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) > 4096 {
		return errors.New("SSH 公钥过长")
	}
	if !sshKeyRE.MatchString(s) {
		return errors.New("SSH 公钥格式不正确（需要 OpenSSH authorized_keys 一行）")
	}
	return nil
}

// NewSecret returns an alphanumeric secret safe to embed in cloud-init YAML
// without quoting surprises.
func NewSecret(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a condition this process can reason about
		panic("vm: crypto/rand unavailable: " + err.Error())
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// Snapshot is the buyer/seller-facing view. Secrets are absent by construction;
// the order response is the only place that carries them.
func (i *Instance) Snapshot() map[string]any {
	return map[string]any{
		"id": i.ID, "offer_id": i.OfferID, "node_id": i.NodeID, "name": i.Name,
		"status": i.Status, "cpus": i.CPUs, "mem_mb": i.MemMB, "disk_gb": i.DiskGB,
		"image_format": i.ImageFormat, "os": i.OS, "net_mode": i.NetMode,
		"guest_ipv6": i.GuestIPv6, "prefix_len": i.PrefixLen, "gateway": i.Gateway,
		"host_ipv6": i.HostIPv6, "ports": i.Ports,
		"ssh_user": i.SSHUser, "ssh_port": i.SSHPort, "console_port": i.ConsolePort,
		"price_hour": i.PriceHour, "hold": i.Hold, "spent": i.Spent,
		"max_hours": i.MaxHours, "progress": i.Progress, "endpoints": i.Endpoints,
		"note": i.Note, "err": i.Err, "created_at": i.CreatedAt,
		"started_at": i.StartedAt, "stopped_at": i.StoppedAt,
		"last_seen": i.LastSeen, "expires_at": i.ExpiresAt,
		"stopping_at": i.StoppingAt,
	}
}

func round4(f float64) float64 {
	return math.Round(f*10000) / 10000
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// SortedPorts is for deterministic command lines and tests.
func SortedPorts(in []PortMap) []PortMap {
	out := append([]PortMap(nil), in...)
	sort.Slice(out, func(a, b int) bool {
		if out[a].Proto != out[b].Proto {
			return out[a].Proto < out[b].Proto
		}
		return out[a].HostPort < out[b].HostPort
	})
	return out
}
