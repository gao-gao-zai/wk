# -*- coding: utf-8 -*-
"""对齐 new-api 视角的发现：短输出流"慢"。
在 wk 自己的 request_logs 里，把 deepseek-v4.1-flash 按 output_tokens 分桶，
分解每桶的 TTFB / 生成段 / 生成速率，看短流慢在哪里。
new-api 的 use_time = 总墙钟（含 TTFB），wk 有 ttfb_millis 可以剥离。"""
import sqlite3
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

rows = list(cur.execute("""
    select output_tokens, ttfb_millis, latency_millis, input_tokens
    from request_logs
    where mode='stream' and status=200 and model='deepseek-v4.1-flash'
      and output_tokens > 10 and ttfb_millis > 0 and latency_millis > ttfb_millis
"""))
print(f"n={len(rows)}")

buckets = defaultdict(list)
for ot, ttfb, lat, itok in rows:
    if ot < 50: b = "a <50tok"
    elif ot < 200: b = "b 50-200"
    elif ot < 500: b = "c 200-500"
    elif ot < 2000: b = "d 500-2k"
    else: b = "e >2k"
    gen_ms = max(lat - ttfb, 100)
    buckets[b].append((ttfb, gen_ms, ot / (gen_ms / 1000.0), itok))

print(f"\n{'bucket':>10} {'n':>7} {'ttfb_p50':>9} {'gen_p50':>8} {'tps_p50':>8} {'input_p50':>10}")
for b in sorted(buckets):
    v = buckets[b]
    v.sort(key=lambda x: x[2])
    n = len(v)
    ttfb = sorted(x[0] for x in v)[n//2]
    gen = sorted(x[1] for x in v)[n//2]
    tps = v[n//2][2]
    itok = sorted(x[3] for x in v)[n//2]
    print(f"{b:>10} {n:>7} {ttfb:>8.0f}ms {gen:>6.0f}ms {tps:>8.1f} {itok:>10.0f}")

# 对齐 new-api 口径（use_time=总墙钟，tokens/use_time）复现 <100 现象
print("\n== new-api 口径（总墙钟含 TTFB）复现 ==")
nb = defaultdict(list)
for ot, ttfb, lat, itok in rows:
    if ot < 50: b = "a <50tok"
    elif ot < 200: b = "b 50-200"
    elif ot < 500: b = "c 200-500"
    elif ot < 2000: b = "d 500-2k"
    else: b = "e >2k"
    wall_s = max(lat, 100) / 1000.0
    nb[b].append(ot / wall_s)
for b in sorted(nb):
    v = sorted(nb[b])
    n = len(v)
    slow = sum(1 for x in v if x < 100)
    print(f"{b:>10}  n={n:6d}  tps_p50={v[n//2]:7.1f}  <100tok/s 占比={slow*100//n}%")
