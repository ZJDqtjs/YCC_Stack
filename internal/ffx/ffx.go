package ffx

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	Ffmpeg  = "ffmpeg"
	Ffprobe = "ffprobe"
)

type stream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type probeOut struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []stream `json:"streams"`
}

type Media struct {
	Duration float64
	HasAudio bool
	Width    int
	Height   int
	VCodec   string
}

func Probe(ctx context.Context, path string) (*Media, error) {
	cmd := exec.CommandContext(ctx, Ffprobe, "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path)
	var buf, errBuf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	var po probeOut
	if err := json.Unmarshal([]byte(buf.String()), &po); err != nil {
		return nil, err
	}
	m := &Media{}
	m.Duration, _ = strconv.ParseFloat(po.Format.Duration, 64)
	for _, s := range po.Streams {
		switch s.CodecType {
		case "video":
			if m.Width == 0 {
				m.Width, m.Height, m.VCodec = s.Width, s.Height, s.CodecName
			}
		case "audio":
			m.HasAudio = true
		}
	}
	if m.Duration <= 0 {
		return nil, fmt.Errorf("cannot read duration from %s", path)
	}
	return m, nil
}

// Keyframes returns pts_time of every video keyframe. -skip_frame nokey makes
// ffmpeg drop non-key frames before decode, so this stays cheap on long files.
func Keyframes(ctx context.Context, path string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, Ffprobe, "-v", "error", "-select_streams", "v:0",
		"-skip_frame", "nokey", "-show_entries", "frame=best_effort_timestamp_time",
		"-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe keyframes: %w", err)
	}
	var kfs []float64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if v, err := strconv.ParseFloat(line, 64); err == nil && !math.IsNaN(v) && v >= 0 {
			kfs = append(kfs, v)
		}
	}
	sort.Float64s(kfs)
	return kfs, nil
}

type Seg struct{ Start, Dur float64 }

// Plan cuts [0,duration] into ~want pieces, moving every cut to the nearest
// keyframe so each piece starts on an IDR frame.
func Plan(duration float64, kfs []float64, want int) []Seg {
	if want < 1 {
		want = 1
	}
	if duration <= 0 {
		return nil
	}
	cuts := make([]float64, 0, want+1)
	cuts = append(cuts, 0)
	if want > 1 && len(kfs) > 1 {
		step := duration / float64(want)
		prev := 0.0
		for i := 1; i < want; i++ {
			ideal := float64(i) * step
			best, bestDist := -1.0, math.Inf(1)
			for _, k := range kfs {
				if k <= prev+0.01 || k >= duration-0.01 {
					continue
				}
				if d := math.Abs(k - ideal); d < bestDist {
					bestDist, best = d, k
				}
			}
			if best < 0 {
				continue
			}
			prev = best
			cuts = append(cuts, best)
		}
	}
	cuts = append(cuts, duration)
	segs := make([]Seg, 0, len(cuts)-1)
	for i := 0; i+1 < len(cuts); i++ {
		d := cuts[i+1] - cuts[i]
		if d <= 0.01 {
			continue
		}
		segs = append(segs, Seg{Start: cuts[i], Dur: d})
	}
	return segs
}

type EncOpts struct {
	Src, Dst string
	Start    float64
	Dur      float64 // <=0 means encode to end of file
	VCodec   string
	Preset   string
	Q        int
	Height   int // 0 keeps source height
	Audio    bool
}

type Progress struct {
	Out   float64
	Speed float64
	Frame int64
}

var nvencPreset = map[string]string{
	"ultrafast": "p1", "superfast": "p2", "veryfast": "p3", "faster": "p4",
	"fast": "p4", "medium": "p5", "slow": "p6", "slower": "p7", "veryslow": "p7",
}

func (o EncOpts) args() []string {
	a := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-progress", "pipe:1", "-y"}
	if o.Start > 0 {
		a = append(a, "-ss", secStr(o.Start))
	}
	a = append(a, "-i", o.Src)
	if o.Dur > 0 {
		a = append(a, "-t", secStr(o.Dur))
	}
	a = append(a, "-map", "0:v:0", "-map", "0:a:0?")
	if o.Height > 0 {
		a = append(a, "-vf", fmt.Sprintf("scale=-2:%d:flags=bicubic", o.Height))
	}
	a = append(a, "-c:v", o.VCodec)
	if strings.Contains(o.VCodec, "nvenc") {
		p := nvencPreset[o.Preset]
		if p == "" {
			p = "p5"
		}
		a = append(a, "-preset", p, "-rc", "constqp", "-qp", strconv.Itoa(o.Q))
	} else {
		p := o.Preset
		if p == "" {
			p = "medium"
		}
		a = append(a, "-preset", p, "-crf", strconv.Itoa(o.Q))
	}
	if o.Audio {
		a = append(a, "-c:a", "aac", "-b:a", "128k", "-af", "aresample=async=1:first_pts=0")
	} else {
		a = append(a, "-an")
	}
	return append(a, o.Dst)
}

func secStr(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }

// Encode runs one ffmpeg pass and returns the measured throughput in
// video-seconds encoded per wall-second.
func Encode(ctx context.Context, o EncOpts, onProgress func(Progress)) (float64, error) {
	if err := os.MkdirAll(filepath.Dir(o.Dst), 0o755); err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, Ffmpeg, o.args()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	target := o.Dur
	lastNotify := time.Time{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		kv := strings.SplitN(sc.Text(), "=", 2)
		if len(kv) != 2 {
			continue
		}
		var p Progress
		switch kv[0] {
		case "out_time_us":
			if us, err := strconv.ParseInt(kv[1], 10, 64); err == nil {
				p.Out = float64(us) / 1e6
			}
		case "speed":
			p.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(kv[1]), "x"), 64)
		case "frame":
			p.Frame, _ = strconv.ParseInt(kv[1], 10, 64)
		default:
			continue
		}
		if onProgress != nil && (p.Out > 0 || p.Frame > 0) && time.Since(lastNotify) > 500*time.Millisecond {
			lastNotify = time.Now()
			onProgress(p)
		}
	}
	err = cmd.Wait()
	wall := time.Since(start).Seconds()
	if err != nil {
		return 0, fmt.Errorf("ffmpeg exit: %v %s", err, tail(errBuf.String(), 1200))
	}
	if _, statErr := os.Stat(o.Dst); statErr != nil {
		return 0, fmt.Errorf("no output produced: %s", tail(errBuf.String(), 600))
	}
	if target <= 0 {
		target = wall
	}
	if wall <= 0 {
		return 0, nil
	}
	return target / wall, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ClipOpts describes one cut of an edited timeline. Every piece of a parallel
// timeline render goes through this same shape, because the broker joins the
// results with a stream copy: same size, same rate, same pixel format, same
// audio layout is what makes the pieces interchangeable.
type ClipOpts struct {
	Src, Dst      string
	In, Dur       float64
	Width, Height int
	FPS           float64
	CRF           int
	HasAudio      bool
}

func (o ClipOpts) args() []string {
	crf := o.CRF
	if crf <= 0 {
		crf = 20
	}
	fps := o.FPS
	if fps <= 0 {
		fps = 30
	}
	w, h := o.Width, o.Height
	if w <= 0 {
		w = 1920
	}
	if h <= 0 {
		h = 1080
	}
	// libx264 wants even dimensions; a 1919-wide source must not fail the slice
	w, h = w&^1, h&^1
	a := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats",
		"-progress", "pipe:1", "-y"}
	if o.In > 0 {
		a = append(a, "-ss", secStr(o.In))
	}
	a = append(a, "-i", o.Src)
	if !o.HasAudio {
		// a silent bed keeps every slice's stream layout identical, so a clip
		// without sound cannot force the final concat into a re-encode
		a = append(a, "-f", "lavfi", "-t", secStr(o.Dur), "-i", "anullsrc=r=48000:cl=stereo")
	}
	if o.Dur > 0 {
		a = append(a, "-t", secStr(o.Dur))
	}
	a = append(a, "-map", "0:v:0")
	if o.HasAudio {
		a = append(a, "-map", "0:a:0")
	} else {
		a = append(a, "-map", "1:a:0")
	}
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,"+
		"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%s,format=yuv420p",
		w, h, w, h, secStr(fps))
	a = append(a, "-vf", vf, "-c:v", "libx264", "-preset", "veryfast", "-crf",
		strconv.Itoa(crf), "-g", strconv.Itoa(int(fps+0.5)), "-keyint_min", strconv.Itoa(int(fps+0.5)),
		"-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-movflags", "+faststart", o.Dst)
	return a
}

// RenderClip encodes one trimmed, normalised piece of a timeline.
func RenderClip(ctx context.Context, o ClipOpts, onProgress func(Progress)) error {
	if err := os.MkdirAll(filepath.Dir(o.Dst), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, Ffmpeg, o.args()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	lastNotify := time.Time{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		kv := strings.SplitN(sc.Text(), "=", 2)
		if len(kv) != 2 || onProgress == nil {
			continue
		}
		var p Progress
		switch kv[0] {
		case "out_time_us":
			if us, err := strconv.ParseInt(kv[1], 10, 64); err == nil {
				p.Out = float64(us) / 1e6
			}
		case "speed":
			p.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(kv[1]), "x"), 64)
		default:
			continue
		}
		if time.Since(lastNotify) > 900*time.Millisecond {
			lastNotify = time.Now()
			onProgress(p)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg clip: %v %s", err, tail(errBuf.String(), 1000))
	}
	if _, err := os.Stat(o.Dst); err != nil {
		return fmt.Errorf("no clip produced: %s", tail(errBuf.String(), 600))
	}
	return nil
}

// BlackClip fills a gap in the timeline that no source clip covers, with the
// same stream layout as RenderClip so it can sit between two rendered cuts.
func BlackClip(ctx context.Context, dst string, dur float64, width, height int, fps float64) error {
	if fps <= 0 {
		fps = 30
	}
	if width <= 0 {
		width = 1920
	}
	if height <= 0 {
		height = 1080
	}
	width, height = width&^1, height&^1
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=%s", width, height, secStr(fps)),
		"-f", "lavfi", "-t", secStr(dur), "-i", "anullsrc=r=48000:cl=stereo",
		"-t", secStr(dur), "-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
		"-pix_fmt", "yuv420p", "-g", strconv.Itoa(int(fps + 0.5)), "-keyint_min", strconv.Itoa(int(fps + 0.5)),
		"-sc_threshold", "0", "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-movflags", "+faststart", dst}
	var errBuf strings.Builder
	cmd := exec.CommandContext(ctx, Ffmpeg, args...)
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("black clip: %v %s", err, tail(errBuf.String(), 800))
	}
	return nil
}

func Concat(ctx context.Context, parts []string, dst string) error {
	return concatArgs(ctx, parts, dst, []string{"-c", "copy"})
}

// ConcatEncode joins pieces that a copy-concat refused, re-encoding them into a
// single stream. It is the fallback for slices produced on different machines:
// the command line was the same, but encoder extradata can still differ.
func ConcatEncode(ctx context.Context, parts []string, dst string) error {
	return concatArgs(ctx, parts, dst, []string{"-c:v", "libx264", "-preset", "veryfast",
		"-crf", "20", "-c:a", "aac", "-b:a", "192k"})
}

func concatArgs(ctx context.Context, parts []string, dst string, codecArgs []string) error {
	list := dst + ".concat.txt"
	var b strings.Builder
	for _, p := range parts {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		b.WriteString("file '")
		b.WriteString(strings.ReplaceAll(filepath.ToSlash(abs), "'", `'\''`))
		b.WriteString("'\n")
	}
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		return err
	}
	defer os.Remove(list)
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list}
	args = append(args, codecArgs...)
	args = append(args, "-movflags", "+faststart", dst)
	cmd := exec.CommandContext(ctx, Ffmpeg, args...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("concat: %v %s", err, tail(errBuf.String(), 800))
	}
	return nil
}

// EncodeFrames turns a directory of PNG frames into a video. first is the lowest
// frame number present; Blender numbers frames by their scene frame, not by
// position in the slice.
func EncodeFrames(ctx context.Context, dir string, first int, fps float64, dst string) error {
	if fps <= 0 {
		fps = 24
	}
	pattern := filepath.Join(dir, "frame_%04d.png")
	cmd := exec.CommandContext(ctx, Ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-framerate", strconv.FormatFloat(fps, 'f', 3, 64),
		"-start_number", strconv.Itoa(first),
		"-i", pattern,
		"-c:v", "libx264", "-preset", "medium", "-crf", "18",
		"-pix_fmt", "yuv420p", "-movflags", "+faststart", dst)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("encode frames: %v %s", err, tail(errBuf.String(), 800))
	}
	return nil
}

func Encoders(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, Ffmpeg, "-hide_banner", "-encoders")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var list []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[0]) == 6 && (f[0][0] == 'V' || f[0][0] == 'A') {
			switch f[1] {
			case "libx264", "libx265", "h264_nvenc", "hevc_nvenc", "h264_amf", "hevc_amf",
				"h264_qsv", "hevc_qsv", "libsvtav1", "libopenh264":
				list = append(list, f[1])
			}
		}
	}
	return list, nil
}

// Inventory is the hardware/software picture a provider agent reports at
// registration. The broker's hard-constraint filter matches a task's
// requirements against exactly these fields, so a capability an agent cannot
// measure is reported as zero and treated by the scheduler as "unknown"
// (never a reason to exclude, nor a promise to a buyer).
type Inventory struct {
	Hostname   string      `json:"hostname"`
	OS         string      `json:"os"`
	Arch       string      `json:"arch"`
	CPUs       int         `json:"cpus"`
	MemMB      uint64      `json:"mem_mb"`
	DiskFreeMB uint64      `json:"disk_free_mb"`
	GPUs       []GPUDetail `json:"gpus"`
	Encoders   []string    `json:"encoders"`
	Runtimes   []Runtime   `json:"runtimes"`
	FFmpegVer  string      `json:"ffmpeg_version"`
}

type GPUDetail struct {
	Name  string `json:"name"`
	MemMB int    `json:"mem_mb"`
}

type Runtime struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path,omitempty"`
}

// Detect measures this machine once: it is what the scheduler is allowed to
// promise a buyer about the hardware and software standing behind the agent.
func Detect(ctx context.Context, workDir string) Inventory {
	host, _ := os.Hostname()
	encs, err := Encoders(ctx)
	if err != nil {
		log.Printf("encoder probe failed: %v", err)
	}
	rt := DetectRuntimes(ctx)
	ver := ""
	for _, r := range rt {
		if r.Name == "ffmpeg" {
			ver = r.Version
		}
	}
	return Inventory{
		Hostname:   host,
		OS:         runtime.GOOS + "/" + runtime.GOARCH,
		Arch:       runtime.GOARCH,
		CPUs:       runtime.NumCPU(),
		MemMB:      MemTotalMB(),
		DiskFreeMB: DiskFreeMB(workDir),
		GPUs:       GPUs(ctx),
		Encoders:   encs,
		Runtimes:   rt,
		FFmpegVer:  ver,
	}
}

func firstLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return s
}

func FFmpegVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, Ffmpeg, "-hide_banner", "-version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(firstLine(out), "ffmpeg version ")
}

// DetectRuntimes records which task images this machine can actually execute.
// Absent tools are simply not advertised; the scheduler treats that as an
// unmet hard constraint rather than an error.
func DetectRuntimes(ctx context.Context) []Runtime {
	var rs []Runtime
	if v := FFmpegVersion(ctx); v != "" {
		rs = append(rs, Runtime{Name: "ffmpeg", Version: v, Path: Ffmpeg})
	}
	for _, spec := range []struct{ bin, name string }{
		{"blender", "blender"}, {"nvcc", "cuda"}, {"docker", "docker"}, {"python", "python"},
	} {
		p, err := exec.LookPath(spec.bin)
		if err != nil {
			continue
		}
		c2, cancel := context.WithTimeout(ctx, 4*time.Second)
		out, err := exec.CommandContext(c2, p, "--version").CombinedOutput()
		cancel()
		ver := ""
		if err == nil {
			ver = firstLine(out)
			if i := strings.IndexByte(ver, ' '); i > 0 {
				ver = ver[i+1:]
			}
		}
		rs = append(rs, Runtime{Name: spec.name, Version: ver, Path: p})
	}
	return rs
}

func GPUs(ctx context.Context) []GPUDetail {
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var list []GPUDetail
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		mem, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		list = append(list, GPUDetail{Name: strings.TrimSpace(parts[0]), MemMB: mem})
	}
	return list
}
