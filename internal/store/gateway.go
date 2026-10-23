package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// ---- API keys ----

// APIKey is a buyer credential for the OpenAI-compatible gateway. Only the
// SHA-256 hash is stored: the plaintext key is shown once at creation and a
// database copy must never be enough to call the API.
type APIKey struct {
	ID, UserID, Name string
	Hash, Prefix     string
	CreatedAt        int64
	LastUsed         int64
	Revoked          bool
}

func (s *Store) CreateAPIKey(k *APIKey) error {
	_, err := s.db.Exec(`INSERT INTO api_keys(id,user_id,name,hash,prefix,created_at) VALUES(?,?,?,?,?,?)`,
		k.ID, k.UserID, k.Name, k.Hash, k.Prefix, k.CreatedAt)
	return err
}

func (s *Store) APIKeyByHash(hash string) (*APIKey, error) {
	k := &APIKey{}
	var revoked int
	err := s.db.QueryRow(`SELECT id,user_id,name,hash,prefix,created_at,last_used,revoked
		FROM api_keys WHERE hash=?`, hash).
		Scan(&k.ID, &k.UserID, &k.Name, &k.Hash, &k.Prefix, &k.CreatedAt, &k.LastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	k.Revoked = revoked != 0
	return k, err
}

func (s *Store) APIKeysByUser(userID string) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT id,user_id,name,hash,prefix,created_at,last_used,revoked
		FROM api_keys WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k := &APIKey{}
		var revoked int
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Hash, &k.Prefix,
			&k.CreatedAt, &k.LastUsed, &revoked); err != nil {
			return nil, err
		}
		k.Revoked = revoked != 0
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) CountAPIKeys(userID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=? AND revoked=0`, userID).Scan(&n)
	return n, err
}

// RevokeAPIKey returns false when the key does not exist or belongs to someone else.
func (s *Store) RevokeAPIKey(id, userID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked=1 WHERE id=? AND user_id=? AND revoked=0`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TouchAPIKey records last use, but at most once a minute per key so a busy
// gateway does not turn every completion into an extra SQLite write.
func (s *Store) TouchAPIKey(id string) error {
	_, err := s.db.Exec(`UPDATE api_keys SET last_used=? WHERE id=? AND last_used<?`,
		now(), id, now()-60)
	return err
}

// ---- model upstreams ----

// Upstream is one OpenAI-compatible endpoint a provider contributed to the
// model market. EncKey is the provider's upstream credential, sealed with the
// broker's local gateway key; Models is the list of model ids it serves and
// Price is what the provider charges per 1000 tokens.
type Upstream struct {
	ID, OwnerID, Name, BaseURL string
	EncKey                     string
	Models                     []string
	Price                      float64
	Enabled                    bool
	CreatedAt                  int64
}

func (s *Store) CreateUpstream(u *Upstream) error {
	b, err := json.Marshal(u.Models)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO upstreams(id,owner_id,name,base_url,api_key_enc,models,price,enabled,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, u.ID, u.OwnerID, u.Name, u.BaseURL, u.EncKey, string(b),
		u.Price, b2i(u.Enabled), u.CreatedAt)
	return err
}

func (s *Store) Upstreams() ([]*Upstream, error) {
	return s.queryUpstreams(`SELECT id,owner_id,name,base_url,api_key_enc,models,price,enabled,created_at
		FROM upstreams WHERE enabled=1 ORDER BY price, created_at`)
}

func (s *Store) UpstreamsByOwner(ownerID string) ([]*Upstream, error) {
	return s.queryUpstreams(`SELECT id,owner_id,name,base_url,api_key_enc,models,price,enabled,created_at
		FROM upstreams WHERE owner_id=? ORDER BY created_at DESC`, ownerID)
}

func (s *Store) Upstream(id string) (*Upstream, error) {
	rows, err := s.queryUpstreams(`SELECT id,owner_id,name,base_url,api_key_enc,models,price,enabled,created_at
		FROM upstreams WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return rows[0], nil
}

func (s *Store) queryUpstreams(q string, args ...any) ([]*Upstream, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Upstream
	for rows.Next() {
		u := &Upstream{}
		var models string
		var enabled int
		if err := rows.Scan(&u.ID, &u.OwnerID, &u.Name, &u.BaseURL, &u.EncKey, &models,
			&u.Price, &enabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(models), &u.Models)
		u.Enabled = enabled != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUpstream returns false when the row is not owned by this account.
func (s *Store) DeleteUpstream(id, ownerID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM upstreams WHERE id=? AND owner_id=?`, id, ownerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetUpstreamEnabled lets a provider pause routing to its endpoint without
// losing its model list, price or credential.
func (s *Store) SetUpstreamEnabled(id, ownerID string, enabled bool) (bool, error) {
	res, err := s.db.Exec(`UPDATE upstreams SET enabled=? WHERE id=? AND owner_id=?`,
		b2i(enabled), id, ownerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
