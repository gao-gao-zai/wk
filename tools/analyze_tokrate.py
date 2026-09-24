# -*- coding: utf-8 -*-
"""核心归因：按"同秒并发流数"分桶，看每流 tok/s 如何随并发衰减。
流式请求的生成速率 ≈ output_tokens / (latency - ttfb)。"""
import sqlite3
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

# 只看成功的流式请求（生成速率只对它们有意义）
rows = list(cur.execute("""
    select created_at, account_uid, model, output_tokens, ttfb_millis, latency_millis, input_tokens
    from request_logs
    where mode='stream' and status=200 and output_tokens > 30
      and latency_millis > ttfb_millis and ttfb_millis > 0
"""))
print(f"usable stream rows: {len(rows)}")

# 每条请求的生成段速率
def gen_rate(r):
    gen_ms = r[5] - r[4]  # latency - ttfb
    if gen_ms < 100:
        gen_ms = 100
    return r[3] / (gen_ms / 1000.0)

# 1) 按秒聚合：该秒启动的流数 = 近似并发
per_sec = defaultdict(list)
for r in rows:
    per_sec[r[0]].append((r, gen_rate(r)))

# 2) 按并发桶（该秒内的流数）
buckets = defaultdict(list)
for sec, items in per_sec.items():
    c = len(items)
    if c == 1:
        b = 1
    elif c <= 3:
        b = 3
    elif c <= 10:
        b = 10
    elif c <= 30:
        b = 30
    elif c <= 60:
        b = 60
    elif c <= 100:
        b = 100
    elif c <= 150:
        b = 150
    else:
        b = 200
    for r, rate in items:
        buckets[b].append(rate)

print("\n== 每流生成速率 (tok/s) 按同秒并发桶 ==")
print(f"{'bucket':>7} {'n':>7} {'p10':>7} {'p50':>7} {'p90':>7} {'mean':>7}")
import statistics
for b in sorted(buckets):
    v = sorted(buckets[b])
    n = len(v)
    p10 = v[int(n*0.1)]; p50 = v[int(n*0.5)]; p90 = v[int(n*0.9)]
    mean = sum(v)/n
    print(f"{b:>7} {n:>7} {p10:>7.1f} {p50:>7.1f} {p90:>7.1f} {mean:>7.1f}")

# 3) TTFB 按并发桶（建立段是否变慢）
print("\n== TTFB (ms) 按同秒并发桶 ==")
tbuckets = defaultdict(list)
for sec, items in per_sec.items():
    c = len(items)
    b = 1 if c == 1 else 3 if c <= 3 else 10 if c <= 10 else 30 if c <= 30 else 60 if c <= 60 else 100 if c <= 100 else 150 if c <= 150 else 200
    for r, _ in items:
        tbuckets[b].append(r[4])
print(f"{'bucket':>7} {'n':>7} {'p50':>7} {'p90':>7}")
for b in sorted(tbuckets):
    v = sorted(tbuckets[b])
    print(f"{b:>7} {len(v):>7} {v[len(v)//2]:>7.0f} {v[int(len(v)*0.9)]:>7.0f}")

# 4) 单账号同时挤多少流：每（秒, uid）流数分布 + 该uid并发下的速率
print("\n== 单账号同秒流数 vs 生成速率 ==")
per_uid_sec = defaultdict(list)
for r in rows:
    per_uid_sec[(r[0], r[1])].append(gen_rate(r))
ub = defaultdict(list)
for (sec, uid), rates in per_uid_sec.items():
    c = len(rates)
    b = 1 if c == 1 else 2 if c == 2 else 3 if c == 3 else 4  # max_in_flight=3 上限附近
    ub[b].extend(rates)
print(f"{'streams_on_account':>19} {'n':>7} {'p50':>7}")
for b in sorted(ub):
    v = sorted(ub[b])
    print(f"{b:>19} {len(v):>7} {v[len(v)//2]:>7.1f}")

# 5) 速率的时间分布：最近 24h 是否整体变慢（排除"某天上游慢"）
print("\n== 每小时平均生成速率（最近时段） ==")
hourly = defaultdict(list)
for r in rows:
    hourly[r[0] // 3600].append(gen_rate(r))
import time as _t
for h in sorted(hourly)[-14:]:
    v = sorted(hourly[h])
    print(f"  hour {_t.strftime('%m-%d %H:00', _t.gmtime(h*3600))}  n={len(v):6d}  p50={v[len(v)//2]:6.1f} tok/s")
