# -*- coding: utf-8 -*-
"""TTFB 构成：input 越长 prefill 越久。
deepseek-v4.1-flash 的输入 p50 在 10 万 token 量级——上游要先做完 prefill
才能吐首 token，这是物理时间，任何网关都省不掉。
按输入长度分桶看 TTFB 分布，并算出"TTFB 占总墙钟的比例"。"""
import sqlite3
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

rows = list(cur.execute("""
    select input_tokens, ttfb_millis, latency_millis, cache_read_tokens
    from request_logs
    where mode='stream' and status=200 and model='deepseek-v4.1-flash'
      and output_tokens between 50 and 500 and ttfb_millis > 0 and latency_millis > ttfb_millis
"""))
print(f"n={len(rows)}  (短中输出流，即 new-api 后台看到慢的那些)")

ib = defaultdict(list)
for itok, ttfb, lat, cr in rows:
    if itok <= 20000: b = "<20k"
    elif itok <= 60000: b = "20-60k"
    elif itok <= 120000: b = "60-120k"
    elif itok <= 200000: b = "120-200k"
    else: b = ">200k"
    ib[b].append((ttfb, lat, cr, itok))

print(f"\n{'input':>10} {'n':>7} {'ttfb_p50':>9} {'ttfb_p90':>9} {'wall_p50':>9} {'ttfb/wall':>9} {'cache_hit':>9}")
for b in ["<20k", "20-60k", "60-120k", "120-200k", ">200k"]:
    v = ib.get(b)
    if not v or len(v) < 10:
        print(f"{b:>10}  n={len(v) if v else 0} (样本不足)")
        continue
    v.sort(key=lambda x: x[0])
    n = len(v)
    ttfb50 = v[n//2][0]
    ttfb90 = v[int(n*0.9)][0]
    lat50 = sorted(x[1] for x in v)[n//2]
    cache = sorted(x[2] for x in v)[n//2]
    ratio = ttfb50 / lat50 * 100
    print(f"{b:>10} {n:>7} {ttfb50:>8.0f}ms {ttfb90:>8.0f}ms {lat50:>8.0f}ms {ratio:>8.0f}% {cache:>9.0f}")

# 缓存命中对 TTFB 的影响
print("\n== 有无缓存命中 × TTFB（60-120k 输入桶） ==")
hit = [x[0] for x in ib.get("60-120k", []) if x[2] > 1000]
miss = [x[0] for x in ib.get("60-120k", []) if x[2] <= 1000]
for name, v in [("cache_hit>1k", hit), ("cache_miss", miss)]:
    if len(v) >= 10:
        v.sort()
        print(f"  {name:14s} n={len(v):5d}  ttfb_p50={v[len(v)//2]:.0f}ms  p90={v[int(len(v)*0.9)]:.0f}ms")
    else:
        print(f"  {name:14s} n={len(v)}")
