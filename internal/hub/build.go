package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"starstack/internal/render"
	"starstack/internal/store"
)

// handleCreateBuild queues a compile/packaging job: the buyer uploads a source
// archive, one provider machine with the build runtime unpacks it, runs the
// command and returns the output tree as a zip.
//
// Why one slice: unlike frames or timeline ranges, a build has a dependency
// graph; splitting it by file is a research problem of its own. The scheduler
// still does what it is good at — matching constraints, pricing, holding and
// settling — it simply has one indivisible unit to place.
func (s *Server) handleCreateBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, jsonErr("请先登录后再提交任务"), 401)
		return
	}
	if !s.policy.buildAllowed(u.Name) {
		http.Error(w, jsonErr("本部署未向该账号开放编译任务"), http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.policy.MaxUploadBytes)
	if err := r.ParseMultipartForm(s.policy.MemoryBuffer); err != nil {
		s.deny(w, 400, "上传失败或超出大小上限", "build upload", err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	form := r.Form

	command := strings.TrimSpace(form.Get("command"))
	if command == "" {
		http.Error(w, jsonErr("请填写编译命令"), 400)
		return
	}
	if len(command) > 4000 {
		http.Error(w, jsonErr("编译命令过长"), 400)
		return
	}
	output := strings.TrimSpace(form.Get("output"))
	timeout := atoi(first(form, "timeout", "1800"))
	if timeout < 60 {
		timeout = 60
	}
	if timeout > 3600 {
		timeout = 3600
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
	// The provider only sees this job because its agent advertised "build";
	// keeping the requirement explicit also stops a transcoding-only node from
	// being scored for it.
	req.Runtime = "build"

	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, jsonErr("请上传源码 zip"), 400)
		return
	}
	defer f.Close()

	id := newID()
	dir := filepath.Join(s.dataDir, "jobs", id)
	if err := s.diskOk(dir); err != nil {
		s.deny(w, 507, "Broker 磁盘空间不足，请稍后重试", "disk", err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.deny(w, 500, "写入失败", "build mkdir", err)
		return
	}
	srcPath := filepath.Join(dir, "source.zip")
	out, err := os.Create(srcPath)
	if err != nil {
		s.deny(w, 500, "写入失败", "build create", err)
		return
	}
	n, copyErr := io.Copy(out, io.LimitReader(f, s.policy.MaxUploadBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n == 0 {
		os.RemoveAll(dir)
		http.Error(w, jsonErr("源码上传失败"), 400)
		return
	}
	if n > s.policy.MaxUploadBytes {
		os.RemoveAll(dir)
		http.Error(w, jsonErr("源码包超出大小上限"), http.StatusRequestEntityTooLarge)
		return
	}

	name := strings.TrimSpace(form.Get("name"))
	if name == "" {
		name = hdr.Filename
	}
	job := &Job{
		ID: id, OwnerID: u.ID, Name: name, Kind: render.KindBuild,
		Spec: &render.Spec{Kind: render.KindBuild, Command: command, Output: output,
			TimeoutSec: timeout, Bundle: true},
		Req: req, Status: "queued", CreatedAt: time.Now(), path: srcPath,
		Profile: Profile{Segs: 1},
		// Ten minutes of wall clock is the pricing estimate, not the ceiling;
		// deadlineLocked() uses TimeoutSec for the real limit.
		Duration: 10,
	}
	job.Segs = []*Seg{{ID: id + "-0", JobID: id, Index: 0, Status: "pending"}}

	s.mu.Lock()
	if live := s.countLiveJobsLocked(u.ID); live >= s.policy.MaxActiveJobs {
		s.mu.Unlock()
		os.RemoveAll(dir)
		http.Error(w, jsonErr(fmt.Sprintf("你有 %d 个任务在跑或排队，单账户上限 %d 个",
			live, s.policy.MaxActiveJobs)), http.StatusTooManyRequests)
		return
	}
	hold, holdPrice, basis := s.estimateHoldLocked(job)
	job.Hold = hold
	if n := s.eligibleLocked(req, ""); n == 0 {
		job.Note = "编译任务；当前没有开启 --allow-build 的在线节点，任务将排队等待"
	} else {
		job.Note = fmt.Sprintf("编译任务；共 %d 台可编译节点；预扣依据：%s × 10 分钟 %.2f 点/分钟",
			n, basis, holdPrice)
	}
	if u.Credits < hold {
		s.mu.Unlock()
		os.RemoveAll(dir)
		http.Error(w, jsonErr(fmt.Sprintf("算力点不足：本次任务预扣 %.2f 点，余额 %.2f 点",
			hold, u.Credits)), http.StatusPaymentRequired)
		return
	}
	if _, err := s.chargeJob(job, hold, store.Hold, "编译任务预扣（按实际墙钟分钟结算，差额退还）"); err != nil {
		s.mu.Unlock()
		os.RemoveAll(dir)
		http.Error(w, jsonErr("预扣失败"), 500)
		return
	}
	if err := s.st.InsertJob(jobRowOf(job), segRowsOf(job)); err != nil {
		s.mu.Unlock()
		os.RemoveAll(dir)
		http.Error(w, jsonErr("任务登记失败"), 500)
		return
	}
	s.jobs[id] = job
	s.order = append(s.order, id)
	s.mu.Unlock()
	s.commLog(&store.CommEntry{JobID: id, BuyerID: u.ID, Actor: "buyer", Event: "job.created",
		Detail: commDetail(map[string]any{"kind": "build", "name": name, "hold": hold,
			"timeout": timeout, "command": command})})
	log.Printf("build job %s queued by %s: %.1f MiB source, timeout %ds, hold=%.3f points",
		id, u.Name, float64(n)/(1<<20), timeout, hold)
	writeJSON(w, map[string]any{"ok": true, "id": id, "kind": render.KindBuild,
		"hold": hold, "note": job.Note, "unit": "分钟", "timeout": timeout})
}
