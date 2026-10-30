package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"starstack/internal/vm"
	"starstack/internal/vmhost"
)

// vmAgent is the provider-side loop for the cloud-host market. It is separate
// from the transcode claim loop because provisioning takes minutes and must not
// block slice dispatch; both share the same node identity and credential.
type vmAgent struct {
	a    *agent
	host *vmhost.Host
	sem  chan struct{}

	locks  sync.Map // instance id -> *sync.Mutex
	failed sync.Map // instance id whose last start failed; duplicate starts must not resurrect it
	closed sync.Map // instance id already stopped; a late duplicate start must not revive it
}

func newVMAgent(a *agent, host *vmhost.Host, max int) *vmAgent {
	if max <= 0 {
		max = 2
	}
	return &vmAgent{a: a, host: host, sem: make(chan struct{}, max)}
}

func (v *vmAgent) lock(id string) *sync.Mutex {
	m, _ := v.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (v *vmAgent) loop() {
	for {
		cmd := v.claim()
		if cmd == nil {
			continue
		}
		go v.exec(cmd)
	}
}

func (v *vmAgent) claim() *vm.Command {
	u := fmt.Sprintf("%s/api/vm/claim?worker=%s&wait=20", v.a.server, url.QueryEscape(v.a.id))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		time.Sleep(3 * time.Second)
		return nil
	}
	v.a.decorate(req, true)
	resp, err := v.a.hc.Do(req)
	if err != nil {
		log.Printf("vm claim: %v", err)
		time.Sleep(3 * time.Second)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		log.Printf("vm claim error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		time.Sleep(3 * time.Second)
		return nil
	}
	var cmd vm.Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		return nil
	}
	return &cmd
}

func (v *vmAgent) exec(cmd *vm.Command) {
	mu := v.lock(cmd.Instance)
	mu.Lock()
	defer mu.Unlock()

	switch cmd.Op {
	case vm.OpStart:
		if _, done := v.closed.Load(cmd.Instance); done {
			v.report(vm.Report{Instance: cmd.Instance, State: "stopped"})
			return
		}
		if _, bad := v.failed.Load(cmd.Instance); bad {
			v.report(vm.Report{Instance: cmd.Instance, State: "failed",
				Err: "上一次启动已经失败，等待平台回收"})
			return
		}
		if cmd.Plan == nil {
			v.report(vm.Report{Instance: cmd.Instance, State: "failed", Err: "平台下发的启动指令缺少配置"})
			v.failed.Store(cmd.Instance, true)
			return
		}
		// A machine only has so much RAM/disk; the broker already enforces its
		// own count, but a stale duplicate command must not start a third VM.
		v.sem <- struct{}{}
		defer func() { <-v.sem }()
		v.report(vm.Report{Instance: cmd.Instance, State: "provisioning", Pct: 1, Msg: "准备系统镜像与磁盘"})
		last := time.Now().Add(-5 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		ep, err := v.host.Start(ctx, cmd.Plan, func(pct float64, msg string) {
			if time.Since(last) < 2*time.Second || pct >= 100 {
				return
			}
			last = time.Now()
			v.report(vm.Report{Instance: cmd.Instance, State: "provisioning", Pct: pct, Msg: msg})
		})
		if err != nil {
			log.Printf("vm %s start failed: %v", cmd.Instance, err)
			v.failed.Store(cmd.Instance, true)
			// A failed start can leave a partial disk/seed behind. Remove it now
			// and tell the broker whether that cleanup succeeded, so it does not
			// have to guess from the error string.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 60*time.Second)
			stopErr := v.host.Stop(stopCtx, cmd.Instance)
			stopCancel()
			v.report(vm.Report{Instance: cmd.Instance, State: "failed", Err: err.Error(),
				Cleaned: stopErr == nil})
			return
		}
		v.failed.Delete(cmd.Instance)
		log.Printf("vm %s started: ipv6=%s ssh=%s", cmd.Instance, ep.IPv6, ep.SSH)
		v.report(vm.Report{Instance: cmd.Instance, State: "running", Endpoints: ep})
	case vm.OpStop:
		v.report(vm.Report{Instance: cmd.Instance, State: "stopping", Msg: "正在关机"})
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err := v.host.Stop(ctx, cmd.Instance)
		cancel()
		if err != nil {
			log.Printf("vm %s stop: %v", cmd.Instance, err)
			v.report(vm.Report{Instance: cmd.Instance, State: "failed", Err: err.Error()})
			return
		}
		v.failed.Delete(cmd.Instance)
		v.closed.Store(cmd.Instance, true)
		v.report(vm.Report{Instance: cmd.Instance, State: "stopped", Cleaned: true})
	case vm.OpSync:
		ep, running, err := v.host.Status(context.Background(), cmd.Instance)
		switch {
		case err != nil:
			v.report(vm.Report{Instance: cmd.Instance, State: "failed", Err: err.Error()})
		case running:
			v.report(vm.Report{Instance: cmd.Instance, State: "running", Endpoints: ep})
		default:
			stopErr := v.host.Stop(context.Background(), cmd.Instance)
			v.report(vm.Report{Instance: cmd.Instance, State: "stopped", Cleaned: stopErr == nil})
		}
	default:
		log.Printf("vm %s: unknown op %q", cmd.Instance, cmd.Op)
	}
}

func (v *vmAgent) heartbeat() {
	v.hbOnce()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		v.hbOnce()
	}
}

func (v *vmAgent) hbOnce() {
	for _, st := range v.host.List() {
		if st.Plan == nil {
			continue
		}
		id := st.Plan.InstanceID
		mu := v.lock(id)
		if !mu.TryLock() {
			continue // a start/stop is in flight; it will report its own outcome
		}
		ep, running, err := v.host.Status(context.Background(), id)
		mu.Unlock()
		if err != nil {
			continue
		}
		if running {
			v.report(vm.Report{Instance: id, State: "running", Endpoints: ep})
		} else {
			// The VM is gone; clean its disk/state so the provider's disk is not
			// quietly consumed by dead rentals, then tell the broker whether the
			// cleanup actually happened.
			stopErr := v.host.Stop(context.Background(), id)
			v.report(vm.Report{Instance: id, State: "failed",
				Err: "QEMU 进程已经退出，控制台日志在 qemu.log", Cleaned: stopErr == nil})
		}
	}
}

// janitor trims the shared image cache; a machine that hosts a dozen different
// buyers must not keep every base image forever.
func (v *vmAgent) janitor() {
	v.host.CleanupImages()
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for range t.C {
		v.host.CleanupImages()
	}
}

func (v *vmAgent) report(rep vm.Report) {
	rep.Worker = v.a.id
	if _, err := v.a.postJSON(v.a.server+"/api/vm/report", rep, true); err != nil {
		// A 404 means the broker settled or forgot this instance; retrying from
		// the heartbeat is enough, the VM stays under our control either way.
		log.Printf("vm report %s/%s: %v", rep.Instance, rep.State, err)
	}
}
