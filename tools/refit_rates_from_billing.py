#!/usr/bin/env python3
"""用真实账单重拟合积分费率（credit-rate-inference.md §8 的漂移监控）。

## 为什么需要配对

账单接口 `/billing/meter/get-user-request-usage` 的返回行**只有**
`requestId / credit / model / client / requestTime / inputTrunc / input / agentPurpose`
—— **没有 token 字段**。所以无法脱离本地 `request_logs` 直接拟合，必须配对。

## 配对键（本脚本的核心发现）

账单 id `crb-<hex>` 与本地 id `cmb-<hex>` 的**后 24 个 hex 完全相同**
（= UUIDv1 的 `time_mid + time_hi + clock_seq + node`），仅前 8 个 hex
（`time_low`，亚微秒计数器）不同，且本地值恒大于账单值。

因此配对是**精确**的，不是"UUIDv1 时刻 ±3 秒 + 模型名"那种统计近似。
真值校验：本地 `credit_source='upstream'` 的行两边都有真实 credit，
其中 95.95% 逐条**完全相等**（差额非零的几行是上游重试/合并导致）。

> 注意：旧文档只比较过整串 id，得出"两个命名空间无交集"的结论 —— 那是对的，
> 但会让人误以为只能靠时间近似配对。按后缀配对把可用样本从 60 条提升到 4000+ 条。

## 必须排除的异常

本地 `status != 200` 且 token 全 0、但账单仍扣费的行：这类行没有任何 token
特征，塞进回归只会污染系数。脚本自动识别并单独报告。

## 用法

    python tools/refit_rates_from_billing.py \\
        --billing billing-export/billing-YYYYmmdd-HHMMSS.csv \\
        --db path/to/metrics.db \\
        --out billing-export/rates_refit_final.json

依赖：numpy、scipy。
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
from scipy.optimize import linprog

TZ = dt.timezone(dt.timedelta(hours=8))
CAP = 0.005                      # 上游 credit 舍入到 0.01 → 半宽
ROUND_STD = 0.01 / np.sqrt(12)   # 纯舍入噪声的理论标准差 ≈ 0.002887

# 旧费率（积分/1K token），来自 docs/probes/rates_from_credit.json 与 rate_model.json。
# 用于漂移对比；缺失的模型只报告新值。
OLD_RATES = {
    "deepseek-v4.1-flash": (0.014235874682268753, 0.00028548701144981436, 0.057008628369075255),
    "deepseek-v4-pro": (0.12866811244395474, 0.003920128572986988, 0.3802914619418006),
    "glm-5.1": (0.11969842330110805, 0.025734253559903775, 0.4768478132661705),
    "glm-5.2": (0.114743, 0.023765, 0.387152),
    "glm-5.3-flash": (0.011192, 0.002604, 0.049670),
    "kimi-k2.6": (0.139693, 0.023482, 0.536406),
    "kimi-k3": (0.485557, 0.048248, 2.431957),
    "minimax-m3": (0.059829, 0.011615, 0.231311),
}


def hexpart(rid: str) -> str:
    return rid.split("-", 1)[1] if rid and "-" in rid else (rid or "")


def load_local(db: Path, since: dt.datetime):
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    cur = con.cursor()
    cur.execute("""SELECT id, created_at, model, status, input_tokens, output_tokens,
                          cache_read_tokens, cache_write_tokens,
                          credits_consumed, credit_source
                   FROM request_logs WHERE created_at >= ? ORDER BY created_at""",
                (int(since.timestamp()),))
    rows = [dict(r) for r in cur.fetchall()]
    con.close()
    return rows


def features(rec: dict) -> tuple[int, int, int]:
    """(未缓存输入 miss, 缓存读 hit, 输出 out)。

    本代理 `cache_write_tokens` 列实际存的是**未缓存输入**（上游语义），
    兼容 `input == hit + miss` 的情形。
    """
    inp = max(int(rec["input_tokens"] or 0), 0)
    hit = max(int(rec["cache_read_tokens"] or 0), 0)
    cw = max(int(rec["cache_write_tokens"] or 0), 0)
    return (cw if cw > 0 else max(inp - hit, 0),
            hit,
            max(int(rec["output_tokens"] or 0), 0))


def build_pairs(local, bill):
    """按 UUID 后缀精确配对；后缀碰撞时用（分钟, 模型）消歧。"""
    loc, bil = defaultdict(list), defaultdict(list)
    for r in local:
        h = hexpart(r["id"])
        if len(h) == 32:
            loc[h[8:]].append(r)
    for r in bill:
        h = hexpart(r["request_id"])
        if len(h) == 32:
            bil[h[8:]].append(r)

    pairs, st = [], Counter()
    for suf, brows in bil.items():
        lrows = loc.get(suf)
        if not lrows:
            st["bill_only"] += len(brows)
            continue
        if len(lrows) == 1 and len(brows) == 1:
            cand = [(lrows[0], brows[0])]
        else:
            cand, used = [], set()
            for b in brows:
                bmin = (b["request_time"] or "")[:16]
                hit = [l for l in lrows if l["model"] == b["model"]
                       and dt.datetime.fromtimestamp(l["created_at"], TZ).strftime("%Y-%m-%d %H:%M") == bmin
                       and id(l) not in used]
                if len(hit) == 1:
                    used.add(id(hit[0]))
                    cand.append((hit[0], b))
                else:
                    st["ambiguous"] += 1
        for l, b in cand:
            lt = dt.datetime.fromtimestamp(l["created_at"], TZ)
            bt = dt.datetime.strptime(b["request_time"], "%Y-%m-%d %H:%M:%S").replace(tzinfo=TZ)
            if round((lt.replace(second=0, microsecond=0) - bt).total_seconds() / 60) not in (-1, 0):
                st["time_reject"] += 1
                continue
            if l["model"] != b["model"]:
                st["model_reject"] += 1
                continue
            pairs.append((l, b))
    st["paired"] = len(pairs)
    return pairs, st


def chebyshev(X, y):
    """舍入包络 |Xp - y| <= t 的 Chebyshev 中心（最小化 t）。

    注意：不要给 t 加"自由 slack 列并最大化"——那样问题无界（HiGHS status=3），
    会被误读成"不可行"。
    """
    n, k = X.shape
    A = np.vstack([np.hstack([X, -np.ones((n, 1))]),
                   np.hstack([-X, -np.ones((n, 1))])])
    b = np.concatenate([y, -y])
    c = np.zeros(k + 1)
    c[-1] = 1
    r = linprog(c, A_ub=A, b_ub=b, bounds=[(0, None)] * (k + 1), method="highs")
    return (r.x[:k], r.x[-1]) if r.success else (None, None)


def param_spans(X, y, cap=CAP):
    """各参数的可行区间 min/max（无自由 slack 列）。"""
    n, k = X.shape
    A = np.vstack([X, -X])
    b = np.concatenate([y + cap, -y + cap])
    out = []
    for j in range(k):
        v = []
        for sign in (1, -1):
            c = np.zeros(k)
            c[j] = sign
            r = linprog(c, A_ub=A, b_ub=b, bounds=[(0, None)] * k, method="highs")
            if not r.success:
                return None
            v.append(r.x[j])
        out.append((min(v), max(v)))
    return out


def main() -> int:
    # Windows 控制台默认 GBK，无法编码 ✅/⚠️ 等符号 → 强制 UTF-8，出错也不崩
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser()
    ap.add_argument("--billing", required=True, help="账单 CSV（fetch_billing.py 的产出）")
    ap.add_argument("--db", required=True, help="metrics.db 路径")
    ap.add_argument("--out", default="rates_refit_final.json")
    ap.add_argument("--min-samples", type=int, default=30)
    ap.add_argument("--lookback-days", type=int, default=30)
    args = ap.parse_args()

    billing = Path(args.billing)
    bill = list(csv.DictReader(open(billing, encoding="utf-8-sig")))
    since = dt.datetime.now(TZ) - dt.timedelta(days=args.lookback_days)
    local = load_local(Path(args.db), since)

    pairs, st = build_pairs(local, bill)
    truth = [(l, b) for l, b in pairs if l["credit_source"] == "upstream"]
    exact = sum(1 for l, b in truth
                if abs(float(l["credits_consumed"] or 0) - float(b["credit"] or 0)) < 1e-9)

    print(f"本地 {len(local)} 行 / 账单 {len(bill)} 行 → 配对 {len(pairs)} "
          f"（覆盖 {len(pairs)/len(bill)*100:.1f}% of 账单）")
    print(f"配对明细: {dict(st)}")
    if truth:
        print(f"配对键真值校验: {exact}/{len(truth)} = {exact/len(truth)*100:.2f}% "
              f"逐条 credit 完全相等")

    # 异常：本地零 token 但账单收费（上游重试/失败仍计费）
    zero_tok = [(l, b) for l, b in pairs
                if int(l["input_tokens"] or 0) == 0 and int(l["output_tokens"] or 0) == 0
                and float(b["credit"] or 0) > 0]
    zt_credit = sum(float(b["credit"] or 0) for _, b in zero_tok)
    if zero_tok:
        print(f"异常（零 token 仍计费）: {len(zero_tok)} 行 / {zt_credit:.2f} 积分 → 已排除")

    by_model = defaultdict(list)
    for l, b in pairs:
        by_model[l["model"]].append((l, b))

    results = {}
    for model, rows in sorted(by_model.items(), key=lambda x: -len(x[1])):
        clean = [(l, b) for l, b in rows
                 if not (int(l["input_tokens"] or 0) == 0
                         and int(l["output_tokens"] or 0) == 0
                         and float(b["credit"] or 0) > 0)]
        if len(clean) < args.min_samples:
            results[model] = {"n_pairs": len(rows), "n_used": len(clean),
                              "status": "insufficient_samples"}
            continue

        X = np.array([features(l) for l, _ in clean], float)
        y = np.array([float(b["credit"] or 0) for _, b in clean], float)

        # 迭代重加权：剔除无法被 3 参数模型解释的行（配对/口径异常）
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
        dev = np.abs(resid)
        per_1k = p * 1000.0
        sd = float(resid.std(ddof=3))
        ks = stats.kstest(resid, "uniform", args=(-CAP, 2 * CAP))
        _, tmax = chebyshev(Xk, yk)
        sp = param_spans(Xk, yk)

        old = OLD_RATES.get(model)
        drift = None
        if old:
            drift = {"input_pct": (per_1k[0] - old[0]) / old[0] * 100,
                     "cache_read_pct": (per_1k[1] - old[1]) / old[1] * 100,
                     "output_pct": (per_1k[2] - old[2]) / old[2] * 100,
                     "old_per_1k": list(old)}

        results[model] = {
            "n_pairs": len(rows), "n_used": int(keep.sum()),
            "n_dropped": int((~keep).sum()), "status": "ok",
            "credit_sum_used": float(yk.sum()),
            "rates_per_1k": {"input": float(per_1k[0]), "cache_read": float(per_1k[1]),
                             "output": float(per_1k[2])},
            "rates_per_million": {"input": float(per_1k[0] * 1000),
                                  "cache_read": float(per_1k[1] * 1000),
                                  "output": float(per_1k[2] * 1000)},
            "residual": {"mae": float(dev.mean()), "max": float(dev.max()), "std": sd,
                         "theory_rounding_std": ROUND_STD, "std_ratio": sd / ROUND_STD,
                         "ks_pvalue_uniform": float(ks.pvalue),
                         "pure_rounding_consistent": bool(abs(sd / ROUND_STD - 1) < 0.15)},
            "chebyshev": {"t_max": float(tmax) if tmax is not None else None,
                          "cap_ok": bool(tmax is not None and tmax <= CAP + 1e-9)},
            "lp_intervals_per_1k": ({k: list(v) for k, v in
                                     zip(("input", "cache_read", "output"), sp)} if sp else None),
            "old_rates_per_1k": list(old) if old else None,
            "drift_pct": drift,
        }

        r = results[model]
        print(f"\n{model}  n={r['n_used']}（剔除 {r['n_dropped']}）  积分合计={yk.sum():.2f}")
        print(f"  积分/百万 token: in={per_1k[0]*1000:.4f}  cr={per_1k[1]*1000:.5f}  "
              f"out={per_1k[2]*1000:.4f}")
        print(f"  残差 std={sd:.5f}（纯舍入理论 {ROUND_STD:.5f}，比 {sd/ROUND_STD:.3f}）"
              f" → {'纯舍入噪声 ✅' if abs(sd/ROUND_STD-1) < 0.15 else '存在结构偏差 ⚠️'}")
        if tmax is not None:
            print(f"  Chebyshev t_max={tmax:.6f} "
                  f"{'✅ 在包络内' if tmax <= CAP + 1e-9 else '⚠️ 略超包络'}")
        if drift:
            print(f"  漂移: in={drift['input_pct']:+.2f}%  cr={drift['cache_read_pct']:+.2f}%  "
                  f"out={drift['output_pct']:+.2f}%")

    out = {
        "generated_at": dt.datetime.now(TZ).strftime("%Y-%m-%d %H:%M:%S %z"),
        "source": {"billing_csv": billing.name, "billing_rows": len(bill),
                   "local_rows": len(local)},
        "pairing_method": "crb-/cmb- 共享 UUID 后缀（后 24 hex）+ 同分钟 + 模型一致",
        "pairing_validation": {"checked": len(truth), "exact_match": exact,
                               "exact_match_pct": (exact / len(truth) * 100) if truth else None},
        "pairing_stats": dict(st),
        "anomaly_zero_token_billed": {"rows": len(zero_tok), "credits": zt_credit},
        "rounding_cap": CAP, "rounding_noise_std_theory": ROUND_STD,
        "units": "rates_per_1k = 积分/1K token；rates_per_million = 积分/百万 token",
        "models": results,
    }
    Path(args.out).write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n产出: {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
