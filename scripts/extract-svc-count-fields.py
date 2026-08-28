#!/usr/bin/env python3
# extract-svc-count-fields.py — 원본 클라 Request XML 의 반복부 건수 선언(sizefield)을
# 추출해 etc/svc-count-fields.json 을 생성한다.
#
# 배경: mci-api(pkg/svcio)는 .h 만 파싱하다 보니 반복부(grid) 건수 필드를 "orec 직전
# + 이름관례(rcnt/*_cnt)"로 *추측*했다. 원본 클라는 Request XML 에서
#   <record name="Occurs" sizetype="1" sizefield="Outbound.grid01_cnt">
# 처럼 건수 필드를 *이름으로 선언*한다. 이 선언을 추출해 svcio 가 추측 대신 선언을
# 쓰게 한다 (rec/rec1/nrec1 등 관례 밖 이름까지 정확). docs/svc-count-fields.md.
#
# 사용:
#   python3 scripts/extract-svc-count-fields.py <Request_dir> > etc/svc-count-fields.json
#   # 예 (EC2): sudo python3 .../extract-svc-count-fields.py \
#   #             /home/winway/projects/yuanta/FxAuto_client/Request > etc/svc-count-fields.json
#
# 파일명(확장자 제거) = service code. XML 은 UTF-16.
import os, sys, re, json

def main():
    if len(sys.argv) < 2:
        sys.stderr.write("usage: extract-svc-count-fields.py <Request_dir>\n")
        sys.exit(2)
    root = sys.argv[1]
    # 따옴표는 " 또는 ' 둘 다 (원본 XML 이 파일마다 다름 — 작은따옴표 파일을 놓치면
    # T7101S02/T4102A02 등이 통째로 빠진다). 속성 순서도 파일마다 달라 [^>]* 로 흡수.
    occ = re.compile(
        r"""<record[^>]*name=["']Occurs["'][^>]*sizetype=["']1["'][^>]*sizefield=["']([^"']+)["']""",
        re.I,
    )
    out = {}
    for dp, _, fs in os.walk(root):
        for fn in fs:
            if not fn.lower().endswith(".xml"):
                continue
            svc = os.path.splitext(fn)[0]
            try:
                raw = open(os.path.join(dp, fn), "rb").read().decode("utf-16", "ignore")
            except Exception:
                continue
            flds = [m.split(".")[-1] for m in occ.findall(raw)]  # "Outbound.grid01_cnt" → "grid01_cnt"
            if flds:
                out[svc] = flds if len(flds) > 1 else flds[0]
    print(json.dumps(out, ensure_ascii=False, sort_keys=True, indent=2))
    sys.stderr.write("extracted %d services with sizefield declarations\n" % len(out))

if __name__ == "__main__":
    main()
