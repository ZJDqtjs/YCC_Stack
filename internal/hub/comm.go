package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"starstack/internal/store"
)

// commDetail renders small event payloads; the timeline UI never parses them
// into typed structs, it just shows the fields next to the event.
func commDetail(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// commLog is the durable record of a protocol step. Buyer, seller and broker
// all write here: who ordered the job, which node was handed it, what the node
// reported and what was settled. A failed placement can then be argued from
// evidence instead of screenshots.
func (s *Server) commLog(e *store.CommEntry) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	if e.SellerID == "" && e.NodeID != "" {
		if owner, err := s.st.NodeOwner(e.NodeID); err == nil {
			e.SellerID = owner
		}
	}
	if err := s.st.AddComm(e); err != nil {
		log.Printf("comm log: %v", err)
	}
}

// handleJobLogs serves the buyer's timeline. A provider that owns a node which
// took part in the job sees the same entries, which is what makes it a shared
// record rather than two private views.
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request, jobID string) {
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, jsonErr("请先登录"), http.StatusUnauthorized)
		return
	}
	ownerID := ""
	if j := s.jobs[jobID]; j != nil {
		ownerID = j.OwnerID
	} else if row, err := s.st.Job(jobID); err == nil {
		ownerID = row.OwnerID
	}
	if ownerID == "" {
		http.NotFound(w, r)
		return
	}
	allowed := ownerID == u.ID || s.st.CommSellerOfJob(jobID, u.ID)
	if !allowed {
		http.Error(w, jsonErr("无权查看该任务的通讯记录"), http.StatusForbidden)
		return
	}
	logs, err := s.st.CommByJob(jobID, 500)
	if err != nil {
		http.Error(w, jsonErr("读取通讯记录失败"), http.StatusInternalServerError)
		return
	}
	role := "seller"
	if ownerID == u.ID {
		role = "buyer"
	}
	writeJSON(w, map[string]any{"job_id": jobID, "role": role, "logs": logs})
}

// handleNodeLogs is the provider-side view: every buyer interaction this node
// participated in, newest last.
func (s *Server) handleNodeLogs(w http.ResponseWriter, r *http.Request, nodeID string) {
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, jsonErr("请先登录"), http.StatusUnauthorized)
		return
	}
	owner, err := s.st.NodeOwner(nodeID)
	if err != nil || owner != u.ID {
		http.Error(w, jsonErr("只能查看自己节点的通讯记录"), http.StatusForbidden)
		return
	}
	logs, err := s.st.CommByNode(nodeID, 500)
	if err != nil {
		http.Error(w, jsonErr("读取通讯记录失败"), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"node_id": nodeID, "role": "seller", "logs": logs})
}

// commNodeSuffix extracts "/api/nodes/<id>/logs" without disturbing the existing
// /api/nodes/<id> and /api/nodes/<id>/pause routes.
func commNodeSuffix(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/nodes/")
	id := strings.TrimSuffix(rest, "/logs")
	if id == rest || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// segComm records a segment-level step. WorkerID is the node that acted, not
// necessarily the owner of the segment's current lease.
func (s *Server) segComm(seg *Seg, workerID, actor, event string, detail map[string]any) {
	buyer := ""
	if j := s.jobs[seg.JobID]; j != nil {
		buyer = j.OwnerID
	}
	s.commLog(&store.CommEntry{JobID: seg.JobID, SegID: seg.ID, NodeID: workerID,
		BuyerID: buyer, Actor: actor, Event: event, Detail: commDetail(detail)})
}
