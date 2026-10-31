package store

import (
	"database/sql"
	"errors"
)

// CommEntry is one protocol event between buyer, platform and provider. It is
// the durable record both sides can point at when a job "went missing": who
// asked for it, which node took it, what it reported and what was settled.
type CommEntry struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	JobID    string `json:"job_id,omitempty"`
	SegID    string `json:"seg_id,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	BuyerID  string `json:"buyer_id,omitempty"`
	SellerID string `json:"seller_id,omitempty"`
	Actor    string `json:"actor"` // buyer | seller | server
	Event    string `json:"event"`
	Detail   string `json:"detail,omitempty"`
}

func (s *Store) AddComm(e *CommEntry) error {
	_, err := s.db.Exec(`INSERT INTO comm_logs(ts,job_id,seg_id,node_id,buyer_id,seller_id,actor,event,detail)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		e.TS, e.JobID, e.SegID, e.NodeID, e.BuyerID, e.SellerID, e.Actor, e.Event, e.Detail)
	return err
}

func (s *Store) commQuery(q string, arg string, limit int) ([]*CommEntry, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT ?`, arg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CommEntry
	for rows.Next() {
		e := &CommEntry{}
		if err := rows.Scan(&e.ID, &e.TS, &e.JobID, &e.SegID, &e.NodeID, &e.BuyerID, &e.SellerID, &e.Actor, &e.Event, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Newest first for the LIMIT, oldest first for a timeline.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) CommByJob(jobID string, limit int) ([]*CommEntry, error) {
	if jobID == "" {
		return nil, ErrNotFound
	}
	return s.commQuery(`SELECT id,ts,job_id,seg_id,node_id,buyer_id,seller_id,actor,event,detail
		FROM comm_logs WHERE job_id=?`, jobID, limit)
}

func (s *Store) CommByNode(nodeID string, limit int) ([]*CommEntry, error) {
	if nodeID == "" {
		return nil, ErrNotFound
	}
	return s.commQuery(`SELECT id,ts,job_id,seg_id,node_id,buyer_id,seller_id,actor,event,detail
		FROM comm_logs WHERE node_id=?`, nodeID, limit)
}

// CommSellerOfJob reports whether userID owns a node that took part in jobID.
// It lets the seller read the same timeline as the buyer without exposing every
// job on the platform.
func (s *Store) CommSellerOfJob(jobID, userID string) bool {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM comm_logs WHERE job_id=? AND seller_id=?`, jobID, userID).Scan(&n)
	return errors.Is(err, nil) && n > 0
}

var _ = sql.ErrNoRows
