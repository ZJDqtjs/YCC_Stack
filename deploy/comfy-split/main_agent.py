#!/usr/bin/env python3
"""Generation agent for the main H3 node.

Accepts a prompt + one reference image, injects them into a pruned ComfyUI API
prompt, runs the dual-sampling H3 workflow, and returns the two latent files.
Only the Python standard library is required.
"""
import argparse
import base64
import copy
import hashlib
import json
import os
import re
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HERE = Path(__file__).resolve().parent
CFG = {
    "host": "127.0.0.1",
    "port": 8192,
    "token": "",
    "comfy": "http://127.0.0.1:8188",
    "comfy_input": r"D:\ComfyUI\input",
    "comfy_output": r"D:\ComfyUI\output",
    "template": str(HERE / "h3_api_template.json"),
    "timeout": 7200,
}
SUBMIT_LOCK = threading.Lock()


def json_response(h, code, obj):
    body = json.dumps(obj, ensure_ascii=False).encode()
    h.send_response(code)
    h.send_header("Content-Type", "application/json; charset=utf-8")
    h.send_header("Content-Length", str(len(body)))
    h.end_headers()
    h.wfile.write(body)


def comfy_json(path, payload=None, timeout=60):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(
        CFG["comfy"].rstrip("/") + path, data=data,
        headers={"Content-Type": "application/json"} if data else {},
        method="POST" if data else "GET")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def safe_token(s, limit=40):
    return re.sub(r"[^A-Za-z0-9_-]", "", s or "")[:limit] or hashlib.sha1(
        str(time.time()).encode()).hexdigest()[:10]


def save_image(data_url, job_id):
    if not data_url:
        raise ValueError("缺少参考图")
    if "," in data_url:
        data_url = data_url.split(",", 1)[1]
    raw = base64.b64decode(data_url, validate=True)
    if len(raw) > 20 << 20:
        raise ValueError("参考图过大")
    if raw[:8] == b"\x89PNG\r\n\x1a\n":
        ext = ".png"
    elif raw[:2] == b"\xff\xd8":
        ext = ".jpg"
    elif raw[:4] == b"RIFF" and raw[8:12] == b"WEBP":
        ext = ".webp"
    else:
        raise ValueError("参考图必须是 PNG/JPEG/WebP")
    name = f"comfyhub_{safe_token(job_id)}{ext}"
    dst = Path(CFG["comfy_input"]) / name
    dst.parent.mkdir(parents=True, exist_ok=True)
    tmp = dst.with_suffix(dst.suffix + ".part")
    tmp.write_bytes(raw)
    os.replace(tmp, dst)
    return name


def collect_files(outputs, node_ids):
    found = []

    def walk(obj):
        if isinstance(obj, dict):
            if obj.get("filename") and obj.get("type") == "output":
                found.append({"filename": obj["filename"],
                              "subfolder": obj.get("subfolder") or ""})
            else:
                for v in obj.values():
                    walk(v)
        elif isinstance(obj, list):
            for v in obj:
                walk(v)

    for nid in node_ids:
        walk((outputs or {}).get(nid))
    uniq = {}
    for f in found:
        uniq[(f["subfolder"], f["filename"])] = f
    return list(uniq.values())


def wait_history(prompt_id):
    deadline = time.time() + CFG["timeout"]
    while time.time() < deadline:
        hist = comfy_json("/history/" + prompt_id, timeout=20)
        entry = hist.get(prompt_id)
        if entry:
            st = entry.get("status") or {}
            if st.get("completed") or st.get("status_str") in ("success", "error"):
                return entry
        time.sleep(1.5)
    raise TimeoutError("ComfyUI generation timed out")


def generate(payload):
    prompt_text = (payload.get("prompt") or "").strip()
    if not prompt_text:
        raise ValueError("缺少提示词")
    duration = max(1.0, min(15.0, float(payload.get("duration") or 2)))
    megapixels = max(0.1, min(1.0, float(payload.get("megapixels") or 0.2)))
    steps = max(1, min(15, int(payload.get("steps") or 4)))
    upscale = max(1.0, min(2.0, float(payload.get("upscale") or 1.0)))
    aspect = payload.get("aspect") or "16:9 (Widescreen)"
    if aspect not in ("16:9 (Widescreen)", "9:16 (Portrait)", "1:1 (Square)"):
        aspect = "16:9 (Widescreen)"
    seed = int(payload.get("seed") or 999)
    job_id = safe_token(str(payload.get("job_id") or "job"))
    template = json.loads(Path(CFG["template"]).read_text(encoding="utf-8"))
    p = copy.deepcopy(template)
    image_name = save_image(payload.get("image_b64"), job_id)
    p["138"]["inputs"]["value"] = prompt_text
    p["139"]["inputs"]["image"] = image_name
    p["115"]["inputs"].update({"aspect_ratio": aspect, "megapixels": megapixels, "multiple": 32})
    p["132"]["inputs"]["value"] = duration
    p["124"]["inputs"]["steps"] = steps
    p["208"]["inputs"]["step"] = max(1, steps // 2)
    p["263"]["inputs"]["mode.scale"] = upscale
    p["129"]["inputs"]["noise_seed"] = seed
    p["902"]["inputs"]["filename_prefix"] = f"comfyhub/{job_id}_video"
    p["903"]["inputs"]["filename_prefix"] = f"comfyhub/{job_id}_audio"
    t0 = time.time()
    with SUBMIT_LOCK:
        resp = comfy_json("/prompt", {"prompt": p, "client_id": "comfyhub-main"})
    if resp.get("node_errors"):
        return {"ok": False, "error": "工作流节点错误", "node_errors": resp["node_errors"]}
    prompt_id = resp.get("prompt_id")
    if not prompt_id:
        return {"ok": False, "error": "ComfyUI 没有返回 prompt_id"}
    entry = wait_history(prompt_id)
    status = (entry.get("status") or {}).get("status_str")
    files = collect_files(entry.get("outputs") or {}, ("902", "903"))
    if status != "success" or len(files) < 2:
        return {"ok": False, "error": f"生成失败（{status}）",
                "files": files,
                "messages": (entry.get("status") or {}).get("messages")}
    video = next((f for f in files if "_video" in f["filename"]), None)
    audio = next((f for f in files if "_audio" in f["filename"]), None)
    if not video or not audio:
        return {"ok": False, "error": "没有找到视频/音频 latent", "files": files}
    return {"ok": True, "video": video, "audio": audio,
            "seconds": round(time.time() - t0, 2), "prompt_id": prompt_id}


class Handler(BaseHTTPRequestHandler):
    server_version = "StarStackComfyMain/0.1"

    def log_message(self, fmt, *args):
        print("[main]", fmt % args, flush=True)

    def auth(self):
        return not CFG["token"] or self.headers.get("X-Stage-Token") == CFG["token"]

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/health":
            info = {"ok": True, "role": "generate"}
            try:
                st = comfy_json("/system_stats", timeout=5)
                info["devices"] = st.get("devices")
            except Exception as e:
                info["ok"] = False
                info["error"] = str(e)
            json_response(self, 200, info)
            return
        if u.path == "/file":
            if not self.auth():
                json_response(self, 401, {"ok": False, "error": "unauthorized"})
                return
            q = parse_qs(u.query)
            rel = (q.get("path") or [""])[0]
            root = Path(CFG["comfy_output"]).resolve()
            target = (root / rel).resolve()
            if root not in target.parents and target != root:
                json_response(self, 400, {"ok": False, "error": "path escape"})
                return
            if not target.is_file():
                json_response(self, 404, {"ok": False, "error": "not found"})
                return
            data = target.read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        json_response(self, 404, {"ok": False, "error": "not found"})

    def do_POST(self):
        u = urlparse(self.path)
        if not self.auth():
            json_response(self, 401, {"ok": False, "error": "unauthorized"})
            return
        if u.path != "/generate":
            json_response(self, 404, {"ok": False, "error": "not found"})
            return
        try:
            n = int(self.headers.get("Content-Length") or 0)
            if n <= 0 or n > 40 << 20:
                raise ValueError("bad content-length")
            payload = json.loads(self.rfile.read(n).decode())
            json_response(self, 200, generate(payload))
        except Exception as e:
            json_response(self, 500, {"ok": False, "error": str(e)})


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default=CFG["host"])
    ap.add_argument("--port", type=int, default=CFG["port"])
    ap.add_argument("--token", default=CFG["token"])
    ap.add_argument("--comfy", default=CFG["comfy"])
    ap.add_argument("--comfy-input", default=CFG["comfy_input"])
    ap.add_argument("--comfy-output", default=CFG["comfy_output"])
    ap.add_argument("--template", default=CFG["template"])
    ap.add_argument("--timeout", type=int, default=CFG["timeout"])
    for k, v in vars(ap.parse_args()).items():
        CFG[k] = v
    print(f"main agent on {CFG['host']}:{CFG['port']}", flush=True)
    ThreadingHTTPServer((CFG["host"], CFG["port"]), Handler).serve_forever()


if __name__ == "__main__":
    main()
