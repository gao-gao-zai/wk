#!/usr/bin/env python3
"""归因：分时费率下的超包络行，究竟是"配对错位"还是"上游真的这么收费"？

判别方法（决定性）：超包络行的本地日志带**上游真值** `credits_consumed`。
- 若 `本地真值 == 配对账单 credit`  → 配对是对的，**上游确实按这个价收了**
  （即上游存在重试/合并/额外计费）→ 不是配对问题。
- 若 `本地真值 != 配对账单 credit`  → 配对拿错了账单行。
  进一步检查：同「账号+分钟+模型」下是否存在另一条账单行其 credit 恰等于本地真值
  → 是"换位"（swap）。

用法：
    python tools/attribute_outliers.py --billing <csv> --db <db> --model deepseek-v4.1-flash
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

from diagnose_residuals import load_local_with_account  # noqa: E402
from fit_peak_offpeak import bucket_of  # noqa: E402
from refit_rates_from_billing import CAP, TZ, build_pairs, features  # noqa: E402

# 分时费率（积分/1K），实测值（tools/fit_from_local_credit.py）。
RATES = {
    "deepseek-v4.1-flash": {
        "offpeak": np.array([0.0142853, 0.00028579, 0.0571377]),
        "peak": np.array([0.0285724, 0.00057132, 0.1142465]),
    },
}


def suffix(rid: str) -> str:
    return rid.split("-", 1)[1][8:] if rid and "-" in rid else ""


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

    # 账单行按 (账号, 分钟, 模型) 索引，用于找"真正的伙伴"
    by_key: dict[tuple, list] = defaultdict(list)
    for b in bill:
        by_key[(b["account_uid"], (b["request_time"] or "")[:16], b["model"])].append(b)

    rows = [(l, b) for l, b in pairs if l["model"] == args.model]
    rate = RATES[args.model]

    out = []
    for l, b in rows:
        x = np.array(features(l), float)
        if x[0] == 0 and x[2] == 0:
            continue
        ts = dt.datetime.fromtimestamp(l["created_at"], TZ)
        pred = float(x @ rate[bucket_of(ts)] / 1000.0)
        obs = float(b["credit"] or 0)
        if abs(obs - pred) <= CAP:
            continue
        truth = float(l["credits_consumed"] or 0)
        local_is_upstream = l["credit_source"] == "upstream"
        agrees = local_is_upstream and abs(truth - obs) < 1e-9
        rec = {
            "resid": obs - pred, "pred": pred, "obs": obs, "truth": truth,
            "agrees": agrees, "source": l["credit_source"], "ts": ts,
            "key": (l["account_uid"], ts.strftime("%Y-%m-%d %H:%M"), l["model"]),
            "miss": int(x[0]), "hit": int(x[1]), "out_tok": int(x[2]),
        }
        # 同 (账号,分钟,模型) 下是否另有账单行等于本地真值 → 换位
        rec["swap_candidate"] = (
            local_is_upstream and not agrees
            and any(abs(float(o["credit"] or 0) - truth) < 1e-9
                    for o in by_key.get(rec["key"], []) if o is not b)
        )
        # 本地真值自己是否也对不上"任何"同分钟账单行
        out.append(rec)

    st = Counter()
    for r in out:
        if r["source"] != "upstream":
            st["本地无真值（无法判别）"] += 1
        elif r["agrees"]:
            st["配对正确 → 上游确实如此计费"] += 1
        elif r["swap_candidate"]:
            st["配对错位（同分钟找到真伙伴）"] += 1
        else:
            st["本地真值 ≠ 配对账单，且找不到真伙伴"] += 1

    print(f"{args.model}: 配对 {len(rows)}，超包络 {len(out)} 行"
          f"（{100 * len(out) / max(len(rows), 1):.2f}%）\n")
    print("归因：")
    for k, v in st.most_common():
        print(f"  {k}: {v}")

    print("\n明细（|resid| 降序前 20）：")
    print(f"  {'resid':>9} {'pred':>8} {'账单':>7} {'本地真值':>9} {'一致':>5} "
          f"{'换位':>5}  {'时刻':<12} miss/hit/out")
    for r in sorted(out, key=lambda v: -abs(v["resid"]))[:20]:
        print(f"  {r['resid']:+9.4f} {r['pred']:8.4f} {r['obs']:7.2f} {r['truth']:9.2f} "
              f"{'是' if r['agrees'] else '否':>5} {'是' if r['swap_candidate'] else '否':>5}  "
              f"{r['ts']:%m-%d %H:%M}  {r['miss']}/{r['hit']}/{r['out_tok']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
