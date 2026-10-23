#!/usr/bin/env python3
# 重启恢复实测：转码跑到一半杀掉 Broker，重启后验证两件事——
#   1) 账户/会话/节点归属/单价/收益从 SQLite 原样复原（旧 Cookie 依然有效）
#   2) 在飞分片回退成 pending 且 attempt+1，重派后任务照样跑完、产物时长正确
# 这一步是为了证明"租约 + 计量流水落库"不是纸面设计：内存态调度重启即丢，
# 而闲置算力场景里 Broker 恰恰是最可能被人顺手关掉的那个进程。
import os, re, shutil, subprocess, sys, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from client import Client

# 重定向到文件时 Windows 的 stdout 会退回 GBK，中文变成乱码
sys.stdout.reconfigure(encoding="utf-8")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PORT = int(os.environ.get("PORT", 8798))
NAME, PWD = "restart", "starstack123"
SRC = os.path.join(ROOT, "samples", "input.mp4")
LOGS = os.path.join(ROOT, "rt")


def kill(image, port=PORT):
    # 只杀这套端口上的进程：taskkill /IM 会顺手干掉用户正在跑的其它 Broker
    subprocess.run(["powershell", "-NoProfile", "-Command",
                    "Get-CimInstance Win32_Process -Filter \"Name='%s'\" | "
                    "Where-Object {$_.CommandLine -match ':%d'} | "
                    "ForEach-Object { Stop-Process -Id $_.ProcessId -Force }" % (image, port)],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(0.5)


def spawn(kind, args, log):
    os.makedirs(os.path.dirname(log), exist_ok=True)
    f = open(log, "w", encoding="utf-8", errors="ignore")
    return subprocess.Popen([os.path.join(ROOT, "bin", "starstack-%s.exe" % kind)] + args,
                            stdout=f, stderr=f, cwd=ROOT)


def codes(path):
    txt = open(path, encoding="utf-8", errors="ignore").read()
    return re.findall(r"配对码 ([A-Z0-9]{6})", txt)


def snapshot(c, tag):
    _, st = c.call("/api/state")
    print("   [%s] signed_in=%s 余额=%s" % (tag, st.get("signed_in"), st.get("me", {}).get("credits")))
    for w in st["workers"]:
        print("      node=%-7s online=%-5s mine=%-5s price=%.2f earned=%s speed=%.2f" % (
            w["id"], w["online"], w["mine"], w["price"], w.get("earned"), w["speed"]))
    for j in st["jobs"]:
        print("      job=%s %s hold=%.2f spent=%.2f segs=%s" % (
            j["id"], j["status"], j["credits_hold"], j["credits_spent"],
            [(s["index"], s["status"], s["worker"], s["attempt"]) for s in j["segs"]]))
    return st


def main():
    kill("starstack-server.exe")
    kill("starstack-worker.exe")
    time.sleep(1)
    for d in ("rt-data", "rt-a", "rt-b", LOGS):
        shutil.rmtree(os.path.join(ROOT, d), ignore_errors=True)
    os.makedirs(os.path.join(ROOT, "rt-data"), exist_ok=True)

    print("[1] 起 Broker + 2 个节点（端口 %d）" % PORT)
    server = spawn("server", ["--data", "./rt-data", "--addr", ":%d" % PORT], os.path.join(LOGS, "server-1.log"))
    time.sleep(1)
    wa = spawn("worker", ["--id", "node-a", "--server", "http://127.0.0.1:%d" % PORT, "--work", "./rt-a"],
               os.path.join(LOGS, "node-a.log"))
    wb = spawn("worker", ["--id", "node-b", "--server", "http://127.0.0.1:%d" % PORT, "--work", "./rt-b"],
               os.path.join(LOGS, "node-b.log"))
    time.sleep(3)

    c = Client(PORT)
    me = c.login(NAME, PWD)
    print("[2] 注册账户 %s，赠送余额=%s" % (me["name"], me["credits"]))

    for node, log in (("node-a", "node-a"), ("node-b", "node-b")):
        cs = codes(os.path.join(LOGS, log + ".log"))
        assert cs, "%s 日志里没有配对码" % node
        code, r = c.call("/api/pair", {"code": cs[-1]})
        print("[3] 认领 %s -> %s %s" % (node, cs[-1], r))
        assert code == 200, r

    code, r = c.call("/api/nodes/node-a", {"price": 0.8}, method="PUT")
    print("[4] node-a 定价 0.8 点/视频秒（预扣按最贵的 node-b=1.0 计，差额应退还）-> %s" % r)

    code, j = c.submit({
        "path": SRC, "vcodec": "libx264", "preset": "slow", "q": "24",
        "height": "1080", "segs": "6", "audio": "1",
    })
    assert code == 200, j
    job, hold = j["id"], j["hold"]
    print("[5] 提交任务 %s：%d 片，预扣 %.2f 点" % (job, j["pieces"], hold))

    time.sleep(14)
    before = snapshot(c, "杀 Broker 前")
    inflight = [(s["index"], s["worker"]) for s in before["jobs"][0]["segs"]
                if s["status"] in ("leased", "running", "uploading")]
    print("    在飞分片 = %s" % inflight)

    print("[6] 杀掉 Broker（节点继续活着）")
    kill("starstack-server.exe")
    time.sleep(2)

    print("[7] 重启 Broker，同一份 data 目录")
    spawn("server", ["--data", "./rt-data", "--addr", ":%d" % PORT], os.path.join(LOGS, "server-2.log"))
    time.sleep(3)

    print("[8] 用重启前的 Cookie 直接访问（不重新登录）")
    _, st = c.call("/api/state")
    print("    signed_in=%s 余额=%s 节点数=%d" % (st.get("signed_in"),
          st.get("me", {}).get("credits"), len(st["workers"])))
    assert st.get("signed_in"), "会话没能跨重启存活"
    after = snapshot(c, "重启瞬间")
    segs = after["jobs"][0]["segs"]
    back = [s["index"] for s in segs if s["status"] == "pending" and s["attempt"] >= 1]
    print("    回退重派的分片 = %s（attempt 已 +1）" % back)

    print("[9] 等任务自己跑完")
    done = None
    for i in range(120):
        _, d = c.call("/api/jobs")
        cur = [x for x in d["jobs"] if x["id"] == job]
        if cur and cur[0]["status"] in ("done", "failed"):
            done = cur[0]
            break
        time.sleep(2)
    assert done, "重启后任务没有收敛"
    print("    status=%s wall=%.1fs 实结=%.2f 点（预扣 %.2f，应退 %.2f）" % (
        done["status"], done["wall"], done["credits_spent"], done["credits_hold"],
        done["credits_hold"] - done["credits_spent"]))
    for s in done["segs"]:
        print("      #%d %-8s node=%-7s dur=%.1fs cost=%.3f verified=%s attempt=%d" % (
            s["index"], s["status"], s["worker"], s["dur"], s["cost"], s["match"], s["attempt"]))

    notdone = [s["index"] for s in done["segs"] if s["status"] != "done"]
    print("    任务已 done 但分片状态不是 done 的下标 = %s" % (notdone or "无"))

    _, led = c.call("/api/ledger?limit=50")
    print("[10] 账本：余额=%.2f" % led["balance"])
    # 结算幂等性：一个分片只该出现一次收益流水，否则就是迟到节点的白拿钱
    seg_ids = [x["note"].split(" ")[1] for x in led["earn"] if (x["note"] or "").startswith("分片")]
    dup = sorted({i for i in seg_ids if seg_ids.count(i) > 1})
    print("     重复结算的分片 = %s" % (dup or "无"))
    for x in led["spend"]:
        print("      %-8s %+8.2f -> %8.2f  %s" % (x["kind"], x["amount"], x["balance"], (x["note"] or "")[:30]))
    tot = sum(x["amount"] for x in led["earn"])
    print("      我的节点合计收益 = %+.2f 点" % tot)

    out = os.path.join(ROOT, "rt-data", "jobs", job, "out.mp4")
    r = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration",
                        "-of", "default=nw=1:nk=1", out], capture_output=True, text=True)
    print("[11] 产物 %s -> duration=%s" % (os.path.basename(out), r.stdout.strip() or r.stderr.strip()))
    r = subprocess.run(["ffmpeg", "-v", "error", "-i", out, "-f", "null", "-"],
                       capture_output=True, text=True)
    print("     全片解码：%s" % ("无错误" if not r.stderr.strip() else r.stderr.strip()[:200]))

    kill("starstack-server.exe")
    kill("starstack-worker.exe")
    log = open(os.path.join(LOGS, "server-2.log"), encoding="utf-8", errors="ignore").read()
    print("[12] 重启后的 Broker 关键日志")
    for line in log.splitlines():
        if re.search(r"restored|came online|reclaim|done|stale|duplicate", line):
            print("     " + line.strip()[:150])


if __name__ == "__main__":
    main()
