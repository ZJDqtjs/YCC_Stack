package hub

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

// png returns the smallest valid PNG header so http.DetectContentType agrees
// with the extension the test uses.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func multipartBody(t *testing.T, fields map[string]string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="`+name+`"; filename="shot.png"`)
		h.Set("Content-Type", "image/png")
		fw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(data)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestForumPostCommentAndMedia(t *testing.T) {
	s := newTestServer(t, Policy{})
	alice := asUser(t, s, "alice")
	bob := asUser(t, s, "bob")

	// 发帖带一张图
	body, ct := multipartBody(t, map[string]string{"title": "第一次跑通分片", "body": "分享一下参数"},
		map[string][]byte{"files": pngBytes})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/forum/posts", body)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(alice)
	s.handleForumPosts(rec, r)
	if rec.Code != 200 {
		t.Fatalf("create post: %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ Post struct{ ID string } }
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Post.ID == "" {
		t.Fatal("no post id")
	}

	// 列表里能看到作者与附件数
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/forum/posts", nil)
	r.AddCookie(bob)
	s.handleForumPosts(rec, r)
	var list struct{ Posts []struct{ ID string `json:"id"`; Author string `json:"author"`; MediaCount int `json:"media_count"` } }
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Posts) != 1 || list.Posts[0].Author != "alice" || list.Posts[0].MediaCount != 1 {
		t.Fatalf("unexpected list: %s", rec.Body.String())
	}

	// 评论带图，原帖的附件不能串到评论上
	body, ct = multipartBody(t, map[string]string{"body": "学到了"}, map[string][]byte{"image": pngBytes})
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/forum/posts/"+created.Post.ID+"/comments", body)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(bob)
	s.handleForumPost(rec, r)
	if rec.Code != 200 {
		t.Fatalf("comment: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/forum/posts/"+created.Post.ID, nil)
	r.AddCookie(alice)
	s.handleForumPost(rec, r)
	var detail struct {
		Post     struct{ Comments int `json:"comments"` }
		Media    []struct{ ID string `json:"id"`; Mime string `json:"mime"` }
		Comments []struct{ Author string `json:"author"`; Media []struct{ ID string `json:"id"` } `json:"media"` } `json:"comments"`
	}
	json.Unmarshal(rec.Body.Bytes(), &detail)
	if len(detail.Comments) != 1 || detail.Comments[0].Author != "bob" || len(detail.Comments[0].Media) != 1 {
		t.Fatalf("comment media mismatch: %s", rec.Body.String())
	}
	if len(detail.Media) != 1 || detail.Media[0].Mime != "image/png" {
		t.Fatalf("post media mismatch: %s", rec.Body.String())
	}

	// 评论附图也要能读：它按 post/comment 两级定位，少了 post 就 404
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/forum/media/"+detail.Comments[0].Media[0].ID, nil)
	r.AddCookie(alice)
	s.handleForumMedia(rec, r)
	if rec.Code != 200 {
		t.Fatalf("comment media fetch: %d %s", rec.Code, rec.Body.String())
	}

	// 附件能读回来，未登录读不到
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/forum/media/"+detail.Media[0].ID, nil)
	r.AddCookie(alice)
	s.handleForumMedia(rec, r)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("media fetch: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/forum/media/"+detail.Media[0].ID, nil)
	s.handleForumMedia(rec, r)
	if rec.Code != 401 {
		t.Fatalf("anonymous read should be rejected, got %d", rec.Code)
	}
}

func TestForumRejectsNonMedia(t *testing.T) {
	s := newTestServer(t, Policy{})
	alice := asUser(t, s, "alice")

	body, ct := multipartBody(t, map[string]string{"title": "带个可执行文件"},
		map[string][]byte{"files": []byte("#!/bin/sh\necho hi\n")})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/forum/posts", body)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(alice)
	s.handleForumPosts(rec, r)
	if rec.Code != 200 {
		t.Fatalf("post should still be saved: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/forum/posts", nil)
	r.AddCookie(alice)
	s.handleForumPosts(rec, r)
	var list struct{ Posts []struct{ MediaCount int `json:"media_count"` } }
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Posts) != 1 || list.Posts[0].MediaCount != 0 {
		t.Fatalf("non-media file was stored: %s", rec.Body.String())
	}
}

func TestAdvancedSettingRoundTrip(t *testing.T) {
	s := newTestServer(t, Policy{})
	alice := asUser(t, s, "alice")

	call := func(method, body string) map[string]bool {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/api/me/settings", strings.NewReader(body))
		r.AddCookie(alice)
		s.handleMySettings(rec, r)
		if rec.Code != 200 {
			t.Fatalf("%s settings: %d %s", method, rec.Code, rec.Body.String())
		}
		var out map[string]bool
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if call("GET", "")["advanced"] {
		t.Fatal("fresh account should start in simple mode")
	}
	if !call("PUT", `{"advanced":true}`)["advanced"] {
		t.Fatal("advanced flag not persisted")
	}
	if !call("GET", "")["advanced"] {
		t.Fatal("advanced flag did not survive the round trip")
	}
}
