#!/usr/bin/env python3
"""深挖 deepseek-v4.1-flash 在分时费率下的残差异常（Chebyshev 超包络 |r| > 0.005）。

目的：判断超包络是"少量异常行"（配对/口径问题）还是"真正的第四个定价维度"
（例如按上下文长度分档）。会逐维排查：输入规模、输出规模、缓存命中率、
账号、时间。

用法：
    python tools/diagnose_residuals.py --billing <csv> --db <db> --model deepseek-v4.1-flash
"""
from __future__ import annotations

import argparse
import csv
import datetime as dt
import sys
from collections import Counter
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fit_peak_offpeak import bucket_of  # noqa: E402
from refit_rates_from_billing import CAP, TZ, build_pairs, features  # noqa: E402


def load_local_with_account(db: Path, since: dt.datetime) -> list[dict]:
    """与 refit_rates_from_billing.load_local 同口径，但额外带 account_uid。"""
    import sqlite3

    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    cur = con.cursor()
    cur.execute("""SELECT id, created_at, model, status, input_tokens, output_tokens,
                          cache_read_tokens, cache_write_tokens,
                          credits_consumed, credit_source, account_uid,
                          mode, route
                   FROM request_logs WHERE created_at >= ? ORDER BY created_at""",
                (int(since.timestamp()),))
    rows = [dict(r) for r in cur.fetchall()]
    con.close()
    return rows

# 分时费率（积分/1K），来自 tools/fit_peak_offpeak.py 的实测结果。
RATES = {
    "deepseek-v4.1-flash": {
        "offpeak": np.array([0.0142857, 0.00028586, 0.0571270]),
        "peak": np.array([0.0285727, 0.00057138, 0.1142327]),
    },
    "glm-5.3": {
        "offpeak": np.array([0.1600, 0.0400, 0.5599]),
        "peak": np.array([0.1600, 0.0400, 0.5600]),
    },
}


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

    rows = []
    for l, b in pairs:
        if l["model"] != args.model:
            continue
        inp = int(l["input_tokens"] or 0)
        out = int(l["output_tokens"] or 0)
        if inp == 0 and out == 0:
            continue
        ts = dt.datetime.fromtimestamp(l["created_at"], TZ)
        bucket = bucket_of(ts)
        x = np.array(features(l), float)
        p = RATES[args.model][bucket]
        pred = float(x @ p / 1000.0)
        obs = float(b["credit"] or 0)
        rows.append({
            "resid": obs - pred,
            "miss": int(x[0]), "hit": int(x[1]), "out": int(x[2]),
            "pred": pred, "obs": obs,
            "bucket": bucket,
            "uid": l["account_uid"], "status": l["status"],
            "ts": ts, "source": l["credit_source"],
        })

    r = np.array([x["resid"] for x in rows])
    print(f"{args.model}: n={len(rows)}  |resid|>0.005 的行 = {(np.abs(r) > CAP).sum()}"
          f"（{100 * (np.abs(r) > CAP).mean():.2f}%）")
    print(f"resid: mean={r.mean():+.5f}  std={r.std(ddof=3):.5f}  "
          f"min={r.min():+.4f}  max={r.max():+.4f}")

    out_rows = [x for x in rows if abs(x["resid"]) > CAP]
    print(f"\n超包络行的 obs - pred 分布（前 15 条，按 |resid| 降序）")
    for x in sorted(out_rows, key=lambda v: -abs(v["resid"]))[:15]:
        print(f"  resid={x['resid']:+8.4f}  miss={x['miss']:7d} hit={x['hit']:7d} "
              f"out={x['out']:6d}  pred={x['pred']:7.4f} obs={x['obs']:6.2f} "
              f"{x['bucket']:7s} {x['ts']:%m-%d %H:%M} uid={x['uid'][:8]}")

    # 逐维排查：哪一维能解释超包络？
    print("\n--- 按输入规模分档 ---")
    for lo, hi in ((0, 8_000), (8_000, 32_000), (32_000, 64_000), (64_000, 128_000),
                   (128_000, 10**9)):
        sel = [x for x in rows if lo <= x["miss"] < hi]
        if not sel:
            continue
        rr = np.array([x["resid"] for x in sel])
        bad = (np.abs(rr) > CAP).mean() * 100
        print(f"  miss [{lo:>7d},{hi:>9d}) n={len(sel):5d}  mean={rr.mean():+.5f} "
              f"std={rr.std():.5f}  超包络 {bad:5.2f}%")

    print("\n--- 按输出规模分档 ---")
    for lo, hi in ((0, 200), (200, 1000), (1000, 4000), (4000, 10**9)):
        sel = [x for x in rows if lo <= x["out"] < hi]
        if not sel:
            continue
        rr = np.array([x["resid"] for x in sel])
        bad = (np.abs(rr) > CAP).mean() * 100
        print(f"  out  [{lo:>7d},{hi:>9d}) n={len(sel):5d}  mean={rr.mean():+.5f} "
              f"std={rr.std():.5f}  超包络 {bad:5.2f}%")

    print("\n--- 按账号 top10（超包络行数） ---")
    cnt = Counter(x["uid"][:8] for x in out_rows)
    for uid, n in cnt.most_common(10):
        tot = sum(1 for x in rows if x["uid"][:8] == uid)
        print(f"  {uid} 超包络 {n:4d} / 总 {tot:5d}  ({100 * n / max(tot, 1):5.2f}%)")

    print("\n--- 按小时（观察是否与时段边界有关） ---")
    cnt = Counter(x["ts"].hour for x in out_rows)
    for h in sorted(cnt):
        tot = sum(1 for x in rows if x["ts"].hour == h)
        print(f"  {h:02d}时 超包络 {cnt[h]:4d} / 总 {tot:5d}  ({100 * cnt[h] / max(tot, 1):5.2f}%)")

    print("\n--- credit_source 分布 ---")
    print(" ", Counter(x["source"] for x in rows))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
