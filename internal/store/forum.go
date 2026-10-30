package store

import (
	"database/sql"
	"time"
)

// ---- 个人偏好 ----

// Advanced reports whether the account wants the full task form. Absent row
// means "simple", so a fresh account never sees the expert fields.
func (s *Store) Advanced(userID string) bool {
	var v int
	err := s.db.QueryRow(`SELECT advanced FROM user_settings WHERE user_id=?`, userID).Scan(&v)
	return err == nil && v == 1
}

func (s *Store) SetAdvanced(userID string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO user_settings(user_id,advanced) VALUES(?,?)
		ON CONFLICT(user_id) DO UPDATE SET advanced=excluded.advanced`, userID, v)
	return err
}

// ---- 论坛 ----

type ForumPost struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	Author     string `json:"author"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	CreatedAt  int64  `json:"created_at"`
	Comments   int    `json:"comments"`
	MediaCount int    `json:"media_count"`
}

type ForumComment struct {
	ID        string      `json:"id"`
	PostID    string      `json:"post_id"`
	UserID    string      `json:"user_id"`
	Author    string      `json:"author"`
	Body      string      `json:"body"`
	CreatedAt int64       `json:"created_at"`
	Media     []ForumFile `json:"media,omitempty"`
}

type ForumFile struct {
	ID        string `json:"id"`
	PostID    string `json:"post_id,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	Kind      string `json:"kind"` // image | video
	Name      string `json:"name"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
}

const postCols = `p.id,p.user_id,COALESCE(u.name,'已注销'),p.title,p.body,p.created_at,
	(SELECT COUNT(*) FROM forum_comments c WHERE c.post_id=p.id),
	(SELECT COUNT(*) FROM forum_media m WHERE m.post_id=p.id)`

func scanPost(row interface{ Scan(...any) error }) (*ForumPost, error) {
	p := &ForumPost{}
	err := row.Scan(&p.ID, &p.UserID, &p.Author, &p.Title, &p.Body, &p.CreatedAt, &p.Comments, &p.MediaCount)
	return p, err
}

func (s *Store) CreatePost(p *ForumPost) error {
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO forum_posts(id,user_id,title,body,created_at) VALUES(?,?,?,?,?)`,
		p.ID, p.UserID, p.Title, p.Body, p.CreatedAt)
	return err
}

func (s *Store) Posts(limit int) ([]*ForumPost, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+postCols+` FROM forum_posts p
		LEFT JOIN users u ON u.id=p.user_id ORDER BY p.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ForumPost{}
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Post(id string) (*ForumPost, error) {
	row := s.db.QueryRow(`SELECT `+postCols+` FROM forum_posts p
		LEFT JOIN users u ON u.id=p.user_id WHERE p.id=?`, id)
	p, err := scanPost(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *Store) Comments(postID string) ([]*ForumComment, error) {
	rows, err := s.db.Query(`SELECT c.id,c.post_id,c.user_id,COALESCE(u.name,'已注销'),c.body,c.created_at
		FROM forum_comments c LEFT JOIN users u ON u.id=c.user_id
		WHERE c.post_id=? ORDER BY c.created_at`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ForumComment{}
	for rows.Next() {
		c := &ForumComment{}
		if err := rows.Scan(&c.ID, &c.PostID, &c.UserID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		files, err := s.mediaBy(`comment_id=?`, c.ID)
		if err != nil {
			return nil, err
		}
		c.Media = files
	}
	return out, nil
}

func (s *Store) AddComment(c *ForumComment) error {
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO forum_comments(id,post_id,user_id,body,created_at) VALUES(?,?,?,?,?)`,
		c.ID, c.PostID, c.UserID, c.Body, c.CreatedAt)
	return err
}

func (s *Store) AddMedia(m *ForumFile) error {
	_, err := s.db.Exec(`INSERT INTO forum_media(id,post_id,comment_id,kind,name,mime,size,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, m.ID, m.PostID, m.CommentID, m.Kind, m.Name, m.Mime, m.Size, time.Now().Unix())
	return err
}

func (s *Store) mediaBy(where, arg string) ([]ForumFile, error) {
	rows, err := s.db.Query(`SELECT id,post_id,comment_id,kind,name,mime,size FROM forum_media WHERE `+where+`
		ORDER BY created_at`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForumFile{}
	for rows.Next() {
		var f ForumFile
		if err := rows.Scan(&f.ID, &f.PostID, &f.CommentID, &f.Kind, &f.Name, &f.Mime, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PostMedia returns the files attached to the post body itself (comments carry
// their own).
func (s *Store) PostMedia(postID string) ([]ForumFile, error) {
	return s.mediaBy(`post_id=? AND comment_id=''`, postID)
}

func (s *Store) Media(id string) (*ForumFile, error) {
	f := &ForumFile{}
	err := s.db.QueryRow(`SELECT id,post_id,comment_id,kind,name,mime,size FROM forum_media WHERE id=?`, id).
		Scan(&f.ID, &f.PostID, &f.CommentID, &f.Kind, &f.Name, &f.Mime, &f.Size)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}
