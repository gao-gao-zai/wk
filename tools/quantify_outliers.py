#!/usr/bin/env python3
"""最终归因量化：把"超包络"拆成 (a)真舍入边界伪影 (b)高峰边界时刻错配 (c)配对换位 (d)真异常。

(a) 真舍入边界：|obs - pred| 恰好等于 0.005（浮点上略微超出）。上游是 round 到 0.01，
    预测值落在网格边界时 |残差| 天然就是 0.005，不是异常。必须带容差判定。

(b) 高峰边界：本地 created_at 落在 xx:59，而上游按分钟粒度判定时段，
    可能被归到下一分钟的档位。检验方法：把高峰起点放宽 1 分钟重算，
    看这些行是否消失。

用法：
    python tools/quantify_outliers.py --billing <csv> --db <db>
"""
from __future__ import annotations

import argparse
import csv
import datetime as dt
import sys
from collections import Counter, defaultdict
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))

from attribute_outliers import RATES, suffix  # noqa: E402
from diagnose_residuals import load_local_with_account  # noqa: E402
from fit_peak_offpeak import PEAK_WINDOWS  # noqa: E402
from refit_rates_from_billing import CAP, TZ, build_pairs, features  # noqa: E402

EPS = 1e-9
# 拟合费率本身有微小误差 ε，它会把"真值恰好落在舍入网格边界"的行推过 0.005 包络。
# 这类行不是异常，用相对容差识别（pred 越大，同样的费率误差贡献越大）。
REL_TOL = 1e-3


def is_rounding_boundary(resid: float, pred: float) -> bool:
    """|残差| 略超 0.005，但超出量可用"预测值本身的精度"解释 → 舍入边界伪影。"""
    if abs(resid) <= CAP + EPS:
        return False
    return abs(abs(resid) - CAP) <= max(REL_TOL * pred, 2e-4)


def bucket_with_slack(ts: dt.datetime, slack_min: float) -> str:
    """高峰起点提前 slack_min 分钟的时段判定（模拟上游分钟粒度）。"""
    if ts.weekday() >= 5:
        return "offpeak"
    hour = ts.hour + ts.minute / 60.0 + slack_min / 60.0
    for lo, hi in PEAK_WINDOWS:
        if lo <= hour < hi:
            return "peak"
    return "offpeak"


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser()
    ap.add_argument("--billing", required=True)
    ap.add_argument("--db", required=True)
    ap.add_argument("--model", default="deepseek-v4.1-flash")
    ap.add_argument("--lookback-days", type=int, default=30)
    args = ap.parse_args()

    bill = list(csv.DictReader(open(args.billing, encoding="utf-8-sig")))
    since = dt.datetime.now(TZ) - dt.timedelta(days=args.lookback_days)
    local = load_local_with_account(Path(args.db), since)
    pairs, _ = build_pairs(local, bill)
    rate = RATES[args.model]

    by_key: dict[tuple, list] = defaultdict(list)
    for b in bill:
        by_key[(b["account_uid"], (b["request_time"] or "")[:16], b["model"])].append(b)

    st = Counter()
    boundary_ts = []
    for l, b in pairs:
        if l["model"] != args.model or l["credit_source"] != "upstream":
            continue
        x = np.array(features(l), float)
        if x[0] == 0 and x[2] == 0:
            continue
        ts = dt.datetime.fromtimestamp(l["created_at"], TZ)
        pred = float(x @ rate[bucket_with_slack(ts, 0)] / 1000.0)
        obs = float(b["credit"] or 0)
        truth = float(l["credits_consumed"] or 0)
        resid = obs - pred
        if abs(resid) <= CAP + EPS:
            st["正常（在包络内）"] += 1
            continue

        # (a) 舍入边界伪影：|残差| 就是 0.005（预测值本身的精度所致）
        if is_rounding_boundary(resid, pred):
            st["(a) 恰在舍入边界 ±0.005（非异常）"] += 1
            continue

        # (c) 配对换位：本地真值 != 账单，但同分钟另有账单行等于本地真值
        if abs(truth - obs) > 1e-9:
            key = (l["account_uid"], ts.strftime("%Y-%m-%d %H:%M"), l["model"])
            if any(abs(float(o["credit"] or 0) - truth) < 1e-9
                   for o in by_key.get(key, []) if o is not b):
                st["(c) 配对换位（账单行拿错）"] += 1
            else:
                st["(d) 无法归因"] += 1
            continue

        # 账单与本地真值一致 → 上游确实这么收。检查是否为高峰边界
        alt = float(x @ rate[bucket_with_slack(ts, 1)] / 1000.0)
        if abs(obs - alt) <= CAP + EPS:
            st["(b) 高峰边界时刻错配（xx:59）"] += 1
            boundary_ts.append((ts, pred, alt, obs))
        else:
            st["(d) 无法归因"] += 1

    total = sum(st.values())
    print(f"{args.model}: 配对可比行 {total}\n")
    for k, v in sorted(st.items()):
        print(f"  {k}: {v}  ({100 * v / total:.3f}%)")
    if boundary_ts:
        print("\n  (b) 明细：")
        for ts, pred, alt, obs in boundary_ts:
            print(f"    {ts:%m-%d %H:%M:%S}  空闲档预测={pred:.4f}  "
                  f"高峰档预测={alt:.4f}  实收={obs:.2f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
