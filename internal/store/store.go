package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS users(
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  pass_hash   TEXT NOT NULL,
  salt        TEXT NOT NULL,
  iter        INTEGER NOT NULL,
  credits     REAL NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS sessions(
  token       TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  ua          TEXT);

CREATE TABLE IF NOT EXISTS nodes(
  id             TEXT PRIMARY KEY,
  owner_id       TEXT,
  name           TEXT,
  hostname       TEXT,
  os             TEXT,
  arch           TEXT,
  cpus           INTEGER,
  mem_mb         INTEGER,
  disk_free_mb   INTEGER,
  gpus           TEXT,
  encoders       TEXT,
  runtimes       TEXT,
  price          REAL NOT NULL DEFAULT 1,
  slots          INTEGER,
  speed          REAL,
  ok             INTEGER,
  bad            INTEGER,
  seconds_billed REAL NOT NULL DEFAULT 0,
  earned         REAL NOT NULL DEFAULT 0,
  paused         INTEGER NOT NULL DEFAULT 0,
  registered_at  INTEGER NOT NULL,
  last_seen      INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS jobs(
  id            TEXT PRIMARY KEY,
  owner_id      TEXT,
  name          TEXT,
  src_path      TEXT,
  duration      REAL,
  profile       TEXT,
  req           TEXT,
  kind          TEXT NOT NULL DEFAULT 'transcode',
  plan          TEXT NOT NULL DEFAULT '',
  status        TEXT,
  credits_hold  REAL,
  credits_spent REAL,
  note          TEXT,
  err           TEXT,
  created_at    INTEGER,
  started_at    INTEGER,
  finished_at   INTEGER,
  wall          REAL,
  sum_wall      REAL,
  baseline_wall REAL);

CREATE TABLE IF NOT EXISTS segments(
  id        TEXT PRIMARY KEY,
  job_id    TEXT NOT NULL,
  idx       INTEGER,
  start     REAL,
  dur       REAL,
  status    TEXT,
  node_id   TEXT,
  attempt   INTEGER,
  pct       REAL,
  speed     REAL,
  wall      REAL,
  bytes     INTEGER,
  sha       TEXT,
  srv_sha   TEXT,
  verified  INTEGER,
  err       TEXT,
  cost      REAL,
  price     REAL NOT NULL DEFAULT 0,
  excluded  TEXT NOT NULL DEFAULT '',
  path      TEXT);

CREATE TABLE IF NOT EXISTS ledger(
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  ts        INTEGER NOT NULL,
  kind      TEXT NOT NULL,
  user_id   TEXT,
  node_id   TEXT,
  job_id    TEXT,
  amount    REAL NOT NULL,
  balance   REAL NOT NULL,
  note      TEXT);

CREATE TABLE IF NOT EXISTS api_keys(
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  name       TEXT NOT NULL,
  hash       TEXT NOT NULL UNIQUE,
  prefix     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used  INTEGER NOT NULL DEFAULT 0,
  revoked    INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS upstreams(
  id          TEXT PRIMARY KEY,
  owner_id    TEXT NOT NULL,
  name        TEXT NOT NULL,
  base_url    TEXT NOT NULL,
  api_key_enc TEXT NOT NULL DEFAULT '',
  models      TEXT NOT NULL,
  price       REAL NOT NULL DEFAULT 0,
  max_context INTEGER NOT NULL DEFAULT 32768,
  caps        TEXT NOT NULL DEFAULT '["chat"]',
  price_out   REAL NOT NULL DEFAULT 0,
  price_cached REAL NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL);

CREATE INDEX IF NOT EXISTS idx_seg_job ON segments(job_id);
CREATE INDEX IF NOT EXISTS idx_led_user ON ledger(user_id, ts);
CREATE INDEX IF NOT EXISTS idx_led_node ON ledger(node_id, ts);
CREATE INDEX IF NOT EXISTS idx_sess_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_key_user ON api_keys(user_id);
CREATE INDEX IF NOT EXISTS idx_up_owner ON upstreams(owner_id);
`

// Column additions for databases written by an earlier build. SQLite has no
// "add if missing", so a duplicate column is the expected error and is ignored.
const columnMigrations = `
ALTER TABLE segments ADD COLUMN price REAL NOT NULL DEFAULT 0;
ALTER TABLE segments ADD COLUMN excluded TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'transcode';
ALTER TABLE jobs ADD COLUMN plan TEXT NOT NULL DEFAULT '';
ALTER TABLE upstreams ADD COLUMN max_context INTEGER NOT NULL DEFAULT 32768;
ALTER TABLE upstreams ADD COLUMN caps TEXT NOT NULL DEFAULT '["chat"]';
ALTER TABLE upstreams ADD COLUMN price_out REAL NOT NULL DEFAULT 0;
ALTER TABLE upstreams ADD COLUMN price_cached REAL NOT NULL DEFAULT 0;
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One writer at a time: the broker is single-process and this avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	runColumnMigrations(db)
	return &Store{db: db}, nil
}

// runColumnMigrations applies ALTERs an older database does not have yet.
// Duplicate-column errors are the normal outcome on a fresh file and skipped;
// anything else is worth knowing about, so it reaches the log.
func runColumnMigrations(db *sql.DB) {
	for _, stmt := range strings.Split(columnMigrations, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			log.Printf("store: %v", err)
		}
	}
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func enc(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func dec(raw sql.NullString, v any) {
	if raw.Valid && raw.String != "" {
		json.Unmarshal([]byte(raw.String), v)
	}
}

// ---- users ----

type User struct {
	ID, Name  string
	PassHash  string `json:"-"`
	Salt      string `json:"-"`
	Iter      int    `json:"-"`
	Credits   float64
	CreatedAt int64
}

func (s *Store) CreateUser(u *User) error {
	_, err := s.db.Exec(`INSERT INTO users(id,name,pass_hash,salt,iter,credits,created_at)
		VALUES(?,?,?,?,?,?,?)`, u.ID, u.Name, u.PassHash, u.Salt, u.Iter, u.Credits, u.CreatedAt)
	return err
}

func (s *Store) UserByName(name string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id,name,pass_hash,salt,iter,credits,created_at FROM users WHERE name=?`, name).
		Scan(&u.ID, &u.Name, &u.PassHash, &u.Salt, &u.Iter, &u.Credits, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) User(id string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id,name,pass_hash,salt,iter,credits,created_at FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Name, &u.PassHash, &u.Salt, &u.Iter, &u.Credits, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// AddCredits applies a signed delta and returns the new balance.
func (s *Store) AddCredits(userID string, delta float64) (float64, error) {
	var bal float64
	err := s.db.QueryRow(`UPDATE users SET credits = credits + ? WHERE id=? RETURNING credits`, delta, userID).Scan(&bal)
	return round4(bal), err
}

func (s *Store) Balance(userID string) (float64, error) {
	var bal float64
	err := s.db.QueryRow(`SELECT credits FROM users WHERE id=?`, userID).Scan(&bal)
	return round4(bal), err
}

// ---- sessions ----

func (s *Store) PutSession(token, userID string, ttl time.Duration, ua string) error {
	_, err := s.db.Exec(`INSERT INTO sessions(token,user_id,created_at,expires_at,ua) VALUES(?,?,?,?,?)
		ON CONFLICT(token) DO UPDATE SET created_at=excluded.created_at, expires_at=excluded.expires_at`,
		token, userID, now(), time.Now().Add(ttl).Unix(), ua)
	return err
}

func (s *Store) Session(token string) (string, error) {
	var uid string
	var exp int64
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token=?`, token).Scan(&uid, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if exp < time.Now().Unix() {
		s.DropSession(token)
		return "", ErrNotFound
	}
	return uid, nil
}

func (s *Store) DropSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}

// ---- nodes ----

type GPU struct {
	Name  string `json:"name"`
	MemMB int    `json:"mem_mb"`
}

type Runtime struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type NodeRow struct {
	ID, OwnerID    string
	Name, Hostname string
	OS, Arch       string
	CPUs           int
	MemMB          int
	DiskFreeMB     int64
	GPUs           []GPU
	Encoders       []string
	Runtimes       []Runtime
	Price          float64
	Slots          int
	Speed          float64
	OK, Bad        int
	SecondsBilled  float64
	Earned         float64
	// Paused is the owner's own switch: the machine stays online and visible,
	// it just stops being promised new slices.
	Paused       bool
	RegisteredAt int64
	LastSeen     int64
}

// RegisterNode upserts the hardware/software inventory while preserving
// ownership and lifetime totals across agent restarts.
func (s *Store) RegisterNode(n *NodeRow) error {
	_, err := s.db.Exec(`INSERT INTO nodes(id,owner_id,name,hostname,os,arch,cpus,mem_mb,disk_free_mb,
			gpus,encoders,runtimes,price,slots,speed,ok,bad,registered_at,last_seen)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET owner_id=COALESCE(excluded.owner_id, nodes.owner_id),
			name=excluded.name, hostname=excluded.hostname, os=excluded.os, arch=excluded.arch,
			cpus=excluded.cpus, mem_mb=excluded.mem_mb, disk_free_mb=excluded.disk_free_mb,
			gpus=excluded.gpus, encoders=excluded.encoders, runtimes=excluded.runtimes,
			price=nodes.price, slots=excluded.slots, last_seen=excluded.last_seen`,
		n.ID, nullStr(n.OwnerID), n.Name, n.Hostname, n.OS, n.Arch, n.CPUs, n.MemMB, n.DiskFreeMB,
		enc(n.GPUs), enc(n.Encoders), enc(n.Runtimes), n.Price, n.Slots, n.Speed, n.OK, n.Bad,
		n.RegisteredAt, n.LastSeen)
	return err
}

func (s *Store) SetNodeOwner(nodeID, ownerID string) error {
	_, err := s.db.Exec(`UPDATE nodes SET owner_id=? WHERE id=?`, nullStr(ownerID), nodeID)
	return err
}

func (s *Store) SetNodePrice(nodeID string, price float64) error {
	_, err := s.db.Exec(`UPDATE nodes SET price=? WHERE id=?`, price, nodeID)
	return err
}

func (s *Store) SetNodePaused(nodeID string, paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE nodes SET paused=? WHERE id=?`, v, nodeID)
	return err
}

func (s *Store) NodeOwner(nodeID string) (string, error) {
	var owner sql.NullString
	err := s.db.QueryRow(`SELECT owner_id FROM nodes WHERE id=?`, nodeID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner.String, err
}

func (s *Store) TouchNode(id string, speed float64, ok, bad int) error {
	_, err := s.db.Exec(`UPDATE nodes SET last_seen=?, speed=?, ok=?, bad=? WHERE id=?`, now(), speed, ok, bad, id)
	return err
}

func (s *Store) Nodes() ([]*NodeRow, error) {
	rows, err := s.db.Query(`SELECT id,owner_id,name,hostname,os,arch,cpus,mem_mb,disk_free_mb,gpus,encoders,
		runtimes,price,slots,speed,ok,bad,seconds_billed,earned,paused,registered_at,last_seen FROM nodes ORDER BY registered_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NodeRow
	for rows.Next() {
		n := &NodeRow{}
		var owner, g, e, r sql.NullString
		var paused int
		if err := rows.Scan(&n.ID, &owner, &n.Name, &n.Hostname, &n.OS, &n.Arch, &n.CPUs, &n.MemMB,
			&n.DiskFreeMB, &g, &e, &r, &n.Price, &n.Slots, &n.Speed, &n.OK, &n.Bad, &n.SecondsBilled,
			&n.Earned, &paused, &n.RegisteredAt, &n.LastSeen); err != nil {
			return nil, err
		}
		n.OwnerID = owner.String
		n.Paused = paused != 0
		dec(g, &n.GPUs)
		dec(e, &n.Encoders)
		dec(r, &n.Runtimes)
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) NodesByOwner(ownerID string) ([]*NodeRow, error) {
	all, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	var out []*NodeRow
	for _, n := range all {
		if n.OwnerID == ownerID {
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *Store) AddNodeEarnings(nodeID string, secs, amount float64) error {
	_, err := s.db.Exec(`UPDATE nodes SET seconds_billed = seconds_billed + ?, earned = earned + ? WHERE id=?`,
		secs, amount, nodeID)
	return err
}

// ---- jobs & segments ----

type SegRow struct {
	ID, JobID        string
	Idx              int
	Start, Dur       float64
	Status, NodeID   string
	Attempt          int
	Pct, Speed, Wall float64
	Bytes            int64
	SHA, SrvSHA, Err string
	Path             string
	Verified         bool
	Cost             float64
	Price            float64 // rate quoted when the slice was handed out
	Excluded         string  // JSON array of nodes that already lost this slice
}

type JobRow struct {
	ID, OwnerID, Name, SrcPath  string
	Duration                    float64
	Profile, Req                string
	Kind, Plan                  string
	Status                      string
	CreditsHold, CreditsSpent   float64
	Note, Err                   string
	CreatedAt, StartedAt        int64
	FinishedAt                  int64
	Wall, SumWall, BaselineWall float64
	Segs                        []*SegRow
}

func (s *Store) InsertJob(j *JobRow, segs []*SegRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO jobs(id,owner_id,name,src_path,duration,profile,req,kind,plan,status,
			credits_hold,credits_spent,note,err,created_at,started_at,finished_at,wall,sum_wall,baseline_wall)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, nullStr(j.OwnerID), j.Name, j.SrcPath, j.Duration, j.Profile, j.Req, j.Kind, j.Plan, j.Status,
		j.CreditsHold, j.CreditsSpent, j.Note, j.Err, j.CreatedAt, j.StartedAt, j.FinishedAt,
		j.Wall, j.SumWall, j.BaselineWall); err != nil {
		return err
	}
	for _, sg := range segs {
		if _, err := tx.Exec(`INSERT INTO segments(id,job_id,idx,start,dur,status,node_id,attempt,pct,speed,wall,
				bytes,sha,srv_sha,verified,err,cost,price,excluded,path) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sg.ID, sg.JobID, sg.Idx, sg.Start, sg.Dur, sg.Status, nullStr(sg.NodeID), sg.Attempt,
			sg.Pct, sg.Speed, sg.Wall, sg.Bytes, sg.SHA, sg.SrvSHA, b2i(sg.Verified), sg.Err, sg.Cost,
			sg.Price, sg.Excluded, nullStr(sg.Path)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateJob(j *JobRow) error {
	_, err := s.db.Exec(`UPDATE jobs SET status=?, credits_hold=?, credits_spent=?, note=?, err=?,
			started_at=?, finished_at=?, wall=?, sum_wall=?, baseline_wall=? WHERE id=?`,
		j.Status, j.CreditsHold, j.CreditsSpent, j.Note, j.Err, j.StartedAt, j.FinishedAt,
		j.Wall, j.SumWall, j.BaselineWall, j.ID)
	return err
}

func (s *Store) UpdateSegment(g *SegRow) error {
	_, err := s.db.Exec(`UPDATE segments SET status=?, node_id=?, attempt=?, pct=?, speed=?, wall=?,
			bytes=?, sha=?, srv_sha=?, verified=?, err=?, cost=?, price=?, excluded=?, path=? WHERE id=?`,
		g.Status, nullStr(g.NodeID), g.Attempt, g.Pct, g.Speed, g.Wall, g.Bytes, g.SHA, g.SrvSHA,
		b2i(g.Verified), g.Err, g.Cost, g.Price, g.Excluded, nullStr(g.Path), g.ID)
	return err
}

func (s *Store) Job(id string) (*JobRow, error) {
	rows, err := s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return rows[0], nil
}

const jobCols = `id,owner_id,name,src_path,duration,profile,req,kind,plan,status,credits_hold,credits_spent,
	note,err,created_at,started_at,finished_at,wall,sum_wall,baseline_wall`

func (s *Store) queryJobs(q string, args ...any) ([]*JobRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JobRow
	for rows.Next() {
		j := &JobRow{}
		var owner sql.NullString
		if err := rows.Scan(&j.ID, &owner, &j.Name, &j.SrcPath, &j.Duration, &j.Profile, &j.Req,
			&j.Kind, &j.Plan, &j.Status, &j.CreditsHold, &j.CreditsSpent, &j.Note, &j.Err,
			&j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.Wall, &j.SumWall, &j.BaselineWall); err != nil {
			return nil, err
		}
		j.OwnerID = owner.String
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) JobsByOwner(ownerID string) ([]*JobRow, error) {
	jobs, err := s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE owner_id=? ORDER BY created_at DESC LIMIT 200`, ownerID)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if err := s.loadSegs(j); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// LoadResumable returns jobs that were still in flight when the broker last shut down.
func (s *Store) LoadResumable() ([]*JobRow, error) {
	jobs, err := s.queryJobs(`SELECT ` + jobCols + ` FROM jobs
		WHERE status NOT IN ('done','failed') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if err := s.loadSegs(j); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

func (s *Store) loadSegs(j *JobRow) error {
	rows, err := s.db.Query(`SELECT id,job_id,idx,start,dur,status,node_id,attempt,pct,speed,wall,
		bytes,sha,srv_sha,verified,err,cost,price,excluded,path FROM segments WHERE job_id=? ORDER BY idx`, j.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		g := &SegRow{}
		var node, sha, srv, errS, excl, path sql.NullString
		var v int
		if err := rows.Scan(&g.ID, &g.JobID, &g.Idx, &g.Start, &g.Dur, &g.Status, &node, &g.Attempt,
			&g.Pct, &g.Speed, &g.Wall, &g.Bytes, &sha, &srv, &v, &errS, &g.Cost, &g.Price, &excl, &path); err != nil {
			return err
		}
		g.NodeID, g.SHA, g.SrvSHA, g.Err, g.Path = node.String, sha.String, srv.String, errS.String, path.String
		g.Excluded = excl.String
		g.Verified = v == 1
		j.Segs = append(j.Segs, g)
	}
	return rows.Err()
}

// ---- ledger ----

const (
	Hold     = "hold"
	Settle   = "settle"
	Refund   = "refund"
	Earn     = "earn"
	Recharge = "recharge"
)

type Entry struct {
	ID      int64   `json:"id"`
	TS      int64   `json:"ts"`
	Kind    string  `json:"kind"`
	UserID  string  `json:"user_id,omitempty"`
	NodeID  string  `json:"node_id,omitempty"`
	JobID   string  `json:"job_id,omitempty"`
	Amount  float64 `json:"amount"`
	Balance float64 `json:"balance"`
	Note    string  `json:"note,omitempty"`
}

func (s *Store) Ledger(e *Entry) error {
	_, err := s.db.Exec(`INSERT INTO ledger(ts,kind,user_id,node_id,job_id,amount,balance,note)
		VALUES(?,?,?,?,?,?,?,?)`, e.TS, e.Kind, nullStr(e.UserID), nullStr(e.NodeID), nullStr(e.JobID),
		e.Amount, e.Balance, e.Note)
	return err
}

func (s *Store) LedgerForUser(userID string, limit int) ([]*Entry, error) {
	return s.queryLedger(`SELECT id,ts,kind,user_id,node_id,job_id,amount,balance,note FROM ledger
		WHERE user_id=? ORDER BY ts DESC, id DESC LIMIT ?`, userID, limit)
}

func (s *Store) LedgerForNodes(nodeIDs []string, limit int) ([]*Entry, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	q := `SELECT id,ts,kind,user_id,node_id,job_id,amount,balance,note FROM ledger WHERE node_id IN (`
	args := make([]any, 0, len(nodeIDs)+1)
	for i, id := range nodeIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += `) ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	return s.queryLedger(q, args...)
}

func (s *Store) queryLedger(q string, args ...any) ([]*Entry, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e := &Entry{}
		var u, n, j sql.NullString
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &u, &n, &j, &e.Amount, &e.Balance, &e.Note); err != nil {
			return nil, err
		}
		e.UserID, e.NodeID, e.JobID = u.String, n.String, j.String
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}
