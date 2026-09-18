#!/usr/bin/env python3
"""用本地记录的**上游真实积分**直接拟合费率（不依赖账单配对）。

## 为什么这是更强的证据

`tools/refit_rates_from_billing.py` 必须先做「账单 ↔ 本地日志」配对，
而配对依赖 UUID 后缀 + 同分钟 + 模型一致。实测配对真值校验只有 96.38%，
也就是说约 3.6% 的配对是错的 —— 这些错配对会污染残差（表现为极少数量级的
巨大离群值），使 Chebyshev 包络判定 `cap_ok=false`。

但本地 `request_logs` 里 `credit_source='upstream'` 的行**本身就带着上游返回的
`usage.credit`**（与原账单同精度 0.01、同数值）。直接用这些行拟合：

- 零配对误差；
- 无需账单导出；
- 样本量与"配对成功"的行同量级。

代价：只能覆盖上游确实返回了 credit 的请求（本地 9497/10083 行），
以及**本地无日志的外部消费**（IDE/手机端）看不到。

## 附带诊断

对每一条"本地 credit 有真值、但与账单配对的 credit 不一致"的行，检查同一账号
同一分钟同模型下是否存在**另一条账单行**与本地真值吻合。若吻合 → 认定为配对
错位（而非上游异常），并给出错配率。

用法：

    python tools/fit_from_local_credit.py \\
        --db billing-export-0914/metrics-20260914.db \\
        --out billing-export-0914/rates_from_local_credit.json
"""
from __future__ import annotations

import argparse
import csv
import datetime as dt
import json
import sqlite3
import sys
from collections import Counter, defaultdict
from pathlib import Path

import numpy as np
from scipy import stats

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fit_peak_offpeak import bucket_of, fit  # noqa: E402
from refit_rates_from_billing import CAP, ROUND_STD, TZ, features  # noqa: E402

TAU = 0.05  # 峰值/空闲判定：观测/预测落在期望倍率的 ±5% 内


def load_local(db: Path, since: dt.datetime) -> list[dict]:
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    cur = con.cursor()
    cur.execute("""SELECT id, created_at, model, status, input_tokens, output_tokens,
                          cache_read_tokens, cache_write_tokens,
                          credits_consumed, credit_source, account_uid
                   FROM request_logs WHERE created_at >= ? ORDER BY created_at""",
                (int(since.timestamp()),))
    rows = [dict(r) for r in cur.fetchall()]
    con.close()
    return rows


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--billing", default="", help="可选：用于错配诊断")
    ap.add_argument("--out", default="rates_from_local_credit.json")
    ap.add_argument("--min-samples", type=int, default=30)
    ap.add_argument("--lookback-days", type=int, default=30)
    args = ap.parse_args()

    since = dt.datetime.now(TZ) - dt.timedelta(days=args.lookback_days)
    local = load_local(Path(args.db), since)

    usable = [r for r in local if r["credit_source"] == "upstream"]
    # 零 token 行没有 token 特征 → 排除
    usable = [r for r in usable
              if int(r["input_tokens"] or 0) > 0 or int(r["output_tokens"] or 0) > 0]
    print(f"本地 {len(local)} 行；credit_source=upstream 且 token 非零 {len(usable)} 行 "
          f"（积分合计 {sum(float(r['credits_consumed'] or 0) for r in usable):.2f}）")

    by_model: dict[str, list[dict]] = defaultdict(list)
    for r in usable:
        by_model[r["model"]].append(r)

    # ---- 全量（不分时段）拟合，用于确认"单一费率明显不成立" ----
    results: dict[str, dict] = {}
    for model, rows in sorted(by_model.items(), key=lambda kv: -len(kv[1])):
        if len(rows) < args.min_samples:
            results[model] = {"n": len(rows), "status": "insufficient_samples"}
            continue
        X = np.array([features(r) for r in rows], float)
        y = np.array([float(r["credits_consumed"] or 0) for r in rows], float)
        whole = fit(X, y)

        buckets: dict[str, dict] = {}
        for bucket in ("peak", "offpeak"):
            sel = [r for r in rows if bucket_of(dt.datetime.fromtimestamp(r["created_at"], TZ)) == bucket]
            if len(sel) < args.min_samples:
                buckets[bucket] = {"n": len(sel), "status": "insufficient_samples"}
                continue
            Xb = np.array([features(r) for r in sel], float)
            yb = np.array([float(r["credits_consumed"] or 0) for r in sel], float)
            buckets[bucket] = {"n": len(sel), "status": "ok", **fit(Xb, yb)}

        pk, op = buckets.get("peak", {}), buckets.get("offpeak", {})
        ratio = None
        if pk.get("status") == "ok" and op.get("status") == "ok":
            ratio = [pk["rates_per_1k"][i] / op["rates_per_1k"][i]
                     if op["rates_per_1k"][i] else None for i in range(3)]

        results[model] = {
            "n": len(rows),
            "credit_sum": float(y.sum()),
            "status": "ok",
            "whole_sample_fit": whole,
            "by_bucket": buckets,
            "peak_over_offpeak": ratio,
        }

        print(f"\n{model}  n={len(rows)}  积分={y.sum():.2f}")
        w = whole["rates_per_1k"]
        print(f"  不分时段: in={w[0] * 1000:9.4f} cr={w[1] * 1000:9.5f} out={w[2] * 1000:9.4f}  "
              f"std={whole['residual_std']:.5f}(比 {whole['std_ratio']:.3f})"
              f"{' ✅' if whole['pure_rounding_consistent'] else ' ⚠️ 说明单费率不成立'}")
        for bucket in ("peak", "offpeak"):
            e = buckets[bucket]
            if e.get("status") != "ok":
                print(f"  {bucket:8s} 样本不足 (n={e.get('n', 0)})")
                continue
            r = e["rates_per_1k"]
            print(f"  {bucket:8s} n={e['n']:5d}（剔除 {e['n_dropped']:4d}）  "
                  f"in={r[0] * 1000:9.4f} cr={r[1] * 1000:9.5f} out={r[2] * 1000:9.4f}  "
                  f"std={e['residual_std']:.5f}(比 {e['std_ratio']:.3f})"
                  f"{' ✅' if e['pure_rounding_consistent'] else ' ⚠️'}  "
                  f"t_max={e['chebyshev_t_max']:.6f}"
                  f"{' ✅ 在包络内' if e['cap_ok'] else ' ⚠️ 超包络'}")
        if ratio:
            print(f"  高峰/空闲倍率: in={ratio[0]:.4f}  cr={ratio[1]:.4f}  out={ratio[2]:.4f}")

    # ---- 错配诊断（可选） ----
    mispair = None
    if args.billing:
        mispair = diagnose_mispairing(Path(args.billing), local)
        print("\n--- 配对错位诊断 ---")
        for k, v in mispair.items():
            print(f"  {k}: {v}")

    Path(args.out).write_text(json.dumps({
        "generated_at": dt.datetime.now(TZ).strftime("%Y-%m-%d %H:%M:%S %z"),
        "method": "直接用本地 request_logs.credits_consumed（credit_source=upstream），零配对误差",
        "db": Path(args.db).name,
        "rows_local": len(local),
        "rows_upstream_credit": len(usable),
        "peak_definition": "周一~周五 09:00-12:00 与 14:00-18:00（北京时间）",
        "rounding_noise_std_theory": ROUND_STD,
        "units": "rates_per_1k = 积分/1K token；rates_per_million = 积分/百万 token",
        "mispairing_diagnosis": mispair,
        "models": results,
    }, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n产出: {args.out}")
    return 0


def _suffix(rid: str) -> str:
    return rid.split("-", 1)[1][8:] if rid and "-" in rid else ""


def diagnose_mispairing(billing: Path, local: list[dict]) -> dict:
    """统计"本地真值与配对账单不符"的行里，有多少能在同分钟找到真正的账单伙伴。"""
    bill = list(csv.DictReader(open(billing, encoding="utf-8-sig")))
    loc_by_suffix = defaultdict(list)
    for r in local:
        loc_by_suffix[_suffix(r["id"])].append(r)
    bill_by_suffix = defaultdict(list)
    for b in bill:
        bill_by_suffix[_suffix(b["request_id"])].append(b)

    # 同（账号, 分钟, 模型）下的全部账单行，用于找真正伙伴
    by_key = defaultdict(list)
    for b in bill:
        by_key[(b["account_uid"], (b["request_time"] or "")[:16], b["model"])].append(b)

    st = Counter()
    recovered = 0
    truth_rows = [r for r in local if r["credit_source"] == "upstream"
                  and float(r["credits_consumed"] or 0) > 0]
    for l in truth_rows:
        rows = loc_by_suffix.get(_suffix(l["id"]))
        brows = bill_by_suffix.get(_suffix(l["id"]))
        if not rows or not brows:
            st["no_pair"] += 1
            continue
        paired = None
        for b in brows:
            if b["model"] == l["model"]:
                paired = b
                break
        if paired is None:
            st["model_mismatch"] += 1
            continue
        if abs(float(l["credits_consumed"]) - float(paired["credit"] or 0)) < 1e-9:
            st["exact"] += 1
            continue
        st["mismatch"] += 1
        # 在同（账号, 分钟, 模型）里找真正伙伴
        lt = dt.datetime.fromtimestamp(l["created_at"], TZ)
        key = (l["account_uid"], lt.strftime("%Y-%m-%d %H:%M"), l["model"])
        others = [b for b in by_key.get(key, []) if b is not paired]
        if any(abs(float(l["credits_consumed"]) - float(b["credit"] or 0)) < 1e-9
               for b in others):
            recovered += 1
            st["mismatch_recovered_as_swap"] += 1

    return {
        "upstream_truth_rows": len(truth_rows),
        "exact_match": st["exact"],
        "exact_match_pct": round(100 * st["exact"] / max(len(truth_rows), 1), 3),
        "mismatch": st["mismatch"],
        "mismatch_share_of_mismatch_recovered": round(100 * recovered / max(st["mismatch"], 1), 2),
        "counts": dict(st),
    }


if __name__ == "__main__":
    raise SystemExit(main())
