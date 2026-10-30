package vmhost

import (
	"errors"
	"fmt"
	"strings"

	"starstack/internal/vm"
)

// BuildSeed renders the NoCloud seed the guest's cloud-init reads on first
// boot. This is where "automatically install the image" actually happens for a
// cloud image: the OS is already in the image, and this seed creates the
// account, installs the buyer's SSH key, sets the console password and pins the
// IPv6 address. ISO installs still need the buyer to click through the
// installer over VNC, but the same seed will configure the result on first boot.
func BuildSeed(plan *vm.Plan) ([]byte, error) {
	if plan == nil {
		return nil, errors.New("empty plan")
	}
	host := sanitizeHostname(plan.Hostname)
	if host == "" {
		host = "starstack"
	}
	user := plan.SSHUser
	if user == "" {
		user = "starstack"
	}
	if !safeYAMLToken(user) {
		return nil, errors.New("SSH 用户名包含不安全字符")
	}
	if !safeYAMLToken(plan.RootPassword) {
		return nil, errors.New("控制台口令包含不安全字符")
	}
	key := strings.TrimSpace(plan.SSHAuthorizedKey)
	if strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("SSH 公钥不能包含换行")
	}

	userData := fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true
preserve_hostname: false
users:
  - default
  - name: %s
    gecos: Starstack Cloud
    groups: [sudo, adm]
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: false
%s
chpasswd:
  expire: false
  list: |
    root:%s
    %s:%s
ssh_pwauth: false
disable_root: true
package_update: false
`, host, user, authorizedKeysBlock(plan), plan.RootPassword, user, plan.RootPassword)

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", plan.InstanceID, host)
	network := networkConfig(plan)

	return vm.BuildSeedISO(map[string][]byte{
		"user-data":      []byte(userData),
		"meta-data":      []byte(metaData),
		"network-config": []byte(network),
	})
}

func authorizedKeysBlock(plan *vm.Plan) string {
	key := strings.TrimSpace(plan.SSHAuthorizedKey)
	if key == "" {
		return "    ssh_authorized_keys: []"
	}
	// Keep only "type base64": the optional comment is free text from the
	// buyer and could otherwise turn the YAML list item into a mapping.
	if f := strings.Fields(key); len(f) >= 2 {
		key = f[0] + " " + f[1]
	}
	return "    ssh_authorized_keys:\n      - " + key
}

func networkConfig(plan *vm.Plan) string {
	if plan.Network.Mode != vm.NetRouted {
		return `version: 2
ethernets:
  eth0:
    match:
      name: "e*"
    set-name: eth0
    dhcp4: true
    dhcp6: false
`
	}
	dns := make([]string, 0, len(plan.DNS))
	for _, d := range plan.DNS {
		if !strings.ContainsAny(d, "\r\n\" ") {
			dns = append(dns, `"`+d+`"`)
		}
	}
	dnsBlock := "    nameservers:\n      addresses: [" + strings.Join(dns, ", ") + "]\n"
	if len(dns) == 0 {
		dnsBlock = ""
	}
	return fmt.Sprintf(`version: 2
ethernets:
  eth0:
    match:
      name: "e*"
    set-name: eth0
    dhcp4: false
    dhcp6: false
    addresses: ["%s/%d"]
    routes:
      - to: "::/0"
        via: "%s"
        on-link: true
%s`, plan.Network.GuestIPv6, plan.Network.PrefixLen, plan.Network.Gateway, dnsBlock)
}

func sanitizeHostname(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-")
	if len(h) > 63 {
		h = h[:63]
	}
	return h
}

func safeYAMLToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e || r == '\'' || r == '"' || r == ':' || r == '#' ||
			r == '\n' || r == '\r' || r == '\\' {
			return false
		}
	}
	return true
}
