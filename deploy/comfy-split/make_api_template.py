#!/usr/bin/env python3
"""Turn the UI-to-API dump into a pruned generation template.

last_prompt.json is produced by queue_workflow.js after it loads
main_h3_latent_smoke.json. It contains orphan LoadImage/LoadVideo nodes left
over from the user's original reference set; drop everything that is not an
ancestor of the two SaveLatent nodes, then freeze sensible defaults.
"""
import json
from pathlib import Path

HERE = Path(__file__).resolve().parent
SRC = HERE / "last_prompt.json"
OUT = HERE / "h3_api_template.json"


def deps(prompt, nid):
    node = prompt.get(nid) or {}
    for v in (node.get("inputs") or {}).values():
        if isinstance(v, list) and len(v) == 2 and isinstance(v[0], str):
            yield v[0]


def main():
    prompt = json.loads(SRC.read_text(encoding="utf-8"))
    keep = set()
    stack = ["902", "903"]
    while stack:
        nid = stack.pop()
        if nid in keep or nid not in prompt:
            continue
        keep.add(nid)
        stack.extend(deps(prompt, nid))
    pruned = {k: v for k, v in prompt.items() if k in keep}
    # Stable defaults for the web form; main_agent.py overrides them per job.
    pruned["115"]["inputs"].update({"aspect_ratio": "16:9 (Widescreen)", "megapixels": 0.2, "multiple": 32})
    pruned["132"]["inputs"]["value"] = 2.0
    pruned["124"]["inputs"]["steps"] = 4
    pruned["208"]["inputs"]["step"] = 2
    pruned["263"]["inputs"]["mode.scale"] = 1.0
    pruned["902"]["inputs"]["filename_prefix"] = "comfyhub/template_video"
    pruned["903"]["inputs"]["filename_prefix"] = "comfyhub/template_audio"
    OUT.write_text(json.dumps(pruned, ensure_ascii=False, indent=1), encoding="utf-8")
    dropped = sorted(set(prompt) - keep)
    print(f"kept {len(pruned)} nodes, dropped {dropped}")


if __name__ == "__main__":
    main()
