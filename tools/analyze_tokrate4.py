# -*- coding: utf-8 -*-
"""最终验证：
1) deepseek-v4.1-flash 按"单账号同秒流数"分层——3流钉死是否真的降速（控制模型后）
2) 高并发桶的时间来源——是用户真实流量还是今天的基准测试
3) 全时段每模型固有速率汇总（结论用）"""
import sqlite3, time as _t
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

# 1) deepseek-v4.1-flash 控制模型，按 (sec, uid) 流数分层
per_uid_sec = defaultdict(list)
for r in rows:
    if r[2] == "deepseek-v4.1-flash":
        per_uid_sec[(r[0], r[1])].append(gen_rate(r))
ub = defaultdict(list)
for k, rates in per_uid_sec.items():
    b = min(len(rates), 3)
    ub[b].extend(rates)
print("== deepseek-v4.1-flash: 单账号同秒流数 vs 生成速率 ==")
for b in sorted(ub):
    v = sorted(ub[b])
    print(f"  streams={b}  n={len(v):6d}  p50={v[len(v)//2]:7.1f}  p10={v[int(len(v)*0.1)]:7.1f}")

# 2) >60 并发秒的时间分布（判断是否为今天基准测试）
per_sec = defaultdict(int)
for r in rows:
    per_sec[r[0]] += 1
print("\n== >60 流/秒 的秒的时间分布 ==")
for s in sorted(s for s, c in per_sec.items() if c > 60):
    print(f"  {_t.strftime('%m-%d %H:%M:%S', _t.gmtime(s))}  streams={per_sec[s]}")

# 3) 每模型全时段固有速率
print("\n== 每模型固有生成速率（全时段，不分管发） ==")
mr = defaultdict(list)
for r in rows:
    mr[r[2]].append(gen_rate(r))
for m, v in sorted(mr.items(), key=lambda kv: -len(kv[1])):
    v.sort()
    print(f"  {m:24s} n={len(v):6d}  p50={v[len(v)//2]:7.1f}  p90={v[int(len(v)*0.9)]:7.1f}")

# 4) 输入长度对 deepseek-v4.1-flash 速率的影响（排除长输入拖慢的混杂）
print("\n== deepseek-v4.1-flash: 输入长度 vs 速率 ==")
ib = defaultdict(list)
for r in rows:
    if r[2] != "deepseek-v4.1-flash":
        continue
    k = "<2k" if r[6] <= 2000 else "2-10k" if r[6] <= 10000 else "10-40k" if r[6] <= 40000 else ">40k"
    ib[k].append(gen_rate(r))
for k in ["<2k", "2-10k", "10-40k", ">40k"]:
    v = ib.get(k)
    if v and len(v) >= 5:
        v.sort()
        print(f"  input={k:8s} n={len(v):6d}  p50={v[len(v)//2]:7.1f}")
