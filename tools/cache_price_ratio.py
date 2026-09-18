#!/usr/bin/env python3
"""计算「未命中缓存输入价 / 缓存命中价」的倍率。

用户问的是 p_in / p_cr（积分口径）。同时给出官方价目表的同一比值作为交叉校验：
若积分/元的换算对每个模型是单一标量，则两边的比值应当完全相等。

数据来源（按可信度排序）：
- billing-export-0914/rates_from_local_credit.json  本地上游真值，零配对误差（权威）
- billing-export-0914/rates_peak_offpeak.json       账单配对路径（对照）
- docs/probes/rates_from_credit.json                探针（样本 5~6，仅方向性）
"""
import json
from pathlib import Path

# 官方价目表（元/百万 token）：input / cache_hit / output
OFFICIAL = {
    "glm-5.3": (8.0, 2.0, 28.0),
    "deepseek-v4.1-flash": (1.0, 0.02, 4.0),        # 空闲档
}


def load(p):
    path = Path(p)
    return json.loads(path.read_text(encoding="utf-8")) if path.exists() else None


def show(title, rows):
    print(f"\n{title}")
    print(f"  {'模型':<34} {'输入':>12} {'缓存命中':>12} {'输出':>12} {'输入/命中':>10}")
    for name, r in rows:
        pin, pcr, pout = r
        ratio = (pin / pcr) if pcr else None
        rs = f"{ratio:10.2f}" if ratio is not None else "   不可识别"
        print(f"  {name:<34} {pin:12.5f} {pcr:12.5f} {pout:12.5f} {rs}")


local = load("billing-export-0914/rates_from_local_credit.json")
main_rows = []
for model, m in (local or {}).get("models", {}).items():
    if m.get("status") != "ok":
        continue
    whole = m.get("whole_sample_fit")
    for bucket in ("peak", "offpeak"):
        e = (m.get("by_bucket") or {}).get(bucket) or {}
        if e.get("status") != "ok":
            continue
        r = e["rates_per_1k"]
        main_rows.append((f"{model} [{bucket}]", (r[0] * 1000, r[1] * 1000, r[2] * 1000)))
    if whole and m.get("by_bucket", {}).get("peak", {}).get("status") != "ok":
        r = whole["rates_per_1k"]
        main_rows.append((f"{model} [不分时段]", (r[0] * 1000, r[1] * 1000, r[2] * 1000)))

show("① 实测倍率（本地上游真值，积分/百万 token）—— 权威", main_rows)

# 交叉校验：官方价目表的同一比值
print("\n② 官方价目表交叉校验（元/百万 token）")
print(f"  {'模型':<34} {'输入':>12} {'缓存命中':>12} {'输出':>12} {'输入/命中':>10}")
for name, (pin, pcr, pout) in OFFICIAL.items():
    print(f"  {name:<34} {pin:12.5f} {pcr:12.5f} {pout:12.5f} {pin / pcr:10.2f}")

# 探针数据（覆盖更多模型，但样本小）
probes = load("docs/probes/rates_from_credit.json")
probe_rows = []
for model, m in (probes or {}).get("models", {}).items():
    r = m.get("rates") or {}
    iv = m.get("intervals") or {}
    ok = (iv.get("cache_read_per_1k") or {}).get("identifiable")
    pin = (r.get("input_per_1k") or 0) * 1000
    pcr = (r.get("cache_read_per_1k") or 0) * 1000
    pout = (r.get("output_per_1k") or 0) * 1000
    tag = "" if ok else "  ←p_cr不可识别"
    probe_rows.append((f"{model}{tag}", (pin, pcr, pout)))
probe_rows.sort(key=lambda kv: -(kv[1][1] or 0))
show("③ 探针费率（每模型仅 5~6 条，仅方向性参考）", probe_rows)
