#!/usr/bin/env python3
# krx-fep-reconcile.py — FEP(가상) 피드 라이브 대사.
#
# 시나리오 입력(JSON)에서 기대값을 "수식으로 독립 산출"하고, 같은 시나리오를 실 그룹
# (227.10.20.10)에 재생 → 실행 중인 mci-edge-krx 의 ws 출력(krx-tester --json)과 필드
# 단위로 PASS/FAIL 대사한다. 기대값이 런타임 코드가 아니라 입력+수식에서 나오므로
# enrichment/parse 회귀를 잡는다. (수식 근거: docs/krx-sise-design.md §11)
#
# 사용: krx-fep-reconcile.py <WTG_HOME> [group:port]
import json, os, subprocess, sys, time

WTG = sys.argv[1] if len(sys.argv) > 1 else "/home/winway/nh-fxallone-server/wtg"
DEST = sys.argv[2] if len(sys.argv) > 2 else "227.10.20.10:60642"
SCN = f"{WTG}/etc/krx-fep-scenario.json"
KV = f"{WTG}/bin/krx-verify"
KT = f"{WTG}/bin/krx-tester"
WS = "ws://127.0.0.1:8085/v1/subscribe"
TOL = 1e-6


def price_diff(cur, ref):
    """internal/krx PriceDiff 동형: cur<=0||ref<=0||보합 → (0,0,' ')."""
    if cur <= 0 or ref <= 0 or cur == ref:
        return 0.0, 0.0, " "
    d = cur - ref
    r = d / ref * 100.0
    return d, r, ("+" if r > 0 else "-" if r < 0 else " ")


def expect(sym):
    """시나리오 한 종목 → 기대 envelope 필드 (수식 독립 산출)."""
    m, t = sym.get("master", {}), sym.get("trade", {})
    code, mkt = sym["code"], sym.get("market", "fut")
    last = float(t.get("cprc", 0))
    e = {"code": code, "last": last}
    if mkt == "bond":
        # 채권 전일대비 = 기준가(bprc) 대비 → yDiff/yRate/ySign. 직전대비는 첫체결 0.
        yd, yr, ys = price_diff(last, float(m.get("bprc", 0)))
        e.update(yDiff=yd, yRate=yr, ySign=ys, cdiff=0.0)
    else:
        yprc = float(m.get("yprc", 0))
        yeff = yprc if yprc > 0 else float(m.get("bprc", 0))
        d, r, s = price_diff(last, yeff)
        e.update(diff=d, rate=r, sign=s, prevClose=yprc)
        cp = float(t.get("pprc", 0))
        cd, _, cs = price_diff(last, cp)
        e.update(cdiff=cd, csign=cs)
        st = sym.get("settle")
        if st:
            e["settle"] = float(st.get("sprc", 0))
    bk = sym.get("book", {})
    if bk.get("ask"):
        e["ask0_prc"], e["ask0_vol"] = float(bk["ask"][0][0]), int(bk["ask"][0][1])
        e["askTot"] = sum(int(x[1]) for x in bk["ask"])
    if bk.get("bid"):
        e["bid0_prc"], e["bid0_vol"] = float(bk["bid"][0][0]), int(bk["bid"][0][1])
        e["bidTot"] = sum(int(x[1]) for x in bk["bid"])
    return e


def collect(codes, secs=8):
    """krx-tester 로 ws 출력 수집 → (code,kind)별 최신 envelope."""
    p = subprocess.Popen([KT, "--url", WS, "--symbols", ",".join(codes),
                          "--count", "0", "--timeout", f"{secs}s", "--json"],
                         stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    time.sleep(0.6)
    subprocess.run([KV, "replay", "/tmp/fep_rc.dat", DEST, "2", "40ms"],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    out, _ = p.communicate(timeout=secs + 5)
    latest = {}
    for ln in out.splitlines():
        ln = ln.strip()
        if not ln:
            continue
        try:
            d = json.loads(ln)
        except Exception:
            continue
        latest[(d.get("code"), d.get("kind"))] = d
    return latest


def cmp_num(exp, act):
    return abs(float(exp) - float(act or 0)) <= TOL


def main():
    syms = json.load(open(SCN))
    subprocess.run([KV, "scenario", SCN, "/tmp/fep_rc.dat"],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    codes = [s["code"] for s in syms]
    latest = collect(codes)

    allpass = True
    print(f"{'code':14} {'field':10} {'expect':>12} {'actual':>12}  판정")
    print("-" * 60)
    for s in syms:
        e = expect(s)
        mkt = s.get("market", "fut")
        tr = latest.get((e["code"], "bond.trade" if mkt == "bond" else "fut.trade"))
        bk = latest.get((e["code"], "bond.book" if mkt == "bond" else "fut.book"))
        checks = []
        if tr is None:
            print(f"{e['code']:14} {'trade':10} {'-':>12} {'MISSING':>12}  ✗")
            allpass = False
        else:
            fields = (["last", "yDiff", "yRate", "cdiff"] if mkt == "bond"
                      else ["last", "diff", "rate", "cdiff", "prevClose"]
                      + (["settle"] if "settle" in e else []))
            for f in fields:
                checks.append((f, e[f], tr.get(f)))
            if mkt != "bond":
                checks.append(("sign", e["sign"], tr.get("sign")))
        if bk is not None and "ask0_prc" in e:
            checks.append(("ask0_prc", e["ask0_prc"], (bk.get("ask") or [{}])[0].get("prc")))
            checks.append(("ask0_vol", e["ask0_vol"], (bk.get("ask") or [{}])[0].get("vol")))
            checks.append(("bid0_prc", e["bid0_prc"], (bk.get("bid") or [{}])[0].get("prc")))
            checks.append(("askTot", e["askTot"], bk.get("askTot")))
            checks.append(("bidTot", e["bidTot"], bk.get("bidTot")))
        elif "ask0_prc" in e:
            print(f"{e['code']:14} {'book':10} {'-':>12} {'MISSING':>12}  ✗")
            allpass = False
        for f, ev, av in checks:
            ok = (ev == av) if isinstance(ev, str) else cmp_num(ev, av)
            if not ok:
                allpass = False
            evs = ev if isinstance(ev, str) else f"{ev:.4f}"
            avs = av if isinstance(av, str) else (f"{float(av):.4f}" if av is not None else "None")
            print(f"{e['code']:14} {f:10} {evs:>12} {avs:>12}  {'✓' if ok else '✗'}")
    print("-" * 60)
    print("[PASS] 전 종목·필드 기대값 일치" if allpass else "[FAIL] 불일치 존재 (위 ✗)")
    sys.exit(0 if allpass else 1)


if __name__ == "__main__":
    main()
