#!/usr/bin/env python3
"""压测期间循环采样 /status，观察账号池健康度与在途分布。"""
import json, sys, time, urllib.request

base = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:7863"
key = sys.argv[2] if len(sys.argv) > 2 else ""
interval = float(sys.argv[3]) if len(sys.argv) > 3 else 2.0
model = sys.argv[4] if len(sys.argv) > 4 else "kimi-k3"

while True:
    try:
        req = urllib.request.Request(f"{base}/status?model={model}",
                                      headers={"Authorization": f"Bearer {key}"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            d = json.load(resp)
        accts = d.get("accounts", [])
        busy = sum(1 for a in accts if a.get("in_flight", 0) > 0)
        top = sorted(((a.get("in_flight", 0), a.get("uid", "?")) for a in accts), reverse=True)[:6]
        t = time.strftime("%H:%M:%S")
        tops = ",".join(f"{u[-4:]}={n}" for n, u in top)
        print(f"[{t}] healthy={d.get('healthy')} cooling={d.get('cooling')} "
              f"disabled={d.get('disabled')} busy={busy} top[{tops}]", flush=True)
    except Exception as e:
        print(f"[{time.strftime('%H:%M:%S')}] error: {e}", flush=True)
    time.sleep(interval)
