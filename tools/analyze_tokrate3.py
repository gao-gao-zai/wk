# -*- coding: utf-8 -*-
"""第三层：剥离"流量形态混杂"。
关键问题：>60 并发桶里 p50=92 tok/s 的流，是不是碰巧都是"短输入 glm-5.3-flash"？
对比同模型同输入长度在不同并发下的速率，才是干净的归因。"""
import sqlite3
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

rows = list(cur.execute("""
    select created_at, account_uid, model, output_tokens, ttfb_millis, latency_millis, input_tokens
    from request_logs
    where mode='stream' and status=200 and output_tokens > 30
      and latency_millis > ttfb_millis and ttfb_millis > 0
"""))

def gen_rate(r):
    gen_ms = r[5] - r[4]
    if gen_ms < 100:
        gen_ms = 100
    return r[3] / (gen_ms / 1000.0)

per_sec = defaultdict(list)
for r in rows:
    per_sec[r[0]].append(r)

def conc_bucket(c):
    if c <= 3: return "1-3"
    if c <= 30: return "4-30"
    if c <= 60: return "31-60"
    if c <= 100: return "61-100"
    return ">100"

# 控制 model 后再分桶：高并发时的 glm-5.3-flash 短输入 vs 低并发同形态
print("== 控制模型后的 并发x速率 ==")
mat = defaultdict(list)
for sec, rs in per_sec.items():
    cb = conc_bucket(len(rs))
    for r in rs:
        mat[(r[2], cb)].append(gen_rate(r))
for model in sorted({k[0] for k in mat}):
    print(f"\n  model={model}")
    for cb in ["1-3", "4-30", "31-60", "61-100", ">100"]:
        v = mat.get((model, cb))
        if not v or len(v) < 5:
            continue
        v = sorted(v)
        print(f"    conc={cb:>7} n={len(v):6d} p50={v[len(v)//2]:7.1f} p90={v[int(len(v)*0.9)]:7.1f}")

# 高并发时段的模型构成：>60 流/秒的秒里，各模型占比
print("\n== 高并发秒(>60)的模型构成 ==")
mm = defaultdict(int)
for sec, rs in per_sec.items():
    if len(rs) > 60:
        for r in rs:
            mm[r[2]] += 1
for m, c in sorted(mm.items(), key=lambda kv: -kv[1]):
    print(f"  {m:24s} {c}")

# 时段对照：找出天然高并发小时（每小时请求数 top5），看这些小时里的整体 p50
print("\n== 按小时: 请求速率 & 并发 ==")
hourly = defaultdict(list)
for r in rows:
    hourly[r[0] // 3600].append(r)
import time as _t
stats = []
for h, rs in hourly.items():
    rates = sorted(gen_rate(r) for r in rs)
    stats.append((len(rs), h, rates[len(rates)//2]))
stats.sort(reverse=True)
print(f"{'hour':>14} {'reqs':>6} {'p50 tok/s':>9}")
for n, h, p50 in stats[:12]:
    print(f"{_t.strftime('%m-%d %H:00', _t.gmtime(h*3600)):>14} {n:>6} {p50:>9.1f}")
