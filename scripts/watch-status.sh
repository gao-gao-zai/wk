#!/usr/bin/env bash
# watch-status.sh 压测期间循环采样 /status，观察账号池健康度与在途分布。
# 用法: ./watch-status.sh [base] [key] [interval_sec] [model]
set -euo pipefail
BASE="${1:-http://127.0.0.1:7863}"
KEY="${2:-${WB2A_API_KEY:?需要 API key（参数 2 或 WB2A_API_KEY）}}"
INTERVAL="${3:-2}"
MODEL="${4:-kimi-k3}"

while true; do
  ts=$(date +%H:%M:%S)
  # 总览 + 每账号 in_flight（按在途降序取前 8）
  curl -sS -H "Authorization: Bearer $KEY" \
    "$BASE/status?model=$MODEL" | python3 -c '
import json, sys
d = json.load(sys.stdin)
accts = d.get("accounts", [])
inflight = sorted(((a.get("in_flight", 0), a.get("uid", "?")) for a in accts), reverse=True)[:8]
busy = sum(1 for a in accts if a.get("in_flight", 0) > 0)
m = d.get("metrics", {})
print(f"[{sys.argv[1]}] total={d.get(\"total\")} healthy={d.get(\"healthy\")} cooling={d.get(\"cooling\")} disabled={d.get(\"disabled\")} "
      f"sticky={d.get(\"sticky_sessions\")} 账号占用中={busy}")
cap = d.get("concurrency") or {}
print(f"  池容量: slots={cap.get(\"ConfiguredSlots\")} avail={cap.get(\"AvailableSlots\")} inflight={cap.get(\"in_flight\")}")
print("  Top在途:", ", ".join(f"{u[-6:]}={n}" for n, u in inflight))
' "$ts" || echo "[$ts] status 拉取失败"
  sleep "$INTERVAL"
done
