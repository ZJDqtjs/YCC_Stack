package hub

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"starstack/internal/auth"
	"starstack/internal/store"
)

// The one-click path for a provider. A visitor who wants to lend their machine
// currently has to be handed a shared agent token, invent a node id, install a
// binary and type a pairing code — four steps where the console should offer
// one button. A launch ticket collapses them: a logged-in user clicks, downloads
// a script, double-clicks it, and the machine arrives already bound to their
// account with no secret to copy around.
//
// The ticket is deliberately weaker than the shared agent token: it authorises
// exactly one node id, belongs to one account, expires in minutes and is spent
// the moment the agent registers. Afterwards the machine re-joins with the
// per-node credential it cached on disk, so the script can be double-clicked
// again tomorrow without any code at all.

// Installer runs (winget/apt) can take longer than a quick binary download, so
// the ticket lives for half an hour. It still authorises exactly one node id
// and is spent by the first accepted hello.
const launchTTL = 30 * time.Minute

type launchTicket struct {
	Code     string    `json:"-"`
	OwnerID  string    `json:"-"`
	NodeID   string    `json:"-"`
	Name     string    `json:"-"`
	Price    float64   `json:"-"`
	Slots    int       `json:"-"`
	OS       string    `json:"-"` // windows | linux | darwin
	Arch     string    `json:"-"`
	ExpireAt time.Time `json:"-"`
	// VM turns the same agent into a cloud-host provider. VMMax is how many
	// tenants it may run at once; bridge/uplink are the Linux routed-mode knobs.
	VM       bool   `json:"-"`
	VMMax    int    `json:"-"`
	VMBridge string `json:"-"`
	VMUplink string `json:"-"`
	// AllowBuild writes --allow-build into the generated launcher when the
	// requesting account is on the build allowlist: the provider still has to
	// double-click that script, so the consent stays explicit on the machine.
	AllowBuild bool `json:"-"`
}

const maxLaunchVMs = 8

var ifaceNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

var launchOS = map[string]string{
	"windows": "starstack-worker-windows-%s.exe",
	"linux":   "starstack-worker-linux-%s",
	"darwin":  "starstack-worker-darwin-%s",
}

var launchArch = map[string]bool{"amd64": true, "arm64": true}

// nameRuneOK is the whitelist behind mintLaunch's name sanitising: letters,
// digits, spaces and _ - survive, shell and batch metacharacters do not.
func nameRuneOK(r rune) bool {
	switch r {
	case '_', '-':
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r)
}

// nodeBinName is what the launcher downloads for a given platform.
func nodeBinName(goos, arch string) string {
	t, ok := launchOS[goos]
	if !ok {
		return ""
	}
	return fmt.Sprintf(t, arch)
}

// nodeBinReady reports whether the launcher for a platform can actually be
// served. Used by the console to decide whether to show the button at all.
func (s *Server) nodeBinReady(goos, arch string) bool {
	if s.nodeBinDir == "" {
		return false
	}
	bin := nodeBinName(goos, arch)
	return bin != "" && fileExists(filepath.Join(s.nodeBinDir, bin))
}

func (s *Server) mintLaunch(u *store.User, req launchReq) (*launchTicket, string) {
	goos := strings.ToLower(strings.TrimSpace(req.OS))
	if _, ok := launchOS[goos]; !ok {
		goos = "windows"
	}
	arch := strings.ToLower(strings.TrimSpace(req.Arch))
	if !launchArch[arch] {
		arch = "amd64"
	}
	if req.Price < 0.05 || req.Price > 1000 {
		return nil, "单价需在 0.05 ~ 1000 之间"
	}
	if req.Slots < 1 || req.Slots > s.policy.MaxSlots {
		return nil, "并发槽数需在 1 ~ " + strconv.Itoa(s.policy.MaxSlots) + " 之间"
	}
	vmMax, bridge, uplink := 0, "", ""
	if req.VM {
		vmMax = req.VMMax
		if vmMax <= 0 {
			vmMax = 2
		}
		if vmMax < 1 || vmMax > maxLaunchVMs {
			return nil, "云主机并发数需在 1 ~ " + strconv.Itoa(maxLaunchVMs) + " 之间"
		}
		bridge = strings.TrimSpace(req.VMBridge)
		uplink = strings.TrimSpace(req.VMUplink)
		if bridge != "" || uplink != "" {
			if goos != "linux" {
				return nil, "网桥/上联网卡只用于 Linux routed 模式；Windows/macOS 请留空使用 slirp"
			}
			if bridge == "" {
				return nil, "填了上联网卡就必须同时填网桥"
			}
			if !ifaceNameRE.MatchString(bridge) {
				return nil, "网桥名不合法"
			}
			if uplink != "" && !ifaceNameRE.MatchString(uplink) {
				return nil, "上联网卡名不合法"
			}
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "网页节点"
	}
	// The name is written into a script that runs on the provider's machine, so
	// it is a shell metacharacter carrier: `"`, `&`, `|`, backticks and `$()`
	// would be pasted straight into a .cmd line. Letters, digits, spaces and
	// underscore/hyphen survive; everything else becomes a space. UTF-8 runes are
	// counted (and sliced) so a Chinese name is neither mojibake nor over-long.
	var b strings.Builder
	n := 0
	for _, r := range name {
		if nameRuneOK(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
		n++
		if n == 40 {
			break
		}
	}
	name = strings.Join(strings.Fields(b.String()), " ")
	if name == "" {
		name = "网页节点"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// tickets are memory-only like the pairing codes: a broker restart simply
	// retires whatever nobody ran yet
	for c, t := range s.launches {
		if t.ExpireAt.Before(time.Now()) {
			delete(s.launches, c)
		}
	}
	t := &launchTicket{
		Code: auth.RandomHex(8), OwnerID: u.ID, NodeID: "web-" + auth.RandomHex(3),
		Name: name, Price: req.Price, Slots: req.Slots,
		OS: goos, Arch: arch, ExpireAt: time.Now().Add(launchTTL),
		VM: req.VM, VMMax: vmMax, VMBridge: bridge, VMUplink: uplink,
	}
	s.launches[t.Code] = t
	log.Printf("launch ticket %s minted for node %s by user %s (%s/%s, %.2f 点/秒, %d 槽, vm=%v max=%d bridge=%q)",
		t.Code, t.NodeID, u.Name, t.OS, t.Arch, t.Price, t.Slots, t.VM, t.VMMax, t.VMBridge)
	return t, ""
}

// consumeLaunch spends a ticket. The node id has to match: a leaked code cannot
// be used to enrol a different machine or to squat an id that is already running.
func (s *Server) consumeLaunch(code, wantID string) *launchTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.launches[code]
	if t == nil || t.ExpireAt.Before(time.Now()) {
		return nil
	}
	if t.NodeID != wantID {
		log.Printf("launch code %s presented by %q, minted for %q: refused", code, wantID, t.NodeID)
		return nil
	}
	delete(s.launches, code)
	return t
}

// admitAgent decides whether this hello may register a node. It returns a spent
// ticket when the machine should be bound to an account on arrival, or a reason
// to refuse. The order matters: a code that has already been spent falls
// through, because re-double-clicking the launcher is the normal thing to do and
// the cached per-node credential below is what makes it work.
func (s *Server) admitAgent(r *http.Request, id string) (*launchTicket, string) {
	if code := strings.TrimSpace(r.Header.Get("X-Launch-Code")); code != "" {
		if t := s.consumeLaunch(code, id); t != nil {
			return t, ""
		}
	}
	if s.policy.AgentToken == "" {
		return nil, "" // demo profile: a local broker with nothing to protect
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("X-Agent-Token"))),
		[]byte(s.policy.AgentToken)) == 1 {
		return nil, ""
	}
	if s.knownNode(r, id) {
		return nil, ""
	}
	return nil, "接入需要启动令牌或网页里生成的启动码"
}

// knownNode accepts a returning agent that proves it has been registered before
// with the credential the broker derived for its node id. Without this every
// restart of every provider machine would need a fresh code typed by hand.
func (s *Server) knownNode(r *http.Request, id string) bool {
	if id == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Node-Secret"))
	if got == "" || !constantEqual(got, s.nodeSecret(id)) {
		return false
	}
	owner, err := s.st.NodeOwner(id)
	return err == nil && owner != ""
}

// ---- http ----

type launchReq struct {
	Price    float64 `json:"price"`
	Slots    int     `json:"slots"`
	Name     string  `json:"name"`
	OS       string  `json:"os"`
	Arch     string  `json:"arch"`
	VM       bool    `json:"vm"`
	VMMax    int     `json:"vm_max"`
	VMBridge string  `json:"vm_bridge"`
	VMUplink string  `json:"vm_uplink"`
}

func (s *Server) handleLaunchMint(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, jsonErr("只支持 POST"), http.StatusMethodNotAllowed)
		return
	}
	var req launchReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, jsonErr("请求格式错误"), http.StatusBadRequest)
		return
	}
	t, why := s.mintLaunch(u, req)
	if t == nil {
		http.Error(w, jsonErr(why), http.StatusBadRequest)
		return
	}
	t.AllowBuild = s.policy.buildAllowed(u.Name)
	bin := nodeBinName(t.OS, t.Arch)
	ready := s.nodeBinReady(t.OS, t.Arch)
	if !ready {
		// minting is still fine (the operator may mount the binaries later), but
		// saying so here beats handing out a script that fails on download
		log.Printf("launch: %s 不在 %s，脚本会在下载那步失败", bin, s.nodeBinDir)
	}
	writeJSON(w, map[string]any{
		"node_id": t.NodeID, "code": t.Code,
		"expires_in_sec": int(launchTTL / time.Second),
		"script":         "/launch/" + t.Code + "/" + t.OS + "/" + t.Arch,
		"bundle":         "/launch/" + t.Code + "/" + t.OS + "/" + t.Arch + "?format=bundle",
		"worker":         "/dl/" + bin, "ready": ready,
		"vm": t.VM, "vm_max": t.VMMax,
	})
}

var launchPath = regexp.MustCompile(`^/launch/([0-9a-f]{16,64})/(windows|linux|darwin)/(amd64|arm64)$`)

// handleLaunchScript renders the launcher for a live ticket. The code in the URL
// is the whole capability — it is single-use, minutes-old and bound to one node
// id, and the browser cannot attach a session cookie to a download triggered by
// navigation anyway.
func (s *Server) handleLaunchScript(w http.ResponseWriter, r *http.Request) {
	m := launchPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	t := s.launches[m[1]]
	if t != nil && t.ExpireAt.Before(time.Now()) {
		delete(s.launches, m[1])
		t = nil
	}
	s.mu.Unlock()
	if t == nil {
		http.Error(w, "启动码已过期或已用过，回网页里再点一次", http.StatusNotFound)
		return
	}
	if t.OS != m[2] || t.Arch != m[3] {
		// the ticket was minted for a different platform than the button clicked
		http.Error(w, "这份脚本和生成的平台不一致", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("format") == "bundle" {
		s.serveLaunchBundle(w, t)
		return
	}
	body, ctype, fname := renderLauncher(t, s.baseURL, nodeBinName(t.OS, t.Arch))
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, body)
}

// serveLaunchBundle packages the worker and its config into one zip. The
// provider extracts it and double-clicks the exe; the bat launcher stays as the
// fallback for machines that need QEMU or a different work directory.
func (s *Server) serveLaunchBundle(w http.ResponseWriter, t *launchTicket) {
	if s.nodeBinDir == "" {
		http.Error(w, jsonErr("这台 Broker 没有配置 --node-bin-dir，无法打包节点程序"), http.StatusNotFound)
		return
	}
	bin := nodeBinName(t.OS, t.Arch)
	f, err := os.Open(filepath.Join(s.nodeBinDir, bin))
	if err != nil {
		http.Error(w, jsonErr("缺少节点程序 "+bin), http.StatusNotFound)
		return
	}
	defer f.Close()

	cfg, err := json.MarshalIndent(map[string]any{
		"server":         strings.TrimRight(s.baseURL, "/"),
		"id":             t.NodeID,
		"name":           t.Name,
		"price":          t.Price,
		"slots":          t.Slots,
		"launch":         t.Code,
		"allow_build":    t.AllowBuild,
		"auto_provision": t.AllowBuild,
	}, "", "  ")
	if err != nil {
		http.Error(w, jsonErr("配置生成失败"), http.StatusInternalServerError)
		return
	}

	bundleName := "starstack-node"
	if t.OS == "windows" {
		bundleName += ".exe"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="starstack-node-`+t.NodeID+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")

	zw := zip.NewWriter(w)
	hdr := &zip.FileHeader{Name: bundleName, Method: zip.Deflate}
	hdr.SetMode(0o755)
	hdr.Modified = time.Now()
	dst, err := zw.CreateHeader(hdr)
	if err == nil {
		_, err = io.Copy(dst, f)
	}
	if err == nil && t.OS != "windows" {
		// Double-click does not exist on Linux/macOS: one command after unzip.
		sh := "#!/bin/sh\ncd \"$(dirname \"$0\")\" || exit 1\nchmod +x ./starstack-node 2>/dev/null || true\nexec ./starstack-node\n"
		shHdr := &zip.FileHeader{Name: "start.sh", Method: zip.Deflate}
		shHdr.SetMode(0o755)
		shHdr.Modified = time.Now()
		var out io.Writer
		out, err = zw.CreateHeader(shHdr)
		if err == nil {
			_, err = io.WriteString(out, sh)
		}
	}
	if err == nil {
		var out io.Writer
		out, err = zw.Create("starstack-node.json")
		if err == nil {
			_, err = out.Write(cfg)
		}
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Printf("launch bundle %s: %v", t.NodeID, err)
	}
}

func renderLauncher(t *launchTicket, baseURL, bin string) (string, string, string) {
	server := strings.TrimRight(baseURL, "/")
	price := strconv.FormatFloat(t.Price, 'f', 2, 64)
	slots := strconv.Itoa(t.Slots)

	vmArgsWin, vmArgsSh, qemuWin, qemuSh := "", "", "", ""
	buildArgs := ""
	if t.AllowBuild {
		// Compiling usually needs a toolchain the provider may not have. The
		// provider already opted into running buyer commands, so provisioning the
		// missing toolchain is the same consent and avoids a failed first run.
		buildArgs = " --allow-build --auto-provision"
	}
	if t.VM {
		vmArgsWin = " --vm --vm-max " + strconv.Itoa(t.VMMax) +
			` --qemu "%QEMU_BIN%" --qemu-img qemu-img`
		vmArgsSh = " --vm --vm-max " + strconv.Itoa(t.VMMax) +
			` --qemu "$QEMU_BIN" --qemu-img qemu-img`
		if t.VMBridge != "" {
			vmArgsSh += ` --vm-bridge "` + t.VMBridge + `"`
			if t.VMUplink != "" {
				vmArgsSh += ` --vm-uplink "` + t.VMUplink + `"`
			}
		}
		qemuWin = `
set QEMU_BIN=qemu-system-x86_64
where %QEMU_BIN% >nul 2>nul
if errorlevel 1 if exist "%ProgramFiles%\qemu\qemu-system-x86_64.exe" set "PATH=%ProgramFiles%\qemu;%PATH%"
where %QEMU_BIN% >nul 2>nul
if errorlevel 1 (
  echo.
  echo [!] QEMU is not installed. StarStack needs it to host virtual machines.
  echo     winget install --id SoftwareFreedomConservancy.QEMU -e
  echo     or download: https://www.qemu.org/download/#windows
  where winget >nul 2>nul
  if not errorlevel 1 (
    choice /C YN /M "Install QEMU with winget now"
    if errorlevel 2 goto starstack_qemu_missing
    winget install --id SoftwareFreedomConservancy.QEMU -e --accept-package-agreements --accept-source-agreements
    if exist "%ProgramFiles%\qemu\qemu-system-x86_64.exe" set "PATH=%ProgramFiles%\qemu;%PATH%"
    where %QEMU_BIN% >nul 2>nul
    if errorlevel 1 goto starstack_qemu_missing
  ) else (
    goto starstack_qemu_missing
  )
)
where qemu-img >nul 2>nul
if errorlevel 1 (
  echo [!] qemu-img was not found. Reinstall QEMU and make sure its folder is in PATH.
  goto starstack_qemu_missing
)
echo QEMU is ready:
%QEMU_BIN% -version | findstr /i "version"
echo [!] Cloud hosts need inbound IPv6 TCP 5900-5999 and 20000-29999 (plus explicit ports).
net session >nul 2>nul
if errorlevel 1 (
  echo     Not running as administrator; add the firewall rule manually if needed:
  echo     netsh advfirewall firewall add rule name="StarStack VM" dir=in action=allow protocol=TCP localport=5900-5999,20000-29999
) else (
  choice /C YN /M "Add Windows Firewall rules for StarStack VM ports now"
  if errorlevel 2 goto starstack_fw_ok
  netsh advfirewall firewall delete rule name="StarStack VM" >nul 2>nul
  netsh advfirewall firewall add rule name="StarStack VM" dir=in action=allow protocol=TCP localport=5900-5999,20000-29999
)
:starstack_fw_ok
echo     Enable "Windows Hypervisor Platform" in Optional Features and reboot for WHPX acceleration.
goto starstack_qemu_ok
:starstack_qemu_missing
echo.
echo QEMU is required for cloud hosts. Install it and run this script again.
pause
exit /b 1
:starstack_qemu_ok
`
		qemuSh = `
QEMU_BIN=qemu-system-x86_64
if ! command -v "$QEMU_BIN" >/dev/null 2>&1; then
  echo "[!] 云主机需要 QEMU，但找不到 $QEMU_BIN。"
  INSTALL=""
  if command -v apt-get >/dev/null 2>&1; then
    INSTALL="sudo apt-get update && sudo apt-get install -y qemu-system-x86 qemu-utils"
  elif command -v dnf >/dev/null 2>&1; then
    INSTALL="sudo dnf install -y @virtualization"
  elif command -v pacman >/dev/null 2>&1; then
    INSTALL="sudo pacman -S --noconfirm qemu-full"
  elif command -v brew >/dev/null 2>&1; then
    INSTALL="brew install qemu"
  fi
  if [ -n "$INSTALL" ]; then
    echo "可执行：$INSTALL"
    if [ -t 0 ]; then
      printf "现在安装 QEMU？[y/N] "; read -r ans
      case "$ans" in
        y|Y) sh -c "$INSTALL" ;;
        *) exit 1 ;;
      esac
    else
      echo "非交互环境，请手动执行上面的命令后重试。"; exit 1
    fi
  else
    echo "请从 https://www.qemu.org/download/ 安装 QEMU 后重试。"; exit 1
  fi
fi
command -v "$QEMU_BIN" >/dev/null 2>&1 || { echo "[!] QEMU 仍未安装，退出。"; exit 1; }
command -v qemu-img >/dev/null 2>&1 || { echo "[!] 找不到 qemu-img，请安装 qemu-utils/qemu-img。"; exit 1; }
case "$(uname -s)" in
  Linux)
    if [ -e /dev/kvm ] && [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
      echo "KVM 可用"
    else
      echo "[!] /dev/kvm 不可用：虚拟机将以 TCG 软件模拟运行（明显变慢）。"
      echo "    sudo usermod -aG kvm $USER 后重新登录，或在 BIOS 里开启虚拟化。"
    fi
    ;;
  Darwin) echo "macOS 会使用 HVF；x86_64 模拟时可能是 TCG，速度取决于机型。" ;;
esac
echo "云主机需要放行 IPv6 入站 TCP 5900-5999（控制台）、20000-29999（SSH/自动端口），"
echo "以及在配置里显式填写的其它宿主机端口。"
`
		if t.VMBridge != "" {
			qemuSh += `
echo "[!] routed 模式：需要 Linux 网桥 ` + t.VMBridge + ` 已存在，并以 root/CAP_NET_ADMIN 运行。"
[ -d "/sys/class/net/` + t.VMBridge + `" ] || echo "[!] 找不到网桥 ` + t.VMBridge + `"
if [ -n "` + t.VMUplink + `" ]; then
  [ -d "/sys/class/net/` + t.VMUplink + `" ] || echo "[!] 找不到上联网卡 ` + t.VMUplink + `"
fi
if [ "$(id -u)" -ne 0 ]; then echo "[!] 当前不是 root，routed 会启动失败；slirp 模式不需要 root。"; fi
`
		}
	}
	if t.OS == "windows" {
		// ASCII only, on purpose: cmd.exe reads a .cmd with the machine's OEM
		// codepage, so UTF-8 Chinese in this file reaches the parser as mojibake on
		// a GBK Windows and breaks the lines around it (measured here: an echo of
		// the price turned into a "'池：0.65' 不是内部或外部命令"). The provider types
		// the name in the console, and the broker takes it from the ticket at bind
		// time — over JSON there is no codepage to trip on.
		return `@echo off
setlocal enableextensions enabledelayedexpansion
rem StarStack compute node launcher, generated by the web console.
rem The launch code below is single use and valid for ` + strconv.Itoa(int(launchTTL.Minutes())) + ` minutes.
rem Closing this window stops providing compute. Double-click it again to rejoin.
set SERVER=` + server + `
set NODE_ID=` + t.NodeID + `
set LAUNCH=` + t.Code + `
set DIR=%LOCALAPPDATA%\starstack-node
set BIN=%DIR%\starstack-worker.exe
if not exist "%DIR%" mkdir "%DIR%"
set WANT=
for /f "usebackq delims=" %%H in (` + "`curl -fsSL --retry 3 \"%SERVER%/dl/" + bin + ".sha256\"`" + `) do set WANT=%%H
if not defined WANT (
  echo Cannot fetch the worker checksum; refusing to run an unverified program.
  pause
  exit /b 1
)
set GOT=
if exist "%BIN%" (
  for /f "usebackq delims=" %%H in (` + "`powershell -NoProfile -Command \"(Get-FileHash -Algorithm SHA256 -LiteralPath '%BIN%').Hash.ToLower()\"`" + `) do set GOT=%%H
)
if /i not "!GOT!"=="%WANT%" (
  del /f /q "%BIN%" >nul 2>nul
  if exist "%BIN%" (
    echo The running node is still holding the old program. Close its window and run this again.
    pause
    exit /b 1
  )
  echo Downloading the node program, about 10 MB; this can take a few minutes...
  curl -fsSL --retry 3 -o "%BIN%.part" "%SERVER%/dl/` + bin + `"
  if errorlevel 1 (
    echo Download failed. Check this machine's network, or ask the operator to drop
    echo ` + bin + ` into %DIR% and double-click again.
    pause
    exit /b 1
  )
  set GOT=
  for /f "usebackq delims=" %%H in (` + "`powershell -NoProfile -Command \"(Get-FileHash -Algorithm SHA256 -LiteralPath '%BIN%.part').Hash.ToLower()\"`" + `) do set GOT=%%H
  if /i not "!GOT!"=="%WANT%" (
    del /f /q "%BIN%.part" >nul 2>nul
    echo Checksum mismatch. Refusing to run the downloaded program.
    pause
    exit /b 1
  )
  move /y "%BIN%.part" "%BIN%" >nul
)` + qemuWin + `
echo Joining the compute pool as %NODE_ID%: ` + price + ` credits per video-second, ` + slots + ` slots.
echo Keep this window open while you are sharing. Status and earnings: the web console.
"%BIN%" --server "%SERVER%" --id "%NODE_ID%" --launch "%LAUNCH%" ^
  --price ` + price + ` --slots ` + slots + ` --work "%DIR%\cache"` + vmArgsWin + buildArgs + `
echo.
echo Node stopped. Double-click this file to join again - the credential in
echo %DIR%\cache\node-%NODE_ID%.key means no launch code is needed any more.
pause
`, "application/octet-stream", "starstack-node-" + t.NodeID + ".cmd"
	}
	sh := `#!/bin/sh
# 星栈算力 节点启动器（网页生成，启动码一次性、` + strconv.Itoa(int(launchTTL.Minutes())) + ` 分钟内有效）
# Ctrl-C 就是停止提供算力。下次想再提供，重新执行本文件即可。
SERVER=` + server + `
NODE_ID=` + t.NodeID + `
LAUNCH=` + t.Code + `
DIR="$HOME/.starstack-node"
BIN="$DIR/starstack-worker"
mkdir -p "$DIR"
WANT=$(curl -fsSL --retry 3 "$SERVER/dl/` + bin + `.sha256") || { echo "拿不到校验和，拒绝运行未验证的程序"; exit 1; }
WANT=$(printf '%s' "$WANT" | tr -d '\r\n')
[ -n "$WANT" ] || { echo "校验和为空，拒绝运行未验证的程序"; exit 1; }
hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else return 1; fi
}
GOT=""
[ -x "$BIN" ] && GOT=$(hash_file "$BIN" || true)
if [ "$GOT" != "$WANT" ]; then
  rm -f "$BIN"
  echo "正在下载节点程序（服务器出口慢时这一步要等几分钟）..."
  curl -fsSL --retry 3 -o "$BIN.tmp" "$SERVER/dl/` + bin + `" || { echo "下载失败"; exit 1; }
  GOT=$(hash_file "$BIN.tmp" || true)
  [ "$GOT" = "$WANT" ] || { rm -f "$BIN.tmp"; echo "校验失败，拒绝运行下载的程序"; exit 1; }
  chmod 0755 "$BIN.tmp" && mv -f "$BIN.tmp" "$BIN"
fi` + qemuSh + `
echo "以 $NODE_ID 加入算力池：` + price + ` 点/视频秒，` + slots + ` 个并发槽"
exec "$BIN" --server "$SERVER" --id "$NODE_ID" --launch "$LAUNCH" \
  --name "` + t.Name + `" --price ` + price + ` --slots ` + slots + ` --work "$DIR/cache"` + vmArgsSh + buildArgs + `
`
	return sh, "application/x-sh", "starstack-node-" + t.NodeID + ".sh"
}

// handleNodeBin serves the worker binary a launcher downloads. Names come from a
// fixed pattern, so a request can only ever select one of the six platform builds
// the operator installed — never an arbitrary path.
func (s *Server) handleNodeBin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/dl/")
	wantHash := strings.HasSuffix(name, ".sha256")
	if wantHash {
		name = strings.TrimSuffix(name, ".sha256")
	}
	if !regexp.MustCompile(`^starstack-worker-(windows|linux|darwin)-(amd64|arm64)(\.exe)?$`).MatchString(name) {
		http.NotFound(w, r)
		return
	}
	if s.nodeBinDir == "" {
		http.Error(w, jsonErr("这台 Broker 没有配置 --node-bin-dir，无法提供节点程序下载"), http.StatusNotFound)
		return
	}
	p := filepath.Join(s.nodeBinDir, name)
	if !fileExists(p) {
		http.Error(w, jsonErr("缺少 "+name+"：请运维把对应平台的 worker 二进制放进 --node-bin-dir"),
			http.StatusNotFound)
		return
	}
	if wantHash {
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, jsonErr("读取节点程序失败"), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			http.Error(w, jsonErr("计算校验和失败"), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "%x\n", h.Sum(nil))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, p)
}

type pauseReq struct {
	Paused bool `json:"paused"`
}

// handleNodePause is the switch in the page. The agent is not told to stop —
// it keeps heartbeating and keeps whatever it is running — the scheduler just
// stops promising it new slices, which is what "我先别接活" actually means.
func (s *Server) handleNodePause(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, jsonErr("只支持 PUT"), http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/pause")
	owner, err := s.st.NodeOwner(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, jsonErr("读取失败"), http.StatusInternalServerError)
		return
	}
	if owner != u.ID {
		http.Error(w, jsonErr("这台机器不属于你"), http.StatusForbidden)
		return
	}
	var req pauseReq
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, jsonErr("请求格式错误"), http.StatusBadRequest)
		return
	}
	if err := s.st.SetNodePaused(id, req.Paused); err != nil {
		http.Error(w, jsonErr("更新失败"), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	if wd := s.workers[id]; wd != nil {
		wd.Paused = req.Paused
	}
	s.mu.Unlock()
	log.Printf("node %s %s by %s", id, map[bool]string{
		true:  "暂停派活",
		false: "恢复派活",
	}[req.Paused], u.Name)
	writeJSON(w, map[string]any{"ok": true, "id": id, "paused": req.Paused})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// SetNodeBinDir points the launcher download at the directory holding the
// per-platform worker binaries. Empty means the one-click path is off.
func (s *Server) SetNodeBinDir(dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		log.Printf("node-bin-dir: %v", err)
		return
	}
	s.nodeBinDir = abs
}
