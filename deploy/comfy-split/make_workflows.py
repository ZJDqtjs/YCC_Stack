#!/usr/bin/env python3
"""Build the stage-split ComfyUI workflows from the user's H3 workflow.

Main node: run H3 dual sampling and write the final AV latent instead of
decoding it. Decode nodes only need the VAE weights, not the 21 GB DiT or the
15.7 GB text encoder.
"""
import copy
import json
import sys
from pathlib import Path

SRC = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(
    r"D:\ComfyUI\user\default\workflows\MiniMax-H3-双采-Latent放大.json"
)
OUT = Path(__file__).resolve().parent
DECODE_NODES = {219, 218, 220, 221}  # VAEDecode, VAEDecodeAudio, CreateVideo, SaveVideo


def load():
    with SRC.open(encoding="utf-8") as f:
        return json.load(f)


def node(w, nid):
    for n in w["nodes"]:
        if n["id"] == nid:
            return n
    raise KeyError(nid)


def drop_link(w, link_id):
    w["links"] = [ln for ln in w["links"] if ln[0] != link_id]
    for n in w["nodes"]:
        for i in n.get("inputs", []):
            if i.get("link") == link_id:
                i["link"] = None
        for o in n.get("outputs", []):
            if o.get("links"):
                o["links"] = [x for x in o["links"] if x != link_id]


def unlink_input(w, nid, input_name):
    n = node(w, nid)
    for i in n.get("inputs", []):
        if i["name"] == input_name and i.get("link") is not None:
            drop_link(w, i["link"])
            return


def add_latent_saver(w):
    # H3's sampler emits a NestedTensor (joint AV). Built-in SaveLatent calls
    # .contiguous() and cannot serialize it, so split it first: the video and
    # audio halves are ordinary tensors and can be saved independently.
    sampler = node(w, 209)
    sampler["outputs"][0]["links"] = [375]
    live = {ln[0]: ln for ln in w["links"]}
    if 375 not in live:
        raise RuntimeError("expected link 375 from sampler 209")
    live[375][3] = 901
    live[375][4] = 0
    # Drop every link that terminated in the removed decode/save nodes.
    w["links"] = [
        ln for ln in w["links"]
        if ln[1] not in DECODE_NODES and ln[3] not in DECODE_NODES
    ]
    w["nodes"] = [n for n in w["nodes"] if n["id"] not in DECODE_NODES]
    order = max(n.get("order", 0) for n in w["nodes"]) + 1
    sep = {
        "id": 901,
        "type": "LTXVSeparateAVLatent",
        "pos": [2470, 820],
        "size": [260, 60],
        "flags": {},
        "order": order,
        "mode": 0,
        "inputs": [{"name": "av_latent", "type": "LATENT", "link": 375}],
        "outputs": [
            {"name": "video_latent", "type": "LATENT", "links": [910]},
            {"name": "audio_latent", "type": "LATENT", "links": [911]},
        ],
        "properties": {"Node name for S&R": "LTXVSeparateAVLatent"},
    }
    w["links"].extend([
        [910, 901, 0, 902, 0, "LATENT"],
        [911, 901, 1, 903, 0, "LATENT"],
    ])
    for nid, link, name, prefix, y in (
        (902, 910, "video", "split/h3_video", 930),
        (903, 911, "audio", "split/h3_audio", 1040),
    ):
        w["nodes"].append({
            "id": nid,
            "type": "SaveLatent",
            "pos": [2780, y],
            "size": [320, 110],
            "flags": {},
            "order": order,
            "mode": 0,
            "inputs": [{"name": "samples", "type": "LATENT", "link": link}],
            "outputs": [{"name": "samples", "type": "LATENT", "links": None}],
            "properties": {"Node name for S&R": "SaveLatent"},
            "widgets_values": [prefix],
        })
    w["nodes"].append(sep)
    return w


def prune_dangling(w):
    live = {ln[0] for ln in w["links"]}
    for n in w["nodes"]:
        for i in n.get("inputs", []):
            if i.get("link") is not None and i["link"] not in live:
                i["link"] = None
        for o in n.get("outputs", []):
            if o.get("links"):
                o["links"] = [x for x in o["links"] if x in live] or None


def full_workflow():
    w = add_latent_saver(load())
    prune_dangling(w)
    return w


def smoke_workflow():
    w = add_latent_saver(copy.deepcopy(load()))
    # Shrink resolution/duration/steps and skip upscale so the memory and time
    # cost of a smoke run stays close to model loading.
    node(w, 115)["widgets_values"][1] = 0.2  # megapixels -> 608x352 for 16:9
    node(w, 132)["widgets_values"][0] = 2.0  # seconds
    node(w, 124)["widgets_values"][1] = 4    # scheduler steps
    node(w, 208)["widgets_values"][0] = 2    # split point -> 2 + 2
    node(w, 263)["widgets_values"][2] = 1.0  # no latent upscale
    node(w, 138)["widgets_values"][0] = (
        "A simple test scene: a red cube slowly rotating on a wooden table, "
        "soft studio light, no people, no text."
    )
    node(w, 139)["widgets_values"][0] = "comfy-split-test.png"
    # Keep only the first reference image; the test does not need the user's
    # reference video or audio tracks.
    for name in (
        "ref_images.ref_image_1", "ref_images.ref_image_2",
        "ref_images.ref_image_3", "ref_images.ref_image_4",
        "ref_images.ref_image_5", "ref_images.ref_image_6",
        "ref_videos.ref_video_0", "ref_videos.ref_video_1",
        "ref_video_audios.ref_video_audio_0",
        "ref_video_audios.ref_video_audio_1",
        "ref_audios.ref_audio_0", "ref_audios.ref_audio_1",
        "ref_audios.ref_audio_2",
    ):
        unlink_input(w, 136, name)
    prune_dangling(w)
    return w


def decode_video_api():
    return {
        "1": {"class_type": "LoadLatent", "inputs": {"latent": "LATENT_FILE"}},
        "2": {"class_type": "VAELoader",
              "inputs": {"vae_name": "minimax_h3_video_vae_fp16.safetensors"}},
        "3": {"class_type": "VAEDecodeTiled", "inputs": {
            "samples": ["1", 0], "vae": ["2", 0],
            "tile_size": 256, "overlap": 64,
            "temporal_size": 16, "temporal_overlap": 4}},
        "4": {"class_type": "CreateVideo",
              "inputs": {"images": ["3", 0], "fps": 24.0}},
        "5": {"class_type": "SaveVideo", "inputs": {
            "video": ["4", 0], "filename_prefix": "split/video",
            "format": "mp4", "codec": "h264"}},
    }


def decode_audio_api():
    return {
        "1": {"class_type": "LoadLatent", "inputs": {"latent": "LATENT_FILE"}},
        "2": {"class_type": "VAELoader",
              "inputs": {"vae_name": "minimax_h3_audio_vae_fp32.safetensors"}},
        "3": {"class_type": "VAEDecodeAudio",
              "inputs": {"samples": ["1", 0], "vae": ["2", 0]}},
        "4": {"class_type": "SaveAudio", "inputs": {
            "audio": ["3", 0], "filename_prefix": "split/audio"}},
    }


def dump(path, obj):
    with path.open("w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=False, indent=1)
    print("wrote", path)


if __name__ == "__main__":
    OUT.mkdir(parents=True, exist_ok=True)
    dump(OUT / "main_h3_latent.json", full_workflow())
    dump(OUT / "main_h3_latent_smoke.json", smoke_workflow())
    dump(OUT / "decode_video_api.json", decode_video_api())
    dump(OUT / "decode_audio_api.json", decode_audio_api())
