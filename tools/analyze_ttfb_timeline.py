# -*- coding: utf-8 -*-
"""TTFB 时间轴：找出"突然变大"的拐点。
按天/小时聚合 deepseek-v4.1-flash 的 TTFB，控制输入长度（只看同一输入桶），
排除"流量形态变化"的干扰。"""
import sqlite3
from collections import defaultdict

con = sqlite3.connect("metrics-007.db")
cur = con.cursor()

tmin, tmax = cur.execute("select min(created_at), max(created_at) from request_logs").fetchone()
import time as _t
print(f"数据范围: {_t.strftime('%m-%d %H:%M', _t.gmtime(tmin))} .. {_t.strftime('%m-%d %H:%M', _t.gmtime(tmax))} UTC")

rows = list(cur.execute("""
    select created_at, input_tokens, ttfb_millis, cache_read_tokens, latency_millis, output_tokens
    from request_logs
    where mode='stream' and status=200 and model='deepseek-v4.1-flash'
      and output_tokens > 30 and ttfb_millis > 0
"""))
print(f"n={len(rows)}")

# 按天聚合：控制输入桶（60-120k，样本量最大），看 TTFB 逐日走势
def day_bucket(ts):
    return _t.strftime('%m-%d', _t.gmtime(ts))

daily = defaultdict(list)
for ts, itok, ttfb, cr, lat, ot in rows:
    if not (60000 <= itok <= 120000):
        continue
    daily[day_bucket(ts)].append(ttfb)

print("\n== 逐日 TTFB（60-120k 输入桶，控制变量）==")
print(f"{'day':>8} {'n':>7} {'p50':>8} {'p90':>8}")
for d in sorted(daily):
    v = sorted(daily[d])
    if len(v) < 10:
        continue
    print(f"{d:>8} {len(v):>7} {v[len(v)//2]:>7.0f} {v[int(len(v)*0.9)]:>7.0f}")

# 按小时聚合：最近 48h 的 TTFB 走势（同桶）
print("\n== 最近 48h 逐小时 TTFB（60-120k 桶）==")
hourly = defaultdict(list)
for ts, itok, ttfb, cr, lat, ot in rows:
    if not (60000 <= itok <= 120000):
        continue
    h = ts // 3600
    if h * 3600 < tmax - 48*3600:
        continue
    hourly[h].append((ttfb, cr, itok))
print(f"{'hour':>10} {'n':>6} {'ttfb_p50':>9} {'cache_p50':>10} {'input_p50':>10}")
for h in sorted(hourly):
    v = hourly[h]
    if len(v) < 8:
        continue
    ttfbs = sorted(x[0] for x in v)
    caches = sorted(x[1] for x in v)
    itoks = sorted(x[2] for x in v)
    print(f"{_t.strftime('%m-%d %H:00', _t.gmtime(h*3600)):>10} {len(v):>6} {ttfbs[len(ttfbs)//2]:>8.0f} {caches[len(caches)//2]:>10.0f} {itoks[len(itoks)//2]:>10.0f}")
