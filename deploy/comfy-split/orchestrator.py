#!/usr/bin/env python3
"""Watch the main node's H3 latent output and drive the decode/mux stages.

Flow:
  main ComfyUI  -> output/split/h3_final_av_*.latent
       |
       +--> decode node (1060): video VAE  -> mp4
       +--> decode node (1060 or CPU): audio VAE -> flac
       +--> mux node (CPU): ffmpeg(video, audio) -> final mp4

Run the main workflow manually in ComfyUI (main_h3_latent.json), then leave
this script running. It only needs the Python standard library.
"""
import argparse
import json
import os
import shutil
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


def load_cfg(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def request(url, data=None, headers=None, timeout=3600, method=None):
    req = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()


def post_file(url, token, path):
    body = Path(path).read_bytes()
    headers = {"Content-Type": "application/octet-stream"}
    if token:
        headers["X-Stage-Token"] = token
    raw = request(url, data=body, headers=headers, timeout=7200)
    return json.loads(raw.decode())


def post_json(url, token, obj, timeout=3600):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["X-Stage-Token"] = token
    raw = request(url, data=json.dumps(obj).encode(), headers=headers, timeout=timeout)
    return json.loads(raw.decode())


def get_file(base, token, scope, rel, dest):
    q = urllib.parse.urlencode({"scope": scope, "path": rel})
    headers = {"X-Stage-Token": token} if token else {}
    data = request(base.rstrip("/") + "/file?" + q, headers=headers, timeout=7200)
    dest = Path(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_bytes(data)
    return len(data)


def pick(files, exts):
    for f in files:
        if Path(f["filename"]).suffix.lower() in exts:
            return f
    return None


def decode(helper, latent, kind, tmp):
    name = Path(latent).name
    q = urllib.parse.urlencode({"kind": kind, "name": name})
    res = post_file(helper["url"].rstrip("/") + "/decode?" + q, helper.get("token", ""), latent)
    if not res.get("ok"):
        raise RuntimeError(f"{kind} decode failed: {res}")
    exts = {".mp4", ".webm", ".mkv"} if kind == "video" else {".flac", ".wav", ".mp3", ".m4a"}
    f = pick(res.get("files") or [], exts)
    if not f:
        raise RuntimeError(f"{kind} decode produced no usable file: {res}")
    rel = os.path.join(f.get("subfolder") or "", f["filename"]).replace("\\", "/")
    dest = Path(tmp) / f["filename"]
    get_file(helper["url"], helper.get("token", ""), "output", rel, dest)
    return dest


def mux_via_node(helper, video, audio, out, tmp):
    tag = Path(video).stem.replace("video_", "").rstrip("_")
    vname, aname, oname = f"{tag}.mp4", f"{tag}.flac", f"{tag}_final.mp4"
    base = helper["url"].rstrip("/")
    token = helper.get("token", "")
    for path, name in ((video, vname), (audio, aname)):
        q = urllib.parse.urlencode({"name": name})
        res = post_file(base + "/mux/put?" + q, token, path)
        if not res.get("ok"):
            raise RuntimeError(f"mux upload failed: {res}")
    q = urllib.parse.urlencode({"video": vname, "audio": aname, "out": oname})
    res = post_json(base + "/mux/run?" + q, token, {})
    if not res.get("ok"):
        raise RuntimeError(f"mux failed: {res}")
    dest = Path(out) / oname
    get_file(base, token, "work", oname, dest)
    return dest


def mux_local(video, audio, out):
    if shutil.which("ffmpeg") is None:
        raise RuntimeError("ffmpeg not found locally and no mux_helper configured")
    dest = Path(out) / (Path(video).stem + "_final.mp4")
    dest.parent.mkdir(parents=True, exist_ok=True)
    cmd = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
           "-i", str(video), "-i", str(audio),
           "-map", "0:v:0", "-map", "1:a:0",
           "-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
           "-shortest", "-movflags", "+faststart", str(dest)]
    subprocess.run(cmd, check=True, timeout=7200)
    return dest


def stable(path, prev):
    try:
        st = path.stat()
    except FileNotFoundError:
        return None, None
    key = (st.st_size, int(st.st_mtime))
    if prev == key and st.st_size > 0:
        return key, True
    return key, False


def process(cfg, video_latent, audio_latent):
    tag = Path(video_latent).stem.replace("h3_video_", "")
    tmp = Path(cfg.get("tmp_dir", Path(cfg["out_dir"]) / ".work")) / tag
    tmp.mkdir(parents=True, exist_ok=True)
    print(f"[{tag}] latency decode start", flush=True)
    video = decode(cfg["video_helper"], video_latent, "video", tmp)
    audio = decode(cfg["audio_helper"], audio_latent, "audio", tmp)
    print(f"[{tag}] decoded video={video.name} audio={audio.name}", flush=True)
    if cfg.get("mux_helper"):
        final = mux_via_node(cfg["mux_helper"], video, audio, cfg["out_dir"], tmp)
    else:
        final = mux_local(video, audio, cfg["out_dir"])
    print(f"[{tag}] final {final} ({final.stat().st_size} bytes)", flush=True)
    return final


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default=str(Path(__file__).with_name("config.json")))
    ap.add_argument("--once", action="store_true")
    ap.add_argument("--retry-failed", action="store_true")
    args = ap.parse_args()
    cfg = load_cfg(args.config)
    watch = Path(cfg["watch_dir"])
    out = Path(cfg["out_dir"])
    out.mkdir(parents=True, exist_ok=True)
    state_path = Path(cfg.get("state_file", Path(__file__).with_name("orchestrator-state.json")))
    state = {}
    if state_path.is_file():
        state = json.loads(state_path.read_text(encoding="utf-8"))
    pattern = cfg.get("pattern", "h3_video_*.latent")
    pending = {}
    print(f"watching {watch / pattern}", flush=True)
    while True:
        for path in sorted(watch.glob(pattern)):
            name = path.name
            rec = state.get(name) or {}
            if rec.get("status") == "done":
                continue
            if rec.get("status") == "failed" and not args.retry_failed:
                continue
            audio_path = path.with_name(name.replace("h3_video_", "h3_audio_", 1))
            vkey, vready = stable(path, (pending.get(name) or (None, None))[0])
            akey, aready = stable(audio_path, (pending.get(name) or (None, None))[1])
            pending[name] = (vkey, akey)
            if args.once:
                # A one-shot run normally starts after the writer has already
                # closed the files; require a non-empty file instead of the
                # two-sample stability check used by the watch loop.
                vready = path.is_file() and path.stat().st_size > 0
                aready = audio_path.is_file() and audio_path.stat().st_size > 0
            if not (vready and aready):
                continue
            state[name] = {"status": "running", "started": int(time.time())}
            state_path.write_text(json.dumps(state, ensure_ascii=False, indent=1), encoding="utf-8")
            try:
                final = process(cfg, path, audio_path)
                state[name] = {"status": "done", "final": str(final), "finished": int(time.time())}
            except Exception as e:
                print(f"[{name}] FAILED: {e}", flush=True)
                state[name] = {"status": "failed", "error": str(e), "finished": int(time.time())}
            state_path.write_text(json.dumps(state, ensure_ascii=False, indent=1), encoding="utf-8")
            if args.once:
                return
        if args.once:
            return
        time.sleep(cfg.get("poll_seconds", 2))


if __name__ == "__main__":
    main()
