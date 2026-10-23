#!/usr/bin/env python3
# 观察当前账户名下的节点与任务状态（fault-test.sh 用它在故障注入前后各打一次快照）
import sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from client import Client

sys.stdout.reconfigure(encoding="utf-8")  # 管道里 Windows 会退回 GBK

c = Client()
me = c.login()
_, d = c.call("/api/state")
print("用户 %s 余额=%.2f" % (me["name"], me["credits"]))
for w in d["workers"]:
    print("NODE %-8s online=%s mine=%s ok=%d bad=%d speed=%.2f price=%.2f earned=%s" % (
        w["id"], w["online"], w["mine"], w["ok"], w["bad"], w["speed"], w["price"],
        ("%.2f" % w["earned"]) if w.get("earned") is not None else "-"))
if not d["jobs"]:
    print("（该账户名下没有任务：fault-test 用同一个账户跑才会有输出）")
for j in d["jobs"]:
    if sys.argv[1:] and j["id"] != sys.argv[1]:
        continue
    print("JOB %s %s wall=%.1f sum=%.1f hold=%.2f spent=%.2f err=%s" % (
        j["id"], j["status"], j["wall"], j["sum_wall"], j.get("credits_hold", 0),
        j.get("credits_spent", 0), j["err"]))
    for s in j["segs"]:
        print("   #%-2d %-9s %-7s att=%d spd=%.2f cost=%.3f match=%s %s" % (
            s["index"], s["status"], s["worker"], s["attempt"], s["speed"],
            s.get("cost", 0), s["match"], (s["err"] or "")[:60]))
