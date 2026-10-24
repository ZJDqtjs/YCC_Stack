package hub

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"starstack/internal/ffx"
	"starstack/internal/render"
	"starstack/internal/store"
)

// Renders are jobs whose unit of work is a frame range or a stretch of an
// edited timeline rather than a slice of one video file. Everything that makes
// a job a job — holding credits, matching hardware, leasing slices, verifying
// uploads — is shared with transcoding; only the planner, the artifact, and the
// merge differ, and those three are in this file.

const maxRenderFrames = 20000

func artifactName(kind string, index int) string {
	if kind == render.KindBlender {
		// frames arrive as a zip; the broker stitches them into a video later
		return fmt.Sprintf("seg%03d.zip", index)
	}
	return fmt.Sprintf("seg%03d.mkv", index)
}

func (s *Server) artifactPath(jobID, kind string, index int) string {
	return filepath.Join(s.dataDir, "jobs", jobID, artifactName(kind, index))
}

// handleCreateRender accepts a creative project, plans it into independent
// slices and queues it exactly like a transcode job. The planning is the whole
// product decision here: what a machine is asked to do must be self-contained,
// or the pieces cannot be rendered in parallel at all.
func (s *Server) handleCreateRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, jsonErr("请先登录后再提交任务"), 401)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.policy.MaxUploadBytes)
	if err := r.ParseMultipartForm(s.policy.MemoryBuffer); err != nil {
		s.deny(w, 400, "上传失败或超出大小上限", "render upload", err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	form := r.Form
	kind := strings.TrimSpace(form.Get("kind"))
	if kind != render.KindBlender && kind != render.KindPR {
		http.Error(w, jsonErr("kind 必须是 blender 或 pr"), 400)
		return
	}
	segs := atoi(first(form, "segs", "4"))
	if segs < 1 {
		segs = 1
	}
	if segs > s.policy.MaxSegs {
		segs = s.policy.MaxSegs
	}
	var req Req
	if rs := form.Get("req"); rs != "" && rs != "{}" {
		if len(rs) > 4096 {
			http.Error(w, jsonErr("硬件要求过长"), 400)
			return
		}
		if err := json.Unmarshal([]byte(rs), &req); err != nil {
			http.Error(w, jsonErr("硬件要求格式错误"), 400)
			return
		}
	}

	// Upload into a throwaway job directory first: a rejected submission must
	// not leave its bundle behind, same rule as the transcode path.
	id := newID()
	dir := filepath.Join(s.dataDir, "jobs", id)
	if err := s.diskOk(dir); err != nil {
		s.deny(w, 507, "Broker 磁盘空间不足，请稍后重试", "disk", err)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "field 'file' required", 400)
		return
	}
	defer f.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.deny(w, 500, "写入失败", "mkdir", err)
		return
	}
	srcPath := filepath.Join(dir, "source"+safeExt(hdr.Filename))
	out, err := os.Create(srcPath)
	if err != nil {
		s.deny(w, 500, "写入失败", "create", err)
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		s.deny(w, 500, "上传中断", "copy", err)
		return
	}
	out.Close()

	accepted := false
	defer func() {
		if !accepted {
			os.RemoveAll(dir)
		}
	}()

	job := &Job{ID: id, Name: hdr.Filename, Kind: kind, Req: req, Status: "queued",
		CreatedAt: time.Now(), path: srcPath,
		Profile: Profile{VCodec: "libx264", Preset: "veryfast", Q: 23, Audio: true, Segs: segs}}
	var note string
	var plan []render.Slice
	switch kind {
	case render.KindBlender:
		spec, slices, n, err := s.planBlender(form, srcPath, hdr.Filename, segs)
		if err != nil {
			http.Error(w, jsonErr(err.Error()), 400)
			return
		}
		job.Spec, plan, note = spec, slices, n
	case render.KindPR:
		spec, slices, n, err := s.planTimeline(srcPath, hdr.Filename, segs)
		if err != nil {
			http.Error(w, jsonErr(err.Error()), 400)
			return
		}
		job.Spec, plan, note = spec, slices, n
	}
	if len(plan) == 0 {
		http.Error(w, jsonErr("这个工程没有可切分的独立片段"), 400)
		return
	}
	job.Duration = 0
	for _, sl := range plan {
		job.Segs = append(job.Segs, &Seg{
			ID: id + "-" + strconv.Itoa(sl.Index), JobID: id, Index: sl.Index,
			Start: sl.Start, Dur: sl.Dur, Status: "pending",
		})
		job.Duration += sl.Dur
	}
	job.Duration = round3(job.Duration)

	s.mu.Lock()
	if live := s.countLiveJobsLocked(u.ID); live >= s.policy.MaxActiveJobs {
		s.mu.Unlock()
		http.Error(w, jsonErr(fmt.Sprintf("你有 %d 个任务在跑或排队，单账户上限 %d 个",
			live, s.policy.MaxActiveJobs)), http.StatusTooManyRequests)
		return
	}
	hold, holdPrice, basis := s.estimateHoldLocked(job)
	job.OwnerID, job.Hold = u.ID, hold
	job.Note = note
	needReq, needVCodec := jobReq(job)
	if n := s.eligibleLocked(needReq, needVCodec); n == 0 {
		job.Note += fmt.Sprintf("；当前没有满足要求的在线节点（需要 %s），任务将排队等待",
			renderNeed(job))
	} else {
		job.Note += fmt.Sprintf("；共 %d 台节点满足要求；预扣依据：%s × %.2f 点/%s",
			n, basis, holdPrice, job.Spec.Unit())
	}
	if u.Credits < hold {
		s.mu.Unlock()
		http.Error(w, jsonErr(fmt.Sprintf("算力点不足：本次任务预扣 %.2f 点，余额 %.2f 点，请先充值",
			hold, u.Credits)), http.StatusPaymentRequired)
		return
	}
	if _, err := s.chargeJob(job, hold, store.Hold, "渲染任务预扣（按实际用量结算，差额退还）"); err != nil {
		s.mu.Unlock()
		http.Error(w, jsonErr("预扣失败"), 500)
		return
	}
	if err := s.st.InsertJob(jobRowOf(job), segRowsOf(job)); err != nil {
		s.mu.Unlock()
		http.Error(w, jsonErr("任务登记失败"), 500)
		return
	}
	s.jobs[id] = job
	s.order = append(s.order, id)
	accepted = true
	log.Printf("render job %s (%s) queued by %s: %d pieces, %.1f %s, hold=%.3f points",
		id, kind, u.Name, len(job.Segs), job.Duration, job.Spec.Unit(), hold)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "id": id, "kind": kind, "note": job.Note,
		"pieces": len(job.Segs), "hold": hold, "units": job.Duration, "unit": job.Spec.Unit()})
}

func renderNeed(j *Job) string {
	if j.Kind == render.KindBlender {
		return "安装了 Blender 的节点"
	}
	return "ffmpeg 节点"
}

// planBlender validates a .blend or a bundle containing one, and cuts the frame
// range into contiguous chunks. A .blend that references textures or caches is
// only self-contained if those travel with it, which is why a zip is accepted
// and preferred.
func (s *Server) planBlender(form map[string][]string, srcPath, uploadName string, segs int) (*render.Spec, []render.Slice, string, error) {
	isZip := strings.EqualFold(filepath.Ext(uploadName), ".zip")
	spec := &render.Spec{Kind: render.KindBlender, Bundle: isZip}
	spec.Engine = strings.ToUpper(strings.TrimSpace(first(form, "engine", "")))
	switch spec.Engine {
	case "", "CYCLES", "BLENDER_EEVEE", "BLENDER_EEVEE_NEXT", "BLENDER_WORKBENCH":
	default:
		return nil, nil, "", errors.New("不支持的渲染引擎")
	}
	spec.Samples = atoi(first(form, "samples", "0"))
	if spec.Samples < 0 || spec.Samples > 100000 {
		return nil, nil, "", errors.New("采样数不合理")
	}
	spec.Percent = atoi(first(form, "percent", "0"))
	if spec.Percent != 0 && (spec.Percent < 1 || spec.Percent > 100) {
		return nil, nil, "", errors.New("分辨率百分比需在 1~100 之间")
	}
	spec.FPS = floatOr(first(form, "fps", "24"), 24)
	if spec.FPS < 0 || spec.FPS > 240 {
		return nil, nil, "", errors.New("帧率需在 0~240 之间")
	}

	want := first(form, "scene", "")
	if isZip {
		idx, err := zipIndex(srcPath)
		if err != nil {
			return nil, nil, "", fmt.Errorf("读不了这个 zip：%v", err)
		}
		if want != "" {
			entry, ok := idx[strings.ToLower(render.BaseName(want))]
			if !ok || !strings.HasSuffix(strings.ToLower(entry), ".blend") {
				return nil, nil, "", fmt.Errorf("压缩包里没有 %s", want)
			}
			spec.Scene = render.BaseName(entry)
		}
		var scenes []string
		for lower, name := range idx {
			if strings.HasSuffix(lower, ".blend") {
				scenes = append(scenes, name)
			}
		}
		if spec.Scene == "" {
			switch len(scenes) {
			case 0:
				return nil, nil, "", errors.New("压缩包里没有 .blend 文件")
			case 1:
				spec.Scene = render.BaseName(scenes[0])
			default:
				return nil, nil, "", fmt.Errorf("压缩包里有 %d 个 .blend，请用 scene 字段指定其中一个：%s",
					len(scenes), strings.Join(scenes, ", "))
			}
		}
	} else {
		if !strings.HasSuffix(strings.ToLower(uploadName), ".blend") {
			return nil, nil, "", errors.New("Blender 渲染要上传 .blend，或把场景和素材一起打成 zip")
		}
		spec.Scene = render.BaseName(uploadName)
	}
	from := atoi(first(form, "from", "1"))
	to := atoi(first(form, "to", "0"))
	if to <= 0 {
		return nil, nil, "", errors.New("请填写结束帧 to")
	}
	if to < from {
		return nil, nil, "", errors.New("结束帧不能小于起始帧")
	}
	total := to - from + 1
	if total > maxRenderFrames {
		return nil, nil, "", fmt.Errorf("帧数 %d 超过上限 %d", total, maxRenderFrames)
	}
	plan := render.PlanFrames(from, to, segs)
	note := fmt.Sprintf("Blender 场景 %s，第 %d~%d 帧（%d 帧），切成 %d 段",
		spec.Scene, from, to, total, len(plan))
	if spec.Engine != "" {
		note += "，引擎 " + spec.Engine
	}
	if spec.Samples > 0 {
		note += fmt.Sprintf("，采样 %d", spec.Samples)
	}
	if spec.Percent > 0 {
		note += fmt.Sprintf("，分辨率 %d%%", spec.Percent)
	}
	if isZip {
		note += "；素材随场景一起下发"
	}
	return spec, plan, note, nil
}

// planTimeline accepts a zip with the NLE XML and every referenced asset. Media
// paths inside the XML point at the editor's own disk, so matching by base name
// is the only thing that survives the move to a provider machine.
func (s *Server) planTimeline(srcPath, uploadName string, segs int) (*render.Spec, []render.Slice, string, error) {
	if !strings.EqualFold(filepath.Ext(uploadName), ".zip") {
		return nil, nil, "", errors.New("时间线渲染要上传 zip：里面放 Premiere 导出的 XML 和所有素材")
	}
	idx, err := zipIndex(srcPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("读不了这个 zip：%v", err)
	}
	var tl *render.Timeline
	var xmlName string
	for lower, name := range idx {
		if !strings.HasSuffix(lower, ".xml") && !strings.HasSuffix(lower, ".fcpxml") {
			continue
		}
		data, err := readZipEntry(srcPath, name, 32<<20)
		if err != nil {
			continue
		}
		if t, err := render.ParseTimeline(data); err == nil {
			tl, xmlName = t, name
			break
		}
	}
	if tl == nil {
		return nil, nil, "", errors.New("压缩包里没有可解析的时间线 XML（Premiere：文件 → 导出 → Final Cut Pro XML；或 FCPXML）")
	}
	var missing []string
	for _, a := range tl.Assets() {
		if _, ok := idx[strings.ToLower(a)]; !ok {
			missing = append(missing, a)
		}
	}
	if len(missing) > 0 {
		if len(missing) > 6 {
			missing = append(missing[:6], fmt.Sprintf("…另有 %d 个", len(missing)-6))
		}
		return nil, nil, "", fmt.Errorf("压缩包里缺少这些素材：%s", strings.Join(missing, ", "))
	}
	plan := render.PlanTimeline(tl, segs)
	if len(plan) == 0 {
		return nil, nil, "", errors.New("时间线没有可切分的时长")
	}
	spec := &render.Spec{Kind: render.KindPR, Timeline: tl, Bundle: true,
		Width: tl.Width, Height: tl.Height, FPS: tl.FPS}
	note := fmt.Sprintf("时间线「%s」%.1fs / %d 个镜头，%dx%d@%.2ffps，切成 %d 段（XML：%s）",
		tl.Name, tl.Duration, len(tl.Clips), tl.Width, tl.Height, tl.FPS, len(plan), xmlName)
	if tl.Note != "" {
		note += "；" + tl.Note
	}
	return spec, plan, note, nil
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

func floatOr(s string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return v
}

// ---- bundle inspection ----

// zipIndex maps lower-cased base names to the entry's in-zip path. Two different
// directories holding a.mov and b/a.mov is not a conflict; two a.mov files are,
// because the timeline only names files.
func zipIndex(path string) (map[string]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(strings.ReplaceAll(f.Name, `\`, "/"))
		key := strings.ToLower(base)
		if prev, ok := out[key]; ok {
			return nil, fmt.Errorf("%s 在压缩包里出现了两次（%s / %s）", base, prev, f.Name)
		}
		out[key] = strings.ReplaceAll(f.Name, `\`, "/")
	}
	if len(out) == 0 {
		return nil, errors.New("压缩包是空的")
	}
	return out, nil
}

func readZipEntry(path, name string, limit int64) ([]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.ReplaceAll(f.Name, `\`, "/") != name {
			continue
		}
		if int64(f.UncompressedSize64) > limit {
			return nil, errors.New("entry too large")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, limit))
	}
	return nil, os.ErrNotExist
}

// ---- merging ----

// mergeRender puts the pieces back together. A timeline is a byte-concatenation
// of identically-encoded pieces; Blender output is a directory of frames, which
// becomes a video only after every piece is in place.
func (s *Server) mergeRender(j *Job) error {
	if j.Kind == render.KindBlender {
		return s.mergeFrames(j)
	}
	parts := make([]string, len(j.Segs))
	for i, seg := range j.Segs {
		parts[i] = seg.path
	}
	dst := filepath.Join(s.dataDir, "jobs", j.ID, "out.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := ffx.Concat(ctx, parts, dst); err != nil {
		// Pieces come from different machines, so an encoder extradata mismatch is
		// possible even though the command line is identical. Falling back to a
		// re-encode costs a pass but finishes the job the buyer paid for.
		log.Printf("job %s: copy concat failed (%v), retrying with re-encode", j.ID, err)
		if err2 := ffx.ConcatEncode(ctx, parts, dst); err2 != nil {
			return fmt.Errorf("concat: %v / re-encode: %v", err, err2)
		}
	}
	j.out = dst
	return nil
}

func (s *Server) mergeFrames(j *Job) error {
	dir := filepath.Join(s.dataDir, "jobs", j.ID, "frames")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, seg := range j.Segs {
		if err := unzipInto(seg.path, dir); err != nil {
			return fmt.Errorf("解压第 %d 段帧包: %w", seg.Index+1, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	frames := 0
	first := 1 << 30
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".png") {
			continue
		}
		frames++
		var n int
		if _, err := fmt.Sscanf(e.Name(), "frame_%04d.png", &n); err == nil && n < first {
			first = n
		}
	}
	if frames == 0 {
		return errors.New("帧包解开后没有任何 PNG")
	}
	out := filepath.Join(s.dataDir, "jobs", j.ID, "out.mp4")
	fps := 24.0
	if j.Spec != nil && j.Spec.FPS > 0 {
		fps = j.Spec.FPS
	}
	if fps > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := ffx.EncodeFrames(ctx, dir, first, fps, out); err != nil {
			// frames are still a deliverable even when the encode step fails: hand
			// back the zip rather than failing a render that fully succeeded
			log.Printf("job %s: frame encode failed: %v (falling back to zip)", j.ID, err)
			out = ""
		}
	}
	if out != "" {
		j.out = out
		return nil
	}
	zb := filepath.Join(s.dataDir, "jobs", j.ID, "out.zip")
	if err := zipDir(dir, zb); err != nil {
		return err
	}
	j.out = zb
	return nil
}

// unzipInto extracts a slice archive, refusing entries that would escape dst.
// The archive was produced by an agent, which is not a trusted writer.
func unzipInto(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	const (
		maxEntry   = 64 << 20
		maxTotal   = 2 << 30
		maxEntries = 200000
	)
	if len(zr.File) > maxEntries {
		return fmt.Errorf("帧包条目过多")
	}
	var total, written int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(strings.ReplaceAll(f.Name, `\`, "/"))
		target := filepath.Join(dst, name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("压缩包里的路径不安全: %s", f.Name)
		}
		if f.UncompressedSize64 > maxEntry || total+int64(f.UncompressedSize64) > maxTotal {
			return fmt.Errorf("帧包过大")
		}
		total += int64(f.UncompressedSize64)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(w, io.LimitReader(rc, maxEntry))
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
		// count the bytes that actually landed, not the header's claim
		written += n
		if n >= maxEntry || written > maxTotal {
			return fmt.Errorf("帧包过大")
		}
	}
	return nil
}

func zipDir(dir, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		w, err := zw.Create(filepath.Base(p))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
}
