package main

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"starstack/internal/ffx"
	"starstack/internal/hub"
	"starstack/internal/render"
)

// blenderBin is overridable because Blender is rarely on PATH on Windows: the
// provider passes --blender "C:\Program Files\Blender Foundation\Blender 4.2\blender.exe".
var blenderBin = "blender"

// bundleState is one job's unpacked upload, shared by every slot on this
// machine. Extracting per slice would unpack a multi-gigabyte bundle N times.
type bundleState struct {
	once  sync.Once
	root  string
	index map[string]string // lower-cased base name -> absolute path
	err   error
}

var bundleCache sync.Map

func (a *agent) bundleFor(c *hub.Claim, src string) (*bundleState, error) {
	v, _ := bundleCache.LoadOrStore(c.JobID, &bundleState{})
	bs := v.(*bundleState)
	bs.once.Do(func() {
		root := filepath.Join(a.work, "job-"+c.JobID)
		if err := os.RemoveAll(root); err != nil {
			bs.err = err
			return
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			bs.err = err
			return
		}
		if err := unzipBundle(src, root); err != nil {
			bs.err = fmt.Errorf("解包工程: %w", err)
			return
		}
		idx, err := fileIndex(root)
		if err != nil {
			bs.err = err
			return
		}
		bs.root, bs.index = root, idx
	})
	return bs, bs.err
}

func (bs *bundleState) lookup(name string) (string, bool) {
	p, ok := bs.index[strings.ToLower(render.BaseName(name))]
	return p, ok
}

// unzipBundle extracts a buyer-supplied project archive. Entries are the one
// part of this pipeline that a tenant fully controls, so every name is checked
// against escaping the destination and every write is size-capped.
func unzipBundle(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	const maxEntry = 4 << 30
	const maxTotal = 8 << 30
	var total int64
	root := filepath.Clean(dst)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ReplaceAll(f.Name, `\`, "/")
		name = strings.TrimPrefix(name, "/")
		clean := path.Clean(name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("压缩包内的路径不安全: %s", f.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(clean))
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("压缩包内的路径不安全: %s", f.Name)
		}
		if f.UncompressedSize64 > maxEntry {
			return fmt.Errorf("%s 解压后超过 4 GiB", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if total > maxTotal {
			return fmt.Errorf("工程解压后超过 8 GiB")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(w, io.LimitReader(rc, maxEntry))
		rc.Close()
		w.Close()
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// fileIndex answers the only question the renderer asks of a bundle: where is
// this file name? The NLE XML carries absolute paths from the editor's disk, so
// the base name is the only portable identity.
func fileIndex(root string) (map[string]string, error) {
	idx := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		key := strings.ToLower(info.Name())
		if _, dup := idx[key]; !dup {
			idx[key] = p
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(idx) == 0 {
		return nil, fmt.Errorf("工程压缩包是空的")
	}
	return idx, nil
}

func (a *agent) runBlender(c *hub.Claim) {
	log.Printf("seg %s (job %s #%d) blender frames %.0f..%.0f", c.SegID, c.JobID, c.Index,
		c.Start, c.Start+c.Dur-1)
	spec := c.Spec
	if spec == nil || spec.Scene == "" {
		a.fail(c, "任务缺少 Blender 场景描述")
		return
	}
	src, err := a.fetchSource(c)
	if err != nil {
		a.fail(c, "source: "+err.Error())
		return
	}
	scene := src
	if c.Bundle {
		bs, err := a.bundleFor(c, src)
		if err != nil {
			a.fail(c, err.Error())
			return
		}
		p, ok := bs.lookup(spec.Scene)
		if !ok {
			a.fail(c, "工程包里找不到场景 "+spec.Scene)
			return
		}
		scene = p
	}
	if _, err := os.Stat(scene); err != nil {
		a.fail(c, "场景文件不可读: "+err.Error())
		return
	}
	from := int(c.Start)
	total := int(c.Dur)
	if total < 1 {
		a.fail(c, "分片帧数为空")
		return
	}
	outDir := filepath.Join(a.work, "frames-"+c.SegID)
	os.RemoveAll(outDir)
	defer os.RemoveAll(outDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		a.fail(c, err.Error())
		return
	}
	zipPath := filepath.Join(a.work, "out-"+c.SegID+".zip")
	defer os.Remove(zipPath)

	timeout := time.Duration(total*300+600) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"-b", scene, "-noaudio",
		"-o", filepath.Join(outDir, "frame_####"), "-F", "PNG",
		"-s", strconv.Itoa(from), "-e", strconv.Itoa(from + total - 1)}
	if spec.Engine != "" {
		args = append(args, "-E", spec.Engine)
	}
	if expr := blenderExpr(spec); expr != "" {
		args = append(args, "--python-expr", expr)
	}
	args = append(args, "-a")

	t0 := time.Now()
	if err := a.blenderRender(ctx, c, args, from, total); err != nil {
		a.fail(c, err.Error())
		return
	}
	wall := time.Since(t0).Seconds()
	if err := zipFrames(outDir, zipPath); err != nil {
		a.fail(c, err.Error())
		return
	}
	speed := 0.0
	if wall > 0 {
		speed = float64(total) / wall
	}
	a.finish(c, zipPath, wall, speed)
}

// blenderExpr changes only what the submitter explicitly asked for. Everything
// else stays as the .blend author set it, so a distributed render looks like
// the local one.
func blenderExpr(spec *render.Spec) string {
	var parts []string
	if spec.Percent > 0 {
		parts = append(parts, fmt.Sprintf("s.render.resolution_percentage=%d", spec.Percent))
	}
	if spec.Samples > 0 {
		parts = append(parts, fmt.Sprintf("s.cycles.samples=%d", spec.Samples))
	}
	if spec.FPS > 0 {
		fps := spec.FPS
		if d := float64(int(fps + 0.5)); fps > 0 && absF(d-fps) < 0.001 {
			parts = append(parts, fmt.Sprintf("s.render.fps=%d; s.render.fps_base=1", int(d)))
		} else {
			// Blender stores a rate as fps/fps_base, so NTSC 23.976 becomes 1000/41.708
			parts = append(parts, fmt.Sprintf("s.render.fps=1000; s.render.fps_base=%.6f", 1000/fps))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "import bpy; s=bpy.context.scene; " + strings.Join(parts, "; ")
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-w.max:]...)
	}
	w.mu.Unlock()
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func (a *agent) blenderRender(ctx context.Context, c *hub.Claim, args []string, first, total int) error {
	cmd := exec.CommandContext(ctx, blenderBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errBuf := &tailWriter{max: 4096}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Blender 失败（节点是否安装了 blender？）: %w", err)
	}
	last := time.Time{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		// Blender repaints its progress on one line with carriage returns
		for _, line := range strings.Split(sc.Text(), "\r") {
			i := strings.Index(line, "Fra:")
			if i < 0 {
				continue
			}
			f := strings.Fields(strings.TrimSpace(line[i+4:]))
			if len(f) == 0 {
				continue
			}
			n, err := strconv.Atoi(f[0])
			if err != nil || n < first {
				continue
			}
			if time.Since(last) < time.Second {
				continue
			}
			last = time.Now()
			pct := 100 * float64(n-first+1) / float64(total)
			if pct > 99 {
				pct = 99
			}
			a.post(a.server+"/api/report", map[string]any{
				"worker": a.id, "seg": c.SegID, "state": "running", "pct": pct,
			}, true)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("blender 退出: %v %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

func zipFrames(dir, dst string) error {
	files, err := filepath.Glob(filepath.Join(dir, "frame_*.png"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("Blender 没有输出任何 PNG 帧")
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, p := range files {
		w, err := zw.Create(filepath.Base(p))
		if err != nil {
			zw.Close()
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			zw.Close()
			return err
		}
		_, err = io.Copy(w, src)
		src.Close()
		if err != nil {
			zw.Close()
			return err
		}
	}
	return zw.Close()
}

// runTimeline renders one time range of an edited sequence. Each cut is encoded
// to the same normalised format, gaps become black, and the pieces are joined
// on this machine: the broker only has to copy-concat per-slice files.
func (a *agent) runTimeline(c *hub.Claim) {
	log.Printf("seg %s (job %s #%d) timeline %.2f..%.2fs, %d clips", c.SegID, c.JobID,
		c.Index, c.Start, c.Start+c.Dur, len(c.Clips))
	spec := c.Spec
	if spec == nil {
		a.fail(c, "任务缺少时间线描述")
		return
	}
	src, err := a.fetchSource(c)
	if err != nil {
		a.fail(c, "source: "+err.Error())
		return
	}
	bs, err := a.bundleFor(c, src)
	if err != nil {
		a.fail(c, err.Error())
		return
	}
	type piece struct {
		asset    string
		path     string
		in, dur  float64
		at       float64
		hasAudio bool
	}
	pieces := make([]piece, 0, len(c.Clips))
	for _, clip := range c.Clips {
		p, ok := bs.lookup(clip.Asset)
		if !ok {
			a.fail(c, "工程包里找不到素材 "+clip.Asset)
			return
		}
		pieces = append(pieces, piece{asset: clip.Asset, path: p, in: clip.In, dur: clip.Dur, at: clip.At})
	}

	width, height := spec.Width, spec.Height
	fps := spec.FPS
	if fps <= 0 {
		fps = 30
	}
	// Probe once per asset: dimensions fallback and audio presence. A timeline
	// with no audio at all still gets a silent track so every piece matches.
	probes := map[string]*ffx.Media{}
	for i := range pieces {
		m := probes[pieces[i].path]
		if m == nil {
			ctxProbe, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			m, err = ffx.Probe(ctxProbe, pieces[i].path)
			cancel()
			if err != nil {
				a.fail(c, "读取素材 "+pieces[i].asset+" 失败: "+err.Error())
				return
			}
			probes[pieces[i].path] = m
		}
		pieces[i].hasAudio = m.HasAudio
		if width <= 0 || height <= 0 {
			width, height = m.Width, m.Height
		}
	}
	if width <= 0 || height <= 0 {
		width, height = 1920, 1080
	}

	partsDir := filepath.Join(a.work, "parts-"+c.SegID)
	os.RemoveAll(partsDir)
	defer os.RemoveAll(partsDir)
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		a.fail(c, err.Error())
		return
	}
	dst := filepath.Join(a.work, "out-"+c.SegID+".mkv")
	defer os.Remove(dst)

	timeout := time.Duration(c.Dur*120+300) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var parts []string
	cursor := 0.0
	done := 0.0
	addGap := func(d float64) error {
		if d <= 1e-3 {
			return nil
		}
		p := filepath.Join(partsDir, fmt.Sprintf("part%03d.mkv", len(parts)))
		if err := ffx.BlackClip(ctx, p, d, width, height, fps); err != nil {
			return err
		}
		parts = append(parts, p)
		done += d
		return nil
	}
	if len(pieces) == 0 {
		if err := addGap(c.Dur); err != nil {
			a.fail(c, err.Error())
			return
		}
	} else if pieces[0].at > 1e-3 {
		if err := addGap(pieces[0].at); err != nil {
			a.fail(c, err.Error())
			return
		}
		cursor = pieces[0].at
	}
	t0 := time.Now()
	for _, pc := range pieces {
		if pc.at > cursor+1e-3 {
			if err := addGap(pc.at - cursor); err != nil {
				a.fail(c, err.Error())
				return
			}
		}
		p := filepath.Join(partsDir, fmt.Sprintf("part%03d.mkv", len(parts)))
		base := done
		err := ffx.RenderClip(ctx, ffx.ClipOpts{
			Src: pc.path, Dst: p, In: pc.in, Dur: pc.dur,
			Width: width, Height: height, FPS: fps, CRF: 20, HasAudio: pc.hasAudio,
		}, func(pr ffx.Progress) {
			frac := pr.Out
			if frac > pc.dur {
				frac = pc.dur
			}
			pct := 100 * (base + frac) / c.Dur
			if pct > 99 {
				pct = 99
			}
			a.post(a.server+"/api/report", map[string]any{
				"worker": a.id, "seg": c.SegID, "state": "running",
				"pct": pct, "speed": pr.Speed,
			}, true)
		})
		if err != nil {
			a.fail(c, fmt.Sprintf("渲染 %s (%.2fs): %v", pc.asset, pc.dur, err))
			return
		}
		parts = append(parts, p)
		done += pc.dur
		cursor = pc.at + pc.dur
	}
	if cursor < c.Dur-1e-3 {
		if err := addGap(c.Dur - cursor); err != nil {
			a.fail(c, err.Error())
			return
		}
	}
	if len(parts) == 0 {
		a.fail(c, "这一片没有可渲染的片段")
		return
	}
	if err := ffx.Concat(ctx, parts, dst); err != nil {
		log.Printf("seg %s: copy concat failed (%v), retrying with re-encode", c.SegID, err)
		if err2 := ffx.ConcatEncode(ctx, parts, dst); err2 != nil {
			a.fail(c, fmt.Sprintf("拼接本片失败: %v / %v", err, err2))
			return
		}
	}
	wall := time.Since(t0).Seconds()
	speed := 0.0
	if wall > 0 {
		speed = c.Dur / wall
	}
	a.finish(c, dst, wall, speed)
}

// finish is the delivery half shared by all three job kinds: announce the
// upload, keep the lease alive through it, verify what the broker stored.
func (a *agent) finish(c *hub.Claim, path string, wall, speed float64) {
	a.post(a.server+"/api/report", map[string]any{"worker": a.id, "seg": c.SegID, "state": "uploading"}, true)
	var pushed int64
	stopBeat := a.beat(c.SegID, "uploading", &pushed, 0)
	sha, size, err := a.upload(c.SegID, path)
	stopBeat()
	if err != nil {
		a.fail(c, "upload: "+err.Error())
		return
	}
	a.post(a.server+"/api/report", map[string]any{
		"worker": a.id, "seg": c.SegID, "state": "done",
		"speed": speed, "sha": sha, "bytes": size, "wall": wall,
	}, true)
	log.Printf("seg %s done in %.1fs (%.2f units/s, %.1f MiB)", c.SegID, wall, speed, float64(size)/(1<<20))
}
