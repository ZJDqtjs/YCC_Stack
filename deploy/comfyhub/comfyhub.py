#!/usr/bin/env python3
"""ComfyHub: share ComfyUI stage agents on the StarStack network.

Runs next to the Broker, trusts the Broker session cookie, and orchestrates:
  generate (main H3 node) -> VAE decode (1060) -> mux (CPU) -> final mp4

Only the Python standard library is used. Node endpoints are ordinary HTTP
agents (main_agent.py / stage_node.py) and must be reachable from this process.
"""
import argparse
import base64
import hashlib
import json
import os
import sqlite3
import threading
import time
import urllib.parse
import urllib.request
import uuid
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

HERE = Path(__file__).resolve().parent
CFG = {
    "host": "127.0.0.1",
    "port": 8788,
    "data_dir": str(HERE / "data"),
    "static_dir": str(HERE / "static"),
    "agent_dir": str(HERE / "agents"),
    "broker": "http://127.0.0.1:8787",
    "health_seconds": 20,
    "health_fresh": 120,
}
ROLES = {"generate", "decode", "mux"}
PIPELINE_LOCK = threading.Lock()
SESSION_CACHE = {}
SESSION_LOCK = threading.Lock()


@contextmanager
def db():
    p = Path(CFG["data_dir"]) / "comfyhub.db"
    conn = sqlite3.connect(str(p), timeout=30)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    try:
        yield conn
        conn.commit()
    finally:
        conn.close()


def init_db():
    d = Path(CFG["data_dir"])
    d.mkdir(parents=True, exist_ok=True)
    with db() as conn:
        conn.executescript("""
        CREATE TABLE IF NOT EXISTS nodes(
          id TEXT PRIMARY KEY, owner_id TEXT, owner_name TEXT, name TEXT,
          role TEXT, url TEXT, token TEXT, price REAL, capacity INTEGER,
          enabled INTEGER, created_at INTEGER, last_ok INTEGER, last_error TEXT,
          last_seen INTEGER);
        CREATE TABLE IF NOT EXISTS jobs(
          id TEXT PRIMARY KEY, owner_id TEXT, owner_name TEXT, status TEXT,
          stage TEXT, params TEXT, image_path TEXT, video_latent TEXT,
          audio_latent TEXT, video_path TEXT, audio_path TEXT, final_path TEXT,
          error TEXT, log TEXT, created_at INTEGER, updated_at INTEGER);
        """)
    os.chmod(d / "comfyhub.db", 0o600)


def now():
    return int(time.time())


def json_dumps(obj):
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def json_response(h, code, obj):
    body = json_dumps(obj).encode()
    h.send_response(code)
    h.send_header("Content-Type", "application/json; charset=utf-8")
    h.send_header("Cache-Control", "no-store")
    h.send_header("Content-Length", str(len(body)))
    h.end_headers()
    h.wfile.write(body)


def text_response(h, code, text, ctype="text/plain; charset=utf-8"):
    body = text.encode()
    h.send_response(code)
    h.send_header("Content-Type", ctype)
    h.send_header("Content-Length", str(len(body)))
    h.end_headers()
    h.wfile.write(body)


def read_json(h, limit=32 << 20):
    n = int(h.headers.get("Content-Length") or 0)
    if n <= 0 or n > limit:
        raise ValueError("请求体过大或为空")
    return json.loads(h.rfile.read(n).decode())


def http_json(url, token="", payload=None, timeout=120):
    data = None if payload is None else json.dumps(payload).encode()
    headers = {}
    if data is not None:
        headers["Content-Type"] = "application/json"
    if token:
        headers["X-Stage-Token"] = token
    req = urllib.request.Request(url, data=data, headers=headers,
                                 method="POST" if data is not None else "GET")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def http_bytes(url, token="", payload=None, timeout=3600, method=None):
    headers = {}
    if token:
        headers["X-Stage-Token"] = token
    if payload is not None:
        headers["Content-Type"] = "application/octet-stream"
    req = urllib.request.Request(url, data=payload, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()


def upload_bytes(url, token, data):
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/octet-stream",
                                          "X-Stage-Token": token})
    with urllib.request.urlopen(req, timeout=7200) as r:
        return json.loads(r.read().decode())


def download_node_file(node, subfolder, filename, dest):
    rel = "/".join(x for x in (subfolder or "", filename) if x)
    q = urllib.parse.urlencode({"path": rel})
    root = node["url"].rstrip("/")
    # mux files live in the agent's work scope; decode/generate files in output.
    scope = "work" if node["role"] == "mux" else "output"
    if node["role"] == "mux":
        url = root + "/file?" + urllib.parse.urlencode({"scope": "work", "path": rel})
    else:
        url = root + "/file?" + q
    data = http_bytes(url, node["token"], timeout=7200)
    Path(dest).parent.mkdir(parents=True, exist_ok=True)
    Path(dest).write_bytes(data)
    return len(data)


def current_user(headers):
    cookie = headers.get("Cookie") or ""
    if not cookie:
        return None
    key = hashlib.sha256(cookie.encode()).hexdigest()
    with SESSION_LOCK:
        hit = SESSION_CACHE.get(key)
        if hit and time.time() - hit[0] < 5:
            return hit[1]
    try:
        req = urllib.request.Request(CFG["broker"].rstrip("/") + "/api/me",
                                     headers={"Cookie": cookie})
        with urllib.request.urlopen(req, timeout=8) as r:
            me = json.loads(r.read().decode())
    except Exception:
        me = {"signed_in": False}
    user = None
    if me.get("signed_in"):
        user = {"id": me.get("id"), "name": me.get("name") or "用户"}
    with SESSION_LOCK:
        SESSION_CACHE[key] = (time.time(), user)
        if len(SESSION_CACHE) > 500:
            SESSION_CACHE.clear()
    return user


def require_user(h):
    u = current_user(h.headers)
    if not u:
        json_response(h, 401, {"ok": False, "error": "请先在星栈控制台登录"})
        return None
    return u


def node_view(row, public=False):
    fresh = int(row["last_ok"] or 0) >= now() - CFG["health_fresh"]
    v = {
        "id": row["id"], "name": row["name"], "role": row["role"],
        "price": row["price"], "capacity": row["capacity"],
        "enabled": bool(row["enabled"]), "online": fresh,
        "last_seen": row["last_ok"] or 0,
        "last_error": "" if fresh else (row["last_error"] or "等待心跳"),
    }
    if not public:
        v["url"] = row["url"]
        v["has_token"] = bool(row["token"])
        v["owner_name"] = row["owner_name"]
    else:
        v["owner_name"] = row["owner_name"]
    return v


def select_node(role, chosen=""):
    with db() as conn:
        if chosen:
            row = conn.execute("SELECT * FROM nodes WHERE id=? AND role=? AND enabled=1",
                               (chosen, role)).fetchone()
            if row:
                return dict(row)
        row = conn.execute(
            "SELECT * FROM nodes WHERE role=? AND enabled=1 AND last_ok >= ? "
            "ORDER BY price ASC, created_at ASC LIMIT 1",
            (role, now() - CFG["health_fresh"])).fetchone()
        return dict(row) if row else None


def append_log(job_id, text):
    with db() as conn:
        row = conn.execute("SELECT log FROM jobs WHERE id=?", (job_id,)).fetchone()
        log = json.loads(row["log"] or "[]") if row else []
        log.append(f"{time.strftime('%H:%M:%S')} {text}")
        conn.execute("UPDATE jobs SET log=?, updated_at=? WHERE id=?",
                     (json.dumps(log, ensure_ascii=False), now(), job_id))


def set_job(job_id, **fields):
    if not fields:
        return
    fields["updated_at"] = now()
    cols = ", ".join(k + "=?" for k in fields)
    with db() as conn:
        conn.execute(f"UPDATE jobs SET {cols} WHERE id=?", (*fields.values(), job_id))


def pick_file(files, exts):
    for f in files or []:
        if Path(f.get("filename", "")).suffix.lower() in exts:
            return f
    return None


def decode_b64(data, limit):
    if not data:
        return None
    if "," in data:
        data = data.split(",", 1)[1]
    raw = base64.b64decode(data, validate=True)
    if len(raw) > limit:
        raise ValueError("文件过大")
    return raw


def run_job(job_id, payload, user):
    with PIPELINE_LOCK:
        try:
            set_job(job_id, status="running", stage="准备")
            append_log(job_id, "任务开始")
            jobdir = Path(CFG["data_dir"]) / "jobs" / job_id
            jobdir.mkdir(parents=True, exist_ok=True)

            video_latent = jobdir / "video.latent"
            audio_latent = jobdir / "audio.latent"
            image_raw = decode_b64(payload.get("image_b64"), 20 << 20)
            if image_raw:
                (jobdir / "input.img").write_bytes(image_raw)

            if not (video_latent.is_file() and audio_latent.is_file()):
                gen = select_node("generate", payload.get("generator_id") or "")
                if not gen:
                    raise RuntimeError("没有在线的生成节点，请先共享主节点或上传 latent")
                append_log(job_id, f"生成节点：{gen['name']}")
                set_job(job_id, stage="主节点生成")
                body = {
                    "job_id": job_id,
                    "prompt": payload.get("prompt") or "",
                    "image_b64": payload.get("image_b64") or "",
                    "duration": payload.get("duration") or 2,
                    "megapixels": payload.get("megapixels") or 0.2,
                    "steps": payload.get("steps") or 4,
                    "upscale": payload.get("upscale") or 1.0,
                    "aspect": payload.get("aspect") or "16:9 (Widescreen)",
                    "seed": payload.get("seed") or 999,
                }
                res = http_json(gen["url"].rstrip("/") + "/generate", gen["token"],
                                body, timeout=CFG.get("generation_timeout", 7200))
                if not res.get("ok"):
                    raise RuntimeError("生成失败：" + (res.get("error") or "未知错误"))
                download_node_file(gen, res["video"]["subfolder"], res["video"]["filename"], video_latent)
                download_node_file(gen, res["audio"]["subfolder"], res["audio"]["filename"], audio_latent)
                append_log(job_id, f"生成完成（{res.get('seconds', '?')}s）")
                set_job(job_id, video_latent=str(video_latent), audio_latent=str(audio_latent))

            dec = select_node("decode", payload.get("decode_id") or "")
            if not dec:
                raise RuntimeError("没有在线的 VAE 解码节点")
            append_log(job_id, f"解码节点：{dec['name']}")
            set_job(job_id, stage="VAE 解码")
            q = urllib.parse.urlencode({"kind": "video", "name": job_id + "_video.latent"})
            res = upload_bytes(dec["url"].rstrip("/") + "/decode?" + q, dec["token"],
                               video_latent.read_bytes())
            if not res.get("ok"):
                raise RuntimeError("视频解码失败：" + json_dumps(res)[:300])
            vf = pick_file(res.get("files"), {".mp4", ".webm", ".mkv"})
            if not vf:
                raise RuntimeError("视频解码没有返回视频文件")
            video_path = jobdir / "video.mp4"
            download_node_file(dec, vf.get("subfolder"), vf["filename"], video_path)

            q = urllib.parse.urlencode({"kind": "audio", "name": job_id + "_audio.latent"})
            res = upload_bytes(dec["url"].rstrip("/") + "/decode?" + q, dec["token"],
                               audio_latent.read_bytes())
            if not res.get("ok"):
                raise RuntimeError("音频解码失败：" + json_dumps(res)[:300])
            af = pick_file(res.get("files"), {".flac", ".wav", ".mp3", ".m4a"})
            if not af:
                raise RuntimeError("音频解码没有返回音频文件")
            audio_path = jobdir / ("audio" + Path(af["filename"]).suffix)
            download_node_file(dec, af.get("subfolder"), af["filename"], audio_path)
            set_job(job_id, video_path=str(video_path), audio_path=str(audio_path))
            append_log(job_id, "视频/音频解码完成")

            mux = select_node("mux", payload.get("mux_id") or "")
            if not mux:
                raise RuntimeError("没有在线的封装节点")
            append_log(job_id, f"封装节点：{mux['name']}")
            set_job(job_id, stage="封装/转码")
            base = mux["url"].rstrip("/")
            vname, aname, oname = job_id + ".mp4", job_id + ".flac", job_id + "_final.mp4"
            q = urllib.parse.urlencode({"name": vname})
            upload_bytes(base + "/mux/put?" + q, mux["token"], video_path.read_bytes())
            q = urllib.parse.urlencode({"name": aname})
            upload_bytes(base + "/mux/put?" + q, mux["token"], audio_path.read_bytes())
            q = urllib.parse.urlencode({"video": vname, "audio": aname, "out": oname})
            res = http_json(base + "/mux/run?" + q, mux["token"], {}, timeout=3600)
            if not res.get("ok"):
                raise RuntimeError("封装失败：" + json_dumps(res)[:300])
            final = jobdir / "final.mp4"
            download_node_file(mux, "", oname, final)
            set_job(job_id, final_path=str(final), status="done", stage="完成", error="")
            append_log(job_id, f"完成，{final.stat().st_size} bytes")
            # Intermediate latents can be large; keep them for debugging for now.
        except Exception as e:
            set_job(job_id, status="failed", error=str(e))
            append_log(job_id, "失败：" + str(e))


def run_health():
    while True:
        try:
            with db() as conn:
                rows = conn.execute("SELECT * FROM nodes WHERE enabled=1").fetchall()
            for row in rows:
                ok, err = False, ""
                try:
                    res = http_json(row["url"].rstrip("/") + "/health", row["token"], timeout=8)
                    ok = bool(res.get("ok", True))
                    err = "" if ok else (res.get("error") or "health not ok")
                except Exception as e:
                    err = str(e)[:240]
                with db() as conn:
                    conn.execute(
                        "UPDATE nodes SET last_ok=?, last_seen=?, last_error=? WHERE id=?",
                        (now() if ok else row["last_ok"], now(), err, row["id"]))
        except Exception as e:
            print("[health]", e, flush=True)
        time.sleep(CFG["health_seconds"])


class Handler(BaseHTTPRequestHandler):
    server_version = "StarStackComfyHub/0.1"

    def log_message(self, fmt, *args):
        print("[comfyhub]", fmt % args, flush=True)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path in ("/", "/index.html"):
            return self.send_static("index.html", "text/html; charset=utf-8")
        if u.path.startswith("/downloads/"):
            return self.send_agent(u.path.rsplit("/", 1)[-1])
        if u.path == "/api/session":
            user = current_user(self.headers)
            return json_response(self, 200, {"ok": True, "signed_in": bool(user), "user": user})
        if u.path == "/api/nodes":
            user = require_user(self)
            if not user:
                return
            with db() as conn:
                rows = conn.execute("SELECT * FROM nodes WHERE owner_id=? ORDER BY created_at DESC",
                                    (user["id"],)).fetchall()
            return json_response(self, 200, {"ok": True,
                                             "nodes": [node_view(r) for r in rows]})
        if u.path == "/api/market":
            with db() as conn:
                rows = conn.execute("SELECT * FROM nodes WHERE enabled=1 ORDER BY role, price").fetchall()
            return json_response(self, 200, {"ok": True,
                                             "nodes": [node_view(r, public=True) for r in rows]})
        if u.path == "/api/jobs":
            user = require_user(self)
            if not user:
                return
            with db() as conn:
                rows = conn.execute("SELECT * FROM jobs WHERE owner_id=? ORDER BY created_at DESC LIMIT 50",
                                    (user["id"],)).fetchall()
            return json_response(self, 200, {"ok": True, "jobs": [self.job_view(r) for r in rows]})
        m = re_match(r"^/api/jobs/([A-Za-z0-9_-]+)$", u.path)
        if m:
            user = require_user(self)
            if not user:
                return
            with db() as conn:
                row = conn.execute("SELECT * FROM jobs WHERE id=? AND owner_id=?",
                                   (m, user["id"])).fetchone()
            if not row:
                return json_response(self, 404, {"ok": False, "error": "任务不存在"})
            return json_response(self, 200, {"ok": True, "job": self.job_view(row)})
        m = re_match(r"^/api/jobs/([A-Za-z0-9_-]+)/download$", u.path)
        if m:
            user = require_user(self)
            if not user:
                return
            with db() as conn:
                row = conn.execute("SELECT * FROM jobs WHERE id=? AND owner_id=?",
                                   (m, user["id"])).fetchone()
            if not row or not row["final_path"] or not Path(row["final_path"]).is_file():
                return json_response(self, 404, {"ok": False, "error": "还没有成品"})
            data = Path(row["final_path"]).read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "video/mp4")
            self.send_header("Content-Disposition", 'attachment; filename="%s.mp4"' % m)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        return json_response(self, 404, {"ok": False, "error": "not found"})

    def do_POST(self):
        u = urlparse(self.path)
        user = require_user(self)
        if not user:
            return
        try:
            if u.path == "/api/nodes":
                return self.create_node(user)
            if u.path == "/api/jobs":
                return self.create_job(user)
            m = re_match(r"^/api/nodes/([A-Za-z0-9_-]+)/test$", u.path)
            if m:
                return self.test_node(user, m)
            return json_response(self, 404, {"ok": False, "error": "not found"})
        except Exception as e:
            return json_response(self, 500, {"ok": False, "error": str(e)})

    def do_PUT(self):
        user = require_user(self)
        if not user:
            return
        u = urlparse(self.path)
        m = re_match(r"^/api/nodes/([A-Za-z0-9_-]+)$", u.path)
        if not m:
            return json_response(self, 404, {"ok": False, "error": "not found"})
        try:
            body = read_json(self, 1 << 20)
            with db() as conn:
                row = conn.execute("SELECT * FROM nodes WHERE id=? AND owner_id=?",
                                   (m, user["id"])).fetchone()
                if not row:
                    return json_response(self, 404, {"ok": False, "error": "节点不存在"})
                fields = {}
                for k in ("name", "url", "token", "price", "capacity", "enabled", "role"):
                    if k in body:
                        fields[k] = body[k]
                if "enabled" in fields:
                    fields["enabled"] = 1 if fields["enabled"] else 0
                if fields:
                    cols = ", ".join(k + "=?" for k in fields)
                    conn.execute(f"UPDATE nodes SET {cols} WHERE id=?",
                                 (*fields.values(), m))
            return json_response(self, 200, {"ok": True})
        except Exception as e:
            return json_response(self, 400, {"ok": False, "error": str(e)})

    def do_DELETE(self):
        user = require_user(self)
        if not user:
            return
        m = re_match(r"^/api/nodes/([A-Za-z0-9_-]+)$", urlparse(self.path).path)
        if not m:
            return json_response(self, 404, {"ok": False, "error": "not found"})
        with db() as conn:
            conn.execute("DELETE FROM nodes WHERE id=? AND owner_id=?", (m, user["id"]))
        return json_response(self, 200, {"ok": True})

    def create_node(self, user):
        body = read_json(self, 1 << 20)
        name = (body.get("name") or "").strip()[:60]
        role = body.get("role") or ""
        url = (body.get("url") or "").strip().rstrip("/")
        if not name or role not in ROLES or not url:
            return json_response(self, 400, {"ok": False, "error": "名称、角色、地址必填"})
        p = urlparse(url)
        if p.scheme not in ("http", "https") or not p.hostname:
            return json_response(self, 400, {"ok": False, "error": "地址必须是 http(s)://"})
        if p.hostname in ("metadata.google.internal", "metadata"):
            return json_response(self, 400, {"ok": False, "error": "地址不允许"})
        try:
            price = max(0.0, min(10000.0, float(body.get("price") or 0)))
            cap = max(1, min(16, int(body.get("capacity") or 1)))
        except Exception:
            return json_response(self, 400, {"ok": False, "error": "价格或容量格式错误"})
        nid = "n" + uuid.uuid4().hex[:12]
        token = body.get("token") or ""
        with db() as conn:
            conn.execute(
                "INSERT INTO nodes(id,owner_id,owner_name,name,role,url,token,price,capacity,"
                "enabled,created_at,last_ok,last_error,last_seen) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (nid, user["id"], user["name"], name, role, url, token,
                 price, cap, 0 if body.get("enabled") is False else 1, now(), 0, "等待心跳", 0))
        health, ok, err = None, False, "等待心跳"
        try:
            health = http_json(url + "/health", token, timeout=8)
            ok = bool(health.get("ok", True))
            err = "" if ok else (health.get("error") or "health not ok")
        except Exception as e:
            err = str(e)[:240]
        with db() as conn:
            conn.execute("UPDATE nodes SET last_ok=?, last_error=? WHERE id=?",
                         (now() if ok else 0, err, nid))
        return json_response(self, 200, {"ok": True, "id": nid,
                                         "online": ok, "health": health, "error": err})

    def test_node(self, user, nid):
        with db() as conn:
            row = conn.execute("SELECT * FROM nodes WHERE id=? AND owner_id=?",
                               (nid, user["id"])).fetchone()
        if not row:
            return json_response(self, 404, {"ok": False, "error": "节点不存在"})
        try:
            res = http_json(row["url"].rstrip("/") + "/health", row["token"], timeout=15)
            ok = bool(res.get("ok", True))
            with db() as conn:
                conn.execute("UPDATE nodes SET last_ok=?, last_error=? WHERE id=?",
                             (now() if ok else row["last_ok"], "" if ok else "health not ok", nid))
            return json_response(self, 200, {"ok": ok, "health": res})
        except Exception as e:
            with db() as conn:
                conn.execute("UPDATE nodes SET last_error=? WHERE id=?", (str(e)[:240], nid))
            return json_response(self, 502, {"ok": False, "error": str(e)})

    def create_job(self, user):
        body = read_json(self, 64 << 20)
        prompt = (body.get("prompt") or "").strip()
        vlat = decode_b64(body.get("video_latent_b64"), 64 << 20)
        alat = decode_b64(body.get("audio_latent_b64"), 64 << 20)
        img = decode_b64(body.get("image_b64"), 20 << 20)
        if not prompt and not (vlat and alat):
            return json_response(self, 400, {"ok": False, "error": "请填写提示词或上传两份 latent"})
        if not (vlat and alat):
            vlat = alat = None
            if not img:
                return json_response(self, 400, {"ok": False, "error": "生成任务需要一张参考图"})
            if not select_node("generate", body.get("generator_id") or ""):
                return json_response(self, 400, {"ok": False, "error": "没有在线的生成节点"})
        if not select_node("decode", body.get("decode_id") or ""):
            return json_response(self, 400, {"ok": False, "error": "没有在线的 VAE 解码节点"})
        if not select_node("mux", body.get("mux_id") or ""):
            return json_response(self, 400, {"ok": False, "error": "没有在线的封装节点"})
        jid = "c" + uuid.uuid4().hex[:14]
        jobdir = Path(CFG["data_dir"]) / "jobs" / jid
        jobdir.mkdir(parents=True, exist_ok=True)
        if img:
            (jobdir / "input.img").write_bytes(img)
        if vlat:
            (jobdir / "video.latent").write_bytes(vlat)
            (jobdir / "audio.latent").write_bytes(alat)
        params = {k: body.get(k) for k in
                  ("duration", "megapixels", "steps", "upscale", "aspect", "seed",
                   "generator_id", "decode_id", "mux_id") if k in body}
        params["prompt"] = prompt[:600]
        params["uploaded_latents"] = bool(vlat and alat)
        with db() as conn:
            conn.execute(
                "INSERT INTO jobs(id,owner_id,owner_name,status,stage,params,image_path,"
                "video_latent,audio_latent,video_path,audio_path,final_path,error,log,"
                "created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (jid, user["id"], user["name"], "queued", "排队", json_dumps(params),
                 str(jobdir / "input.img") if img else "",
                 str(jobdir / "video.latent") if vlat else "",
                 str(jobdir / "audio.latent") if vlat else "",
                 "", "", "", "", "[]", now(), now()))
        payload = dict(body)
        payload["video_latent_b64"] = None
        threading.Thread(target=run_job, args=(jid, payload, user), daemon=True).start()
        return json_response(self, 200, {"ok": True, "id": jid})

    def job_view(self, row):
        params = {}
        try:
            params = json.loads(row["params"] or "{}")
        except Exception:
            pass
        log = []
        try:
            log = json.loads(row["log"] or "[]")
        except Exception:
            pass
        return {
            "id": row["id"], "status": row["status"], "stage": row["stage"],
            "error": row["error"], "log": log, "params": params,
            "has_final": bool(row["final_path"] and Path(row["final_path"]).is_file()),
            "created_at": row["created_at"], "updated_at": row["updated_at"],
        }

    def send_static(self, name, ctype):
        p = Path(CFG["static_dir"]) / name
        if not p.is_file():
            return json_response(self, 404, {"ok": False, "error": "not found"})
        data = p.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def send_agent(self, name):
        if name not in ("stage_node.py", "main_agent.py", "h3_api_template.json"):
            return json_response(self, 404, {"ok": False, "error": "not found"})
        p = Path(CFG["agent_dir"]) / name
        if not p.is_file():
            return json_response(self, 404, {"ok": False, "error": "not found"})
        data = p.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Disposition", 'attachment; filename="%s"' % name)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def re_match(pattern, value):
    import re
    m = re.match(pattern, value)
    return m.group(1) if m else None


def main():
    ap = argparse.ArgumentParser()
    for k in ("host", "data_dir", "static_dir", "agent_dir", "broker"):
        ap.add_argument("--" + k.replace("_", "-"), default=CFG[k])
    ap.add_argument("--port", type=int, default=CFG["port"])
    ap.add_argument("--health-seconds", type=int, default=CFG["health_seconds"])
    ap.add_argument("--health-fresh", type=int, default=CFG["health_fresh"])
    args = ap.parse_args()
    for k, v in vars(args).items():
        CFG[k] = v
    init_db()
    threading.Thread(target=run_health, daemon=True).start()
    print(f"comfyhub on {CFG['host']}:{CFG['port']} data={CFG['data_dir']}", flush=True)
    ThreadingHTTPServer((CFG["host"], CFG["port"]), Handler).serve_forever()


if __name__ == "__main__":
    main()
