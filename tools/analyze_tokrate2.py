# -*- coding: utf-8 -*-
"""补充归因：高并发慢的根因分解。
a) 单账号 3 流钉死时（streams_on_account=3）速率 99 tok/s——是上游按账号限速？
b) 高并发桶里的请求都挤在哪些账号？粘性会话热点？
c) 输入长度的影响：长上下文是否在高并发时更慢（上游 KV 队列）？
"""
import sqlite3
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

def gen_rate(r):
    gen_ms = r[5] - r[4]
    if gen_ms < 100:
        gen_ms = 100
    return r[3] / (gen_ms / 1000.0)

rows = list(cur.execute("""
    select created_at, account_uid, model, output_tokens, ttfb_millis, latency_millis, input_tokens, requested_output_tokens
    from request_logs
    where mode='stream' and status=200 and output_tokens > 30
      and latency_millis > ttfb_millis and ttfb_millis > 0
"""))
print(f"rows: {len(rows)}")

# a) 同账号高在途（3 流）时，这些请求集中在什么模型上？
per_uid_sec = defaultdict(list)
for r in rows:
    per_uid_sec[(r[0], r[1])].append(r)

saturated = [r for k, rs in per_uid_sec.items() if len(rs) >= 3 for r in rs]
print(f"\n== 饱和账号(>=3流/秒)的请求样本: {len(saturated)} ==")
model_cnt = defaultdict(int)
for r in saturated:
    model_cnt[r[2]] += 1
for m, c in sorted(model_cnt.items(), key=lambda kv: -kv[1])[:8]:
    print(f"  model={m:24s} n={c}")

# b) 高并发桶(>=60/s)时热点账号
per_sec = defaultdict(list)
for r in rows:
    per_sec[r[0]].append(r)
hot_secs = {s: rs for s, rs in per_sec.items() if len(rs) >= 60}
hot_uid = defaultdict(int)
for s, rs in hot_secs.items():
    for r in rs:
        hot_uid[r[1]] += 1
print(f"\n== 高并发秒(>=60流)的热点账号 top10: {len(hot_secs)} 个高并发秒 ==")
for uid, c in sorted(hot_uid.items(), key=lambda kv: -kv[1])[:10]:
    print(f"  {uid[:16]}  n={c}")

# c) 输入长度 × 并发桶 的速率矩阵
print("\n== 生成速率矩阵: 并发桶 x 输入长度桶 ==")
def in_bucket(t):
    if t <= 2000: return "<2k"
    if t <= 10000: return "2-10k"
    if t <= 40000: return "10-40k"
    return ">40k"
def conc_bucket(c):
    if c <= 3: return "1-3"
    if c <= 30: return "4-30"
    if c <= 60: return "31-60"
    if c <= 100: return "61-100"
    return ">100"
matrix = defaultdict(list)
for sec, rs in per_sec.items():
    cb = conc_bucket(len(rs))
    for r in rs:
        matrix[(cb, in_bucket(r[6]))].append(gen_rate(r))
print(f"{'conc':>8} {'input':>8} {'n':>7} {'p50 tok/s':>10}")
for cb in ["1-3", "4-30", "31-60", "61-100", ">100"]:
    for ib in ["<2k", "2-10k", "10-40k", ">40k"]:
        v = matrix.get((cb, ib))
        if not v or len(v) < 5:
            continue
        v = sorted(v)
        print(f"{cb:>8} {ib:>8} {len(v):>7} {v[len(v)//2]:>10.1f}")

# d) requested_output_tokens 分布：用户是不是都在要长输出
print("\n== requested_output_tokens 分布 ==")
for row in cur.execute("""
    select
      sum(case when requested_output_tokens between 1 and 1000 then 1 else 0 end),
      sum(case when requested_output_tokens between 1001 and 4000 then 1 else 0 end),
      sum(case when requested_output_tokens > 4000 then 1 else 0 end),
      count(*)
    from request_logs where mode='stream' and status=200
"""):
    print(f"  <=1k: {row[0]}  1-4k: {row[1]}  >4k: {row[2]}  total: {row[3]}")

# e) 502/超时在高并发时段的分布
print("\n== 502 (upstream_stream_error) 时间分布 ==")
for row in cur.execute("""
    select created_at/3600 as h, count(*) from request_logs
    where status=502 group by h order by h desc limit 8
"""):
    import time as _t
    print(f"  {_t.strftime('%m-%d %H:00', _t.gmtime(row[0]*3600))}  n={row[1]}")
