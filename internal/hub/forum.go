package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"starstack/internal/store"
)

// Community board and the console's simple/advanced preference.
//
// Media lives under <data>/forum/<post>/ and is served back through
// /api/forum/media/<id>: the id comes from SQLite before any path is built, so
// a crafted id cannot walk out of the data directory.

const (
	maxPostImages = 6
	maxImageBytes = 8 << 20
	maxVideoBytes = 64 << 20
	maxTitleChars = 120
	maxBodyChars  = 20000
)

var mediaExt = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"video/mp4": ".mp4", "video/webm": ".webm", "video/quicktime": ".mov",
}

// handleMySettings stores whether this account wants the full task form.
func (s *Server) handleMySettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]bool{"advanced": s.st.Advanced(u.ID)})
	case http.MethodPut, http.MethodPost:
		var body struct {
			Advanced bool `json:"advanced"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
			http.Error(w, jsonErr("请求格式不对"), 400)
			return
		}
		if err := s.st.SetAdvanced(u.ID, body.Advanced); err != nil {
			s.deny(w, 500, "保存设置失败", "save settings", err)
			return
		}
		writeJSON(w, map[string]bool{"advanced": body.Advanced})
	default:
		http.Error(w, "GET/PUT only", 405)
	}
}

func (s *Server) handleForumPosts(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.st.Posts(100)
		if err != nil {
			s.deny(w, 500, "读取帖子失败", "forum list", err)
			return
		}
		writeJSON(w, map[string]any{"posts": list})
	case http.MethodPost:
		s.createForumPost(w, r, u)
	default:
		http.Error(w, "GET/POST only", 405)
	}
}

func (s *Server) createForumPost(w http.ResponseWriter, r *http.Request, u *store.User) {
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoBytes+maxImageBytes*maxPostImages+(4<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.deny(w, 400, "上传失败或超出大小上限", "forum post upload", err)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	if title == "" {
		http.Error(w, jsonErr("标题不能为空"), 400)
		return
	}
	if len([]rune(title)) > maxTitleChars || len([]rune(body)) > maxBodyChars {
		http.Error(w, jsonErr("标题或正文过长"), 400)
		return
	}

	post := &store.ForumPost{ID: newID(), UserID: u.ID, Author: u.Name, Title: title, Body: body, CreatedAt: time.Now().Unix()}
	if err := s.st.CreatePost(post); err != nil {
		s.deny(w, 500, "发帖失败", "create post", err)
		return
	}

	// A file that fails validation is skipped rather than failing the post: the
	// text is already saved, and throwing it away helps nobody.
	images, videos := 0, 0
	for _, fh := range r.MultipartForm.File["files"] {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		id, kind, mime, err := s.saveForumFile(f, post.ID, fh.Size)
		f.Close()
		if err != nil {
			continue
		}
		if (kind == "image" && images >= maxPostImages) || (kind == "video" && videos >= 1) {
			os.Remove(filepath.Join(s.dataDir, "forum", post.ID, id))
			continue
		}
		file := &store.ForumFile{ID: id, PostID: post.ID, Kind: kind, Name: fh.Filename, Mime: mime, Size: fh.Size}
		if err := s.st.AddMedia(file); err != nil {
			os.Remove(filepath.Join(s.dataDir, "forum", post.ID, id))
			continue
		}
		if kind == "image" {
			images++
		} else {
			videos++
		}
	}
	writeJSON(w, map[string]any{"post": post})
}

func (s *Server) handleForumPost(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/forum/posts/")
	id, sub, _ := strings.Cut(rest, "/")
	post, err := s.st.Post(id)
	if err != nil {
		s.deny(w, 500, "读取帖子失败", "forum post", err)
		return
	}
	if post == nil {
		http.Error(w, jsonErr("帖子不存在"), 404)
		return
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		comments, err := s.st.Comments(id)
		if err != nil {
			s.deny(w, 500, "读取评论失败", "forum comments", err)
			return
		}
		media, err := s.st.PostMedia(id)
		if err != nil {
			s.deny(w, 500, "读取附件失败", "forum media", err)
			return
		}
		writeJSON(w, map[string]any{"post": post, "comments": comments, "media": media})
	case sub == "comments" && r.Method == http.MethodPost:
		s.createForumComment(w, r, u, id)
	default:
		http.Error(w, "not found", 404)
	}
}

func (s *Server) createForumComment(w http.ResponseWriter, r *http.Request, u *store.User, postID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.deny(w, 400, "上传失败或超出大小上限", "forum comment upload", err)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	files := r.MultipartForm.File["image"]
	if body == "" && len(files) == 0 {
		http.Error(w, jsonErr("评论内容为空"), 400)
		return
	}
	if len([]rune(body)) > 4000 {
		http.Error(w, jsonErr("评论过长"), 400)
		return
	}
	c := &store.ForumComment{ID: newID(), PostID: postID, UserID: u.ID, Author: u.Name, Body: body, CreatedAt: time.Now().Unix()}
	if err := s.st.AddComment(c); err != nil {
		s.deny(w, 500, "发表评论失败", "create comment", err)
		return
	}
	if len(files) > 0 {
		fh := files[0]
		if f, err := fh.Open(); err == nil {
			id, kind, mime, err := s.saveForumFile(f, postID, fh.Size)
			f.Close()
			if err == nil && kind == "image" {
				file := &store.ForumFile{ID: id, PostID: postID, CommentID: c.ID, Kind: kind, Name: fh.Filename, Mime: mime, Size: fh.Size}
				if err := s.st.AddMedia(file); err == nil {
					c.Media = []store.ForumFile{*file}
				} else {
					os.Remove(filepath.Join(s.dataDir, "forum", postID, id))
				}
			} else if err == nil {
				os.Remove(filepath.Join(s.dataDir, "forum", postID, id))
			}
		}
	}
	writeJSON(w, map[string]any{"comment": c})
}

// handleForumMedia streams an attachment back. Readers must be signed in: the
// board is for accounts, not for the open internet.
func (s *Server) handleForumMedia(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/forum/media/")
	f, err := s.st.Media(id)
	if err != nil {
		s.deny(w, 500, "读取附件失败", "forum media lookup", err)
		return
	}
	if f == nil {
		http.Error(w, "not found", 404)
		return
	}
	fh, err := os.Open(filepath.Join(s.dataDir, "forum", f.PostID, f.ID))
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", f.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Disposition", `inline; filename="attachment"`)
	http.ServeContent(w, r, f.Name, st.ModTime(), fh)
}

// saveForumFile sniffs the payload, rejects anything that is not a still image
// or a common video container, and writes it under the post's directory. The
// returned id is the file name on disk; callers persist it so reads never
// depend on a client-supplied name.
func (s *Server) saveForumFile(src io.Reader, postID string, size int64) (id, kind, mime string, err error) {
	head := make([]byte, 512)
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", "", "", err
	}
	head = head[:n]
	mime = http.DetectContentType(head)
	ext, ok := mediaExt[mime]
	if !ok {
		return "", "", "", errors.New("unsupported media")
	}
	kind = "video"
	if strings.HasPrefix(mime, "image/") {
		kind = "image"
	}
	if (kind == "image" && size > maxImageBytes) || (kind == "video" && size > maxVideoBytes) {
		return "", "", "", errors.New("media too large")
	}

	dir := filepath.Join(s.dataDir, "forum", postID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	id = newID() + ext
	out, err := os.Create(filepath.Join(dir, id))
	if err != nil {
		return "", "", "", err
	}
	_, err = io.Copy(out, io.MultiReader(strings.NewReader(string(head)), io.LimitReader(src, size)))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(filepath.Join(dir, id))
		return "", "", "", err
	}
	return id, kind, mime, nil
}
