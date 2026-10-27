#!/usr/bin/env python3
"""A tiny HTTP stage worker for the ComfyUI H3 split pipeline.

Roles:
  decode  Run a VAE-only ComfyUI workflow (video VAE or audio VAE).
  mux     Receive video/audio files and run ffmpeg on a CPU node.

This intentionally uses only the Python standard library. It is not part of the
StarStack worker binary yet; it is the first executable cut of plan A so the
orchestration can be validated before a `comfy` job type is added to Broker.
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

DEFAULT_VIDEO_VAE = "minimax_h3_video_vae_fp16.safetensors"
DEFAULT_AUDIO_VAE = "minimax_h3_audio_vae_fp32.safetensors"


class Cfg:
    role = "decode"
    host = "127.0.0.1"
    port = 8190
    token = ""
    comfy = "http://127.0.0.1:8188"
    comfy_input = r"D:\ComfyUI\input"
    comfy_output = r"D:\ComfyUI\output"
    video_vae = DEFAULT_VIDEO_VAE
    audio_vae = DEFAULT_AUDIO_VAE
    fps = 24.0
    tile_size = 256
    overlap = 64
    temporal_size = 16
    temporal_overlap = 4
    ffmpeg = "ffmpeg"
    work_dir = r"C:\comfy-mux"
    timeout = 3600


CFG = Cfg()
DECODE_LOCK = threading.Lock()


def json_response(handler, code, obj):
    body = json.dumps(obj, ensure_ascii=False).encode()
    handler.send_response(code)
    handler.send_header("Content-Type", "application/json; charset=utf-8")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def read_body(handler, limit=1 << 30):
    n = int(handler.headers.get("Content-Length") or 0)
    if n <= 0 or n > limit:
        raise ValueError("bad content-length")
    return handler.rfile.read(n)


def safe_name(name):
    name = os.path.basename(name or "")
    if not name or name in (".", "..") or any(c in name for c in '\\/:*?"<>|'):
        raise ValueError("bad filename")
    return name


def comfy_json(path, payload=None, timeout=60):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(
        CFG.comfy.rstrip("/") + path,
        data=data,
        headers={"Content-Type": "application/json"} if data else {},
        method="POST" if data else "GET",
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def wait_history(prompt_id):
    deadline = time.time() + CFG.timeout
    while time.time() < deadline:
        try:
            hist = comfy_json("/history/" + prompt_id, timeout=20)
        except Exception:
            time.sleep(2)
            continue
        entry = hist.get(prompt_id)
        if entry:
            status = entry.get("status") or {}
            if status.get("completed") or status.get("status_str") in ("success", "error"):
                return entry
        time.sleep(1.5)
    raise TimeoutError("ComfyUI job timed out")


def collect_output_files(outputs):
    found = []

    def walk(obj):
        if isinstance(obj, dict):
            if obj.get("filename") and obj.get("type") == "output":
                found.append({
                    "filename": obj["filename"],
                    "subfolder": obj.get("subfolder") or "",
                    "type": "output",
                })
            else:
                for v in obj.values():
                    walk(v)
        elif isinstance(obj, list):
            for v in obj:
                walk(v)

    walk(outputs)
    uniq = {}
    for f in found:
        uniq[(f["subfolder"], f["filename"])] = f
    return list(uniq.values())


def decode_prompt(kind, latent_name, tag):
    if kind == "video":
        return {
            "1": {"class_type": "LoadLatent", "inputs": {"latent": latent_name}},
            "2": {"class_type": "VAELoader", "inputs": {"vae_name": CFG.video_vae}},
            "3": {"class_type": "VAEDecodeTiled", "inputs": {
                "samples": ["1", 0], "vae": ["2", 0],
                "tile_size": CFG.tile_size, "overlap": CFG.overlap,
                "temporal_size": CFG.temporal_size,
                "temporal_overlap": CFG.temporal_overlap}},
            "4": {"class_type": "CreateVideo",
                  "inputs": {"images": ["3", 0], "fps": CFG.fps}},
            "5": {"class_type": "SaveVideo", "inputs": {
                "video": ["4", 0], "filename_prefix": "split/video_" + tag,
                "format": "mp4", "codec": "h264"}},
        }
    if kind == "audio":
        return {
            "1": {"class_type": "LoadLatent", "inputs": {"latent": latent_name}},
            "2": {"class_type": "VAELoader", "inputs": {"vae_name": CFG.audio_vae}},
            "3": {"class_type": "VAEDecodeAudio",
                  "inputs": {"samples": ["1", 0], "vae": ["2", 0]}},
            "4": {"class_type": "SaveAudio", "inputs": {
                "audio": ["3", 0], "filename_prefix": "split/audio_" + tag}},
        }
    raise ValueError("kind must be video or audio")


def run_decode(handler):
    q = parse_qs(urlparse(handler.path).query)
    kind = (q.get("kind") or [""])[0]
    latent_name = safe_name((q.get("name") or ["latent.latent"])[0])
    if not latent_name.endswith(".latent"):
        latent_name += ".latent"
    body = read_body(handler)
    tag = hashlib.sha1(body[:1 << 20] + str(time.time()).encode()).hexdigest()[:10]
    dst = Path(CFG.comfy_input) / latent_name
    dst.parent.mkdir(parents=True, exist_ok=True)
    tmp = dst.with_suffix(".part")
    with DECODE_LOCK:
        tmp.write_bytes(body)
        os.replace(tmp, dst)
    prompt = decode_prompt(kind, latent_name, tag)
    resp = comfy_json("/prompt", {"prompt": prompt, "client_id": "starstack-stage"})
    if resp.get("node_errors"):
        json_response(handler, 400, {"ok": False, "node_errors": resp["node_errors"]})
        return
    prompt_id = resp.get("prompt_id")
    if not prompt_id:
        json_response(handler, 502, {"ok": False, "error": "ComfyUI did not return prompt_id"})
        return
    entry = wait_history(prompt_id)
    files = collect_output_files(entry.get("outputs") or {})
    status = (entry.get("status") or {}).get("status_str")
    if status != "success" or not files:
        json_response(handler, 502, {"ok": False, "status": status,
                                     "files": files,
                                     "messages": (entry.get("status") or {}).get("messages")})
        return
    json_response(handler, 200, {"ok": True, "prompt_id": prompt_id, "files": files})


def run_ffmpeg(video, audio, out):
    cmd = [CFG.ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
           "-i", str(video), "-i", str(audio),
           "-map", "0:v:0", "-map", "1:a:0",
           "-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
           "-shortest", "-movflags", "+faststart", str(out)]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=CFG.timeout)
    if p.returncode != 0:
        raise RuntimeError(p.stderr[-800:] or "ffmpeg failed")


def run_mux(handler):
    q = parse_qs(urlparse(handler.path).query)
    video = safe_name((q.get("video") or [""])[0])
    audio = safe_name((q.get("audio") or [""])[0])
    outname = safe_name((q.get("out") or ["final.mp4"])[0])
    work = Path(CFG.work_dir)
    v, a, out = work / video, work / audio, work / outname
    if not v.is_file() or not a.is_file():
        json_response(handler, 400, {"ok": False, "error": "missing uploaded stream"})
        return
    run_ffmpeg(v, a, out)
    json_response(handler, 200, {"ok": True, "file": {
        "path": outname, "bytes": out.stat().st_size}})


class Handler(BaseHTTPRequestHandler):
    server_version = "StarStackComfyStage/0.1"

    def log_message(self, fmt, *args):
        print("[stage]", fmt % args, flush=True)

    def auth(self):
        if not CFG.token:
            return True
        return self.headers.get("X-Stage-Token") == CFG.token

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/health":
            info = {"ok": True, "role": CFG.role, "comfy": CFG.comfy,
                    "host": os.uname().nodename if hasattr(os, "uname") else os.environ.get("COMPUTERNAME", "")}
            if CFG.role == "decode":
                try:
                    stats = comfy_json("/system_stats", timeout=5)
                    info["devices"] = stats.get("devices")
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
            scope = (q.get("scope") or ["output"])[0]
            rel = (q.get("path") or [""])[0]
            root = Path(CFG.comfy_output if scope == "output" else CFG.work_dir).resolve()
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
        try:
            if u.path == "/decode":
                if CFG.role != "decode":
                    json_response(self, 404, {"ok": False, "error": "decode disabled"})
                    return
                run_decode(self)
                return
            if u.path == "/mux/put":
                if CFG.role != "mux":
                    json_response(self, 404, {"ok": False, "error": "mux disabled"})
                    return
                q = parse_qs(u.query)
                name = safe_name((q.get("name") or [""])[0])
                work = Path(CFG.work_dir)
                work.mkdir(parents=True, exist_ok=True)
                data = read_body(self)
                (work / name).write_bytes(data)
                json_response(self, 200, {"ok": True, "name": name, "bytes": len(data)})
                return
            if u.path == "/mux/run":
                if CFG.role != "mux":
                    json_response(self, 404, {"ok": False, "error": "mux disabled"})
                    return
                run_mux(self)
                return
            json_response(self, 404, {"ok": False, "error": "not found"})
        except Exception as e:
            json_response(self, 500, {"ok": False, "error": str(e)})


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--role", choices=["decode", "mux"], required=True)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8190)
    ap.add_argument("--token", default="")
    ap.add_argument("--comfy", default=CFG.comfy)
    ap.add_argument("--comfy-input", default=CFG.comfy_input)
    ap.add_argument("--comfy-output", default=CFG.comfy_output)
    ap.add_argument("--video-vae", default=CFG.video_vae)
    ap.add_argument("--audio-vae", default=CFG.audio_vae)
    ap.add_argument("--fps", type=float, default=CFG.fps)
    ap.add_argument("--tile-size", type=int, default=CFG.tile_size)
    ap.add_argument("--overlap", type=int, default=CFG.overlap)
    ap.add_argument("--temporal-size", type=int, default=CFG.temporal_size)
    ap.add_argument("--temporal-overlap", type=int, default=CFG.temporal_overlap)
    ap.add_argument("--ffmpeg", default=CFG.ffmpeg)
    ap.add_argument("--work-dir", default=CFG.work_dir)
    ap.add_argument("--timeout", type=int, default=CFG.timeout)
    args = ap.parse_args()
    for k, v in vars(args).items():
        setattr(CFG, k, v)
    if CFG.role == "mux":
        Path(CFG.work_dir).mkdir(parents=True, exist_ok=True)
    print(f"stage node role={CFG.role} on {CFG.host}:{CFG.port}", flush=True)
    ThreadingHTTPServer((CFG.host, CFG.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
