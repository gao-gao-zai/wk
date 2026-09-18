#!/usr/bin/env python3
"""计算成本倍率：本反代实付成本 / 官方 API 牌价。

定义（与 billing-export/REFIT-REPORT.md §六 一致）：

    成本倍率 = (r × p) / o

其中：
- r  = 实测积分费率（积分/百万 token），取自最新一轮本地真值拟合
- p  = 实付单价（元/积分），由购买价换算，如 3.3 元 / 2000 积分 = 0.00165
- o  = 官方牌价（元/百万 token）

对未缓存输入 / 缓存命中 / 输出三种 token 类型分别计算，三者一致（离散度小）
说明该模型的官方牌价与实测结构吻合；不一致则说明牌价本身存疑。

用法：

    python tools/cost_multiplier.py                    # 默认 3.3 元 = 2000 积分
    python tools/cost_multiplier.py --yuan 3 --credits 2000
    python tools/cost_multiplier.py --yuan 10 --credits 5000
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# 实测费率（积分/百万 token）。来源：
#   billing-export-0918/rates_from_local_credit.json —— 本轮权威（空闲档）
#   billing-export-0914/rates_from_local_credit.json —— deepseek 高峰档（本轮无样本）
RATES_FILE = Path("billing-export-0918/rates_from_local_credit.json")
PEAK_FILE = Path("billing-export-0914/rates_from_local_credit.json")

# 官方牌价（元/百万 token）。来源：各厂商定价页，
# 与 tools/compare_7yuan_2000.py 的 LIST 一致。
OFFICIAL = {
    # model: (input, cache_read, output, 时段说明)
    "deepseek-v4.1-flash": {"offpeak": (1.00, 0.02, 4.00),
                            "peak": (2.00, 0.04, 8.00)},
    "deepseek-v4-pro": {"offpeak": (3.00, 0.025, 6.00)},   # ⚠ 牌价与实测结构不符，见输出
    "glm-5.3": {"any": (8.00, 2.00, 28.00)},
    "glm-5.3-flash": {"any": (0.80, 0.23, 2.80)},
    "kimi-k2.6": {"any": (6.50, 1.10, 27.00)},
    "kimi-k3": {"any": (20.00, 2.00, 100.00)},
}

DIMS = ("in", "cr", "out")


def load_rates() -> dict[str, dict[str, tuple[float, float, float]]]:
    """返回 {model: {bucket: (in, cr, out) 积分/百万}}。"""
    out: dict[str, dict[str, tuple[float, float, float]]] = {}
    d = json.loads(RATES_FILE.read_text(encoding="utf-8"))
    for model, m in d["models"].items():
        if m.get("status") != "ok":
            continue
        buckets: dict[str, tuple[float, float, float]] = {}
        for bk, e in (m.get("by_bucket") or {}).items():
            if e.get("status") == "ok":
                r = e["rates_per_1k"]
                buckets[bk] = (r[0] * 1000, r[1] * 1000, r[2] * 1000)
        if not buckets:  # 本轮全为空闲，whole == offpeak
            w = m.get("whole_sample_fit") or {}
            r = w.get("rates_per_1k")
            if r:
                buckets["offpeak"] = (r[0] * 1000, r[1] * 1000, r[2] * 1000)
        if buckets:
            out[model] = buckets

    # deepseek 高峰档：本轮无样本，补 09-14 的测量值
    if PEAK_FILE.exists():
        p = json.loads(PEAK_FILE.read_text(encoding="utf-8"))
        for model, m in p["models"].items():
            e = (m.get("by_bucket") or {}).get("peak") or {}
            if e.get("status") == "ok":
                r = e["rates_per_1k"]
                out.setdefault(model, {})["peak"] = (r[0] * 1000, r[1] * 1000, r[2] * 1000)
    return out


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser()
    ap.add_argument("--yuan", type=float, default=3.3, help="购买价（元）")
    ap.add_argument("--credits", type=float, default=2000, help="购买积分")
    args = ap.parse_args()

    p = args.yuan / args.credits
    rates = load_rates()

    print(f"购买价 {args.yuan:g} 元 = {args.credits:g} 积分  →  实付 {p:.6f} 元/积分"
          f"（1 元 = {1 / p:.1f} 积分）")
    print(f"费率来源：{RATES_FILE.name}（09-18 权威）+ {PEAK_FILE.name}（deepseek 高峰档）")
    print()
    print(f"{'模型':<22} {'时段':<8} {'输入':>9} {'缓存':>9} {'输出':>9} "
          f"{'中位倍率':>9} {'离散度':>8} {'便宜':>8}")
    for model, listed in OFFICIAL.items():
        buckets = rates.get(model)
        if not buckets:
            print(f"{model:<22} {'—':<8} {'无实测费率':>9}")
            continue
        for bk, rate in sorted(buckets.items()):
            tab = listed.get(bk) or listed.get("any")
            if not tab:
                continue
            mults = []
            for i, dim in enumerate(DIMS):
                if rate[i] and tab[i]:
                    mults.append(rate[i] * p / tab[i])
            if not mults:
                continue
            mults.sort()
            mid = mults[len(mults) // 2]
            spread = (mults[-1] - mults[0]) / mid * 100 if mid else 0
            tag = " ⚠️" if spread > 10 else ""
            print(f"{model:<22} {bk:<8} "
                  + "".join(f"{(rate[i] * p / tab[i]) if tab[i] else float('nan'):>9.4f}" for i in range(3))
                  + f" {mid:>9.4f} {spread:>7.1f}% {1 / mid:>7.1f}x{tag}")
        print()

    print("说明：")
    print("  倍率 <1 表示比官方便宜；『便宜 42.4x』= 官方价的 1/42.4 ≈ 2.4%。")
    print("  DeepSeek 分时：高峰费率与官方牌价同为空闲 2 倍 → 倍率与空闲档相同。")
    print("  deepseek-v4-pro 三维离散度大：官方牌价与实测结构不符（实测 输出/输入=3.0×、")
    print("  缓存/输入=1/30×，牌价却是 2× 与 1/120×），该行仅作参考。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
