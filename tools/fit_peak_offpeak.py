#!/usr/bin/env python3
"""按 DeepSeek 高峰/空闲时段分桶重拟合积分费率。

背景（见 billing-export/REFIT-REPORT.md 第 148~149 行）：
DeepSeek 官方分时定价 —— 高峰 = 周一~周五 09:00-12:00 与 14:00-18:00（北京时间），
其余时段半价。上一轮账单窗口（2026-09-12 周六 ~ 09-13 周日）全是周末，
只有一个档位，所以单表能拟合。一旦窗口跨入工作日，同一模型的请求就落在两个
价位上，用单一费率拟合会出现**系统性残差**（std 远超纯舍入噪声 0.002887），
表现为大量样本被迭代重加权剔除。

本脚本复用 refit_rates_from_billing.py 的配对与拟合原语，按 (模型, 时段) 分桶，
对比两档费率是否恰好相差 2 倍。

用法：

    python tools/fit_peak_offpeak.py \\
        --billing billing-export-0914/billing-20260914-234747.csv \\
        --db billing-export-0914/metrics-20260914.db \\
        --out billing-export-0914/rates_peak_offpeak.json
"""
from __future__ import annotations

import argparse
import csv
import datetime as dt
import json
import sys
from collections import defaultdict
from pathlib import Path

import numpy as np
from scipy import stats

sys.path.insert(0, str(Path(__file__).resolve().parent))

from refit_rates_from_billing import (  # noqa: E402
    CAP,
    ROUND_STD,
    TZ,
    build_pairs,
    chebyshev,
    features,
    load_local,
    param_spans,
)

# DeepSeek 官方高峰时段（北京时间，周一~周五）。
PEAK_WINDOWS = ((9, 12), (14, 18))


def bucket_of(ts: dt.datetime) -> str:
    """把请求时刻归入 'peak' 或 'offpeak'。"""
    if ts.weekday() >= 5:
        return "offpeak"
    hour = ts.hour + ts.minute / 60.0
    for lo, hi in PEAK_WINDOWS:
        if lo <= hour < hi:
            return "peak"
    return "offpeak"


def fit(X: np.ndarray, y: np.ndarray) -> dict:
    """迭代重加权最小二乘 + 残差诊断（与 refit 脚本同口径）。"""
    keep = np.ones(len(y), bool)
    for _ in range(8):
        p, *_ = np.linalg.lstsq(X[keep], y[keep], rcond=None)
        nk = np.abs(y - X @ p) <= 0.02
        if nk.sum() == keep.sum():
            break
        keep = nk
    Xk, yk = X[keep], y[keep]
    p, *_ = np.linalg.lstsq(Xk, yk, rcond=None)
    resid = yk - Xk @ p
    sd = float(resid.std(ddof=3))
    ks = stats.kstest(resid, "uniform", args=(-CAP, 2 * CAP))
    _, tmax = chebyshev(Xk, yk)
    sp = param_spans(Xk, yk)
    return {
        "n_used": int(keep.sum()),
        "n_dropped": int((~keep).sum()),
        "credit_sum": float(yk.sum()),
        "rates_per_1k": [float(v * 1000) for v in p],
        "residual_std": sd,
        "std_ratio": sd / ROUND_STD,
        "pure_rounding_consistent": bool(abs(sd / ROUND_STD - 1) < 0.15),
        "ks_pvalue_uniform": float(ks.pvalue),
        "chebyshev_t_max": float(tmax) if tmax is not None else None,
        "cap_ok": bool(tmax is not None and tmax <= CAP + 1e-9),
        "lp_intervals_per_1k": (
            {k: list(v) for k, v in zip(("input", "cache_read", "output"), sp)} if sp else None
        ),
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
    ap.add_argument("--out", default="rates_peak_offpeak.json")
    ap.add_argument("--min-samples", type=int, default=30)
    ap.add_argument("--lookback-days", type=int, default=30)
    ap.add_argument("--models", default="", help="逗号分隔；默认对所有模型分桶")
    args = ap.parse_args()

    bill = list(csv.DictReader(open(args.billing, encoding="utf-8-sig")))
    since = dt.datetime.now(TZ) - dt.timedelta(days=args.lookback_days)
    local = load_local(Path(args.db), since)
    pairs, st = build_pairs(local, bill)

    # 异常行：本地零 token 但上游仍计费（无 token 特征，必须排除）
    def usable(l, b) -> bool:
        if int(l["input_tokens"] or 0) == 0 and int(l["output_tokens"] or 0) == 0:
            return float(b["credit"] or 0) <= 0
        return True

    wanted = {m.strip() for m in args.models.split(",") if m.strip()}
    grouped: dict[str, dict[str, list]] = defaultdict(lambda: defaultdict(list))
    for l, b in pairs:
        if not usable(l, b):
            continue
        if wanted and l["model"] not in wanted:
            continue
        grouped[l["model"]][bucket_of(dt.datetime.fromtimestamp(l["created_at"], TZ))].append((l, b))

    results: dict[str, dict] = {}
    for model in sorted(grouped, key=lambda m: -sum(len(v) for v in grouped[m].values())):
        entry: dict[str, dict] = {}
        for bucket in ("peak", "offpeak"):
            rows = grouped[model].get(bucket) or []
            if len(rows) < args.min_samples:
                entry[bucket] = {"n_pairs": len(rows), "status": "insufficient_samples"}
                continue
            X = np.array([features(l) for l, _ in rows], float)
            y = np.array([float(b["credit"] or 0) for _, b in rows], float)
            entry[bucket] = {"n_pairs": len(rows), "status": "ok", **fit(X, y)}

        pk, op = entry.get("peak", {}), entry.get("offpeak", {})
        if pk.get("status") == "ok" and op.get("status") == "ok":
            entry["peak_over_offpeak"] = [
                pk["rates_per_1k"][i] / op["rates_per_1k"][i] if op["rates_per_1k"][i] else None
                for i in range(3)
            ]
        results[model] = entry

        print(f"\n{model}")
        for bucket in ("peak", "offpeak"):
            e = entry[bucket]
            if e.get("status") != "ok":
                print(f"  {bucket:8s} 样本不足 (n={e.get('n_pairs', 0)})")
                continue
            r = e["rates_per_1k"]
            print(f"  {bucket:8s} n={e['n_used']:5d}（剔除 {e['n_dropped']:4d}）  "
                  f"积分/百万 token: in={r[0] * 1000:9.4f}  cr={r[1] * 1000:9.5f}  "
                  f"out={r[2] * 1000:9.4f}  "
                  f"std={e['residual_std']:.5f}(比 {e['std_ratio']:.3f})"
                  f"{' ✅' if e['pure_rounding_consistent'] else ' ⚠️'}  "
                  f"t_max={e['chebyshev_t_max']:.6f}"
                  f"{' ✅' if e['cap_ok'] else ' ⚠️'}")
        ratio = entry.get("peak_over_offpeak")
        if ratio:
            print(f"  高峰/空闲倍率: in={ratio[0]:.4f}  cr={ratio[1]:.4f}  out={ratio[2]:.4f}")

    Path(args.out).write_text(
        json.dumps(
            {
                "generated_at": dt.datetime.now(TZ).strftime("%Y-%m-%d %H:%M:%S %z"),
                "source": {"billing_csv": Path(args.billing).name, "billing_rows": len(bill),
                           "local_rows": len(local)},
                "pairing_stats": dict(st),
                "peak_definition": "周一~周五 09:00-12:00 与 14:00-18:00（北京时间）",
                "rounding_noise_std_theory": ROUND_STD,
                "units": "rates_per_1k = 积分/1K token",
                "models": results,
            },
            ensure_ascii=False,
            indent=2,
        ),
        encoding="utf-8",
    )
    print(f"\n产出: {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
