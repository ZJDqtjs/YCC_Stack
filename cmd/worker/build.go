package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"starstack/internal/hub"
	"starstack/internal/render"
)

// runBuild executes one compile/packaging job: unpack the buyer's source zip,
// run the command with that directory as the working directory, and return the
// requested output tree as result.zip. The provider opted into this with
// --allow-build; the broker additionally restricts who may submit.
func (a *agent) runBuild(c *hub.Claim) {
	spec := c.Spec
	if spec == nil || spec.Kind != render.KindBuild || strings.TrimSpace(spec.Command) == "" {
		a.fail(c, "任务缺少编译命令")
		return
	}
	log.Printf("build job %s: %.200s", c.JobID, spec.Command)

	src, err := a.fetchSource(c)
	if err != nil {
		a.fail(c, "source: "+err.Error())
		return
	}
	root := filepath.Join(a.work, "build-"+c.SegID)
	os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		a.fail(c, err.Error())
		return
	}
	defer os.RemoveAll(root)

	if c.Bundle {
		if err := unzipBundle(src, root); err != nil {
			a.fail(c, "解压源码包: "+err.Error())
			return
		}
		if runtime.GOOS != "windows" {
			// A Linux build script usually arrives through a zip that did not keep
			// the executable bit; make shell scripts runnable without asking the
			// buyer to prefix every command with `sh`.
			filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".sh") {
					os.Chmod(p, 0o755)
				}
				return nil
			})
		}
	} else if err := copyFile(src, filepath.Join(root, "source.zip")); err != nil {
		a.fail(c, err.Error())
		return
	}

	timeout := spec.TimeoutSec
	if timeout < 60 {
		timeout = 1800
	}
	if timeout > 3600 {
		timeout = 3600
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	logPath := filepath.Join(a.work, "build-"+c.SegID+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		a.fail(c, err.Error())
		return
	}
	defer func() {
		logFile.Close()
		os.Remove(logPath)
	}()

	a.post(a.server+"/api/report", map[string]any{
		"worker": a.id, "seg": c.SegID, "state": "running", "pct": 5}, true)

	extraPath, err := a.ensureDotnet(logFile, autoProvisionEnabled)
	if err != nil {
		a.fail(c, "准备 .NET SDK: "+err.Error()+"｜"+tailFile(logPath, 800))
		return
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/C", spec.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", spec.Command)
	}
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = buildEnv(c.JobID, extraPath)

	a.post(a.server+"/api/report", map[string]any{
		"worker": a.id, "seg": c.SegID, "state": "running", "pct": 5}, true)
	stopBeat := a.beatBuild(c.SegID)
	t0 := time.Now()
	runErr := cmd.Run()
	wall := time.Since(t0).Seconds()
	stopBeat()
	logFile.Sync()

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			a.fail(c, fmt.Sprintf("编译超时（%d 秒）｜%s", timeout, tailFile(logPath, 800)))
			return
		}
		a.fail(c, "编译失败: "+runErr.Error()+"｜"+tailFile(logPath, 800))
		return
	}

	outDir := root
	if spec.Output != "" {
		outDir = filepath.Join(root, filepath.FromSlash(spec.Output))
		cleanRoot, cleanOut := filepath.Clean(root), filepath.Clean(outDir)
		if cleanOut != cleanRoot && !strings.HasPrefix(cleanOut, cleanRoot+string(os.PathSeparator)) {
			a.fail(c, "输出目录越界")
			return
		}
		if st, err := os.Stat(outDir); err != nil || st.IsDir() == false {
			a.fail(c, "输出路径不存在或不是目录: "+spec.Output)
			return
		}
	}

	zipPath := filepath.Join(a.work, "result-"+c.SegID+".zip")
	defer os.Remove(zipPath)
	if err := zipTree(outDir, zipPath, logPath); err != nil {
		a.fail(c, "打包产物: "+err.Error())
		return
	}
	a.post(a.server+"/api/report", map[string]any{
		"worker": a.id, "seg": c.SegID, "state": "running", "pct": 90}, true)
	a.finish(c, zipPath, wall, 0)
}

// autoProvisionEnabled mirrors --auto-provision: the provider allows the agent
// to install a missing toolchain instead of failing the buyer's task.
var autoProvisionEnabled bool

// ensureDotnet returns a directory to prepend to PATH when the machine has no
// usable .NET SDK. With auto-provision it downloads the official installer into
// the user's LocalAppData, which needs no administrator rights; without it the
// build fails with one clear line instead of a wall of MSBuild output.
func (a *agent) ensureDotnet(logFile *os.File, allow bool) (string, error) {
	if runtime.GOOS != "windows" {
		return "", nil
	}
	if out, err := exec.Command("dotnet", "--list-sdks").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return "", nil
	}
	local := filepath.Join(os.Getenv("LOCALAPPDATA"), "starstack-sdk")
	localDotnet := filepath.Join(local, "dotnet.exe")
	if out, err := exec.Command(localDotnet, "--list-sdks").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return local, nil
	}
	if !allow {
		return "", fmt.Errorf("这台机器没有 .NET SDK，节点也没有开启 --auto-provision")
	}

	fmt.Fprintf(logFile, "\n[auto-provision] 未找到 .NET SDK，正在安装到 %s\n", local)
	logFile.Sync()
	a.post(a.server+"/api/report", map[string]any{
		"worker": a.id, "seg": "", "state": "idle"}, true)
	ps := `$ProgressPreference='SilentlyContinue'; ` +
		`$u='https://dot.net/v1/dotnet-install.ps1'; ` +
		`$p=Join-Path $env:TEMP 'dotnet-install.ps1'; ` +
		`Invoke-WebRequest -UseBasicParsing -Uri $u -OutFile $p; ` +
		`& $p -Channel 8.0 -InstallDir (Join-Path $env:LOCALAPPDATA 'starstack-sdk')`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("安装 .NET SDK 失败: %w", err)
	}
	if out, err := exec.Command(localDotnet, "--list-sdks").Output(); err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("安装完成但仍找不到 .NET SDK")
	}
	fmt.Fprintf(logFile, "[auto-provision] .NET SDK 已就绪：%s\n", local)
	return local, nil
}

// buildEnv deliberately does not inherit the agent's environment: a buyer
// supplied command must not be able to read the provider's agent token or other
// credentials just by running `env`.
func buildEnv(jobID, extraPath string) []string {
	keep := map[string]bool{
		"PATH": true, "Path": true, "SystemRoot": true, "SYSTEMROOT": true,
		"WINDIR": true, "COMSPEC": true, "PATHEXT": true, "TEMP": true, "TMP": true,
		"USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true, "HOME": true,
		"LOCALAPPDATA": true, "APPDATA": true,
		"PROGRAMFILES": true, "ProgramFiles": true,
		"PROGRAMFILES(X86)": true, "ProgramFiles(x86)": true,
		"ProgramW6432": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		key, val := kv, ""
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key, val = kv[:i], kv[i+1:]
		}
		if keep[key] {
			// The provisioned SDK must win over a runtime-only dotnet earlier in
			// PATH, so rewrite the entry instead of appending a duplicate PATH.
			if extraPath != "" && strings.EqualFold(key, "PATH") {
				kv = key + "=" + extraPath + ";" + val
			}
			out = append(out, kv)
		}
	}
	return append(out, "CI=1", "STARSTACK_BUILD=1", "STARSTACK_JOB="+jobID)
}

// beatBuild keeps the node online and the lease alive while a command that may
// run for half an hour gives no progress of its own.
func (a *agent) beatBuild(segID string) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				a.post(a.server+"/api/report", map[string]any{
					"worker": a.id, "seg": segID, "state": "running", "pct": 50}, true)
			}
		}
	}()
	return func() { close(done) }
}

func tailFile(p string, n int) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > int64(n) {
		f.Seek(st.Size()-int64(n), io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	return strings.TrimSpace(string(b))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// zipTree returns the output tree to the buyer and appends the build log, so a
// failed-looking artifact still explains itself.
func zipTree(root, dst, logPath string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	const maxTotal = 2 << 30
	var total int64
	missingLog := true
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "build.log" {
			missingLog = false
		}
		total += info.Size()
		if total > maxTotal {
			return fmt.Errorf("产物超过 2 GiB")
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, in)
		in.Close()
		return copyErr
	})
	if walkErr == nil && missingLog && logPath != "" {
		if w, err := zw.Create("build.log"); err == nil {
			if in, err := os.Open(logPath); err == nil {
				io.Copy(w, in)
				in.Close()
			}
		}
	}
	closeErr := zw.Close()
	fileErr := f.Close()
	if walkErr == nil {
		walkErr = closeErr
	}
	if walkErr == nil {
		walkErr = fileErr
	}
	return walkErr
}
