# -*- coding: utf-8 -*-
"""检验"TTFB 突然变大"的另外三个假设：
1) 输入长度的流量构成逐日变化——是否最近有更多超长输入(>200k)的新流量？
2) 缓存命中率逐日变化——cache_write（首次写入）是否变多，即新会话占比上升？
3) 输入逐日变长：全量（不控制桶）的 input p50 走势。"""
import sqlite3
import time as _t
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

tmax = cur.execute("select max(created_at) from request_logs").fetchone()[0]

rows = list(cur.execute("""
    select created_at, input_tokens, ttfb_millis, cache_read_tokens, cache_write_tokens, output_tokens
    from request_logs
    where mode='stream' and status=200 and model='deepseek-v4.1-flash'
      and output_tokens > 30 and ttfb_millis > 0
"""))

daily = defaultdict(list)
for ts, itok, ttfb, cr, cw, ot in rows:
    daily[_t.strftime('%m-%d', _t.gmtime(ts))].append((itok, ttfb, cr, cw))

print("== 逐日全量（不控制输入桶）==")
print(f"{'day':>8} {'n':>7} {'input_p50':>10} {'input_p90':>10} {'>200k占比':>9} {'ttfb_p50':>9} {'cache_hit率':>10} {'cache_write>0占比':>14}")
for d in sorted(daily):
    v = daily[d]
    n = len(v)
    itoks = sorted(x[0] for x in v)
    p50, p90 = itoks[n//2], itoks[int(n*0.9)]
    over200k = sum(1 for x in v if x[0] > 200000) / n
    ttfbs = sorted(x[1] for x in v)
    t50 = ttfbs[n//2]
    hit_rate = sum(1 for x in v if x[2] > 1000) / n
    write_rate = sum(1 for x in v if x[3] > 1000) / n
    print(f"{d:>8} {n:>7} {p50:>10.0f} {p90:>10.0f} {over200k*100:>8.0f}% {t50:>8.0f} {hit_rate*100:>9.0f}% {write_rate*100:>13.0f}%")

# 逐小时输入构成（最近 24h）：>200k 请求的占比变化
print("\n== 最近 24h 逐小时：输入构成与缓存 ==")
hourly = defaultdict(list)
for ts, itok, ttfb, cr, cw, ot in rows:
    h = ts // 3600
    if h * 3600 < tmax - 24*3600:
        continue
    hourly[h].append((itok, ttfb, cr))
print(f"{'hour':>10} {'n':>6} {'in_p50':>8} {'>200k占比':>9} {'ttfb_p50':>9} {'miss率':>7}")
for h in sorted(hourly):
    v = hourly[h]
    if len(v) < 10:
        continue
    n = len(v)
    itoks = sorted(x[0] for x in v)
    over200k = sum(1 for x in v if x[0] > 200000) / n
    ttfbs = sorted(x[1] for x in v)
    miss = sum(1 for x in v if x[2] <= 1000) / n
    print(f"{_t.strftime('%m-%d %H:00', _t.gmtime(h*3600)):>10} {n:>6} {itoks[n//2]:>8.0f} {over200k*100:>8.0f}% {ttfbs[n//2]:>8.0f} {miss*100:>6.0f}%")

# TTFB 尾部：p95/p99 逐日——"突然变大"可能是尾部恶化而非中位数
print("\n== 逐日 TTFB 尾部（全量）==")
print(f"{'day':>8} {'n':>7} {'p50':>7} {'p90':>7} {'p95':>7} {'p99':>7}")
for d in sorted(daily):
    ttfbs = sorted(x[1] for x in daily[d])
    n = len(ttfbs)
    print(f"{d:>8} {n:>7} {ttfbs[n//2]:>7.0f} {ttfbs[int(n*0.9)]:>7.0f} {ttfbs[int(n*0.95)]:>7.0f} {ttfbs[int(n*0.99)]:>7.0f}")
