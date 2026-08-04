# 데모 모의주문/체결 런북

EC2 엔진 연동 환경에서 고객 SPOT 주문을 `/v1/tx` 로 태우는 절차와 검증. 선행:
`deploy/demo-seed-fx-agreements.sql`(고객/약정/한도) + `deploy/schema-align-fxpl.sql`
(스키마 정렬)이 적용돼 있어야 한다.

## 선행 조건 (이 세션에서 만든 것)

1. **스키마 정렬** — `deploy/schema-align-fxpl.sql`. 배포 주문 코드가 신 스키마를
   쓰는데 RDS 의 4개 테이블(CMG039M/CMC004M/CMG016M/CMG020R)이 구 스키마라
   `ORA-00904` 로 막혔던 것을 정본 `table.sql` 기준으로 정렬.
2. **데모 시드** — `deploy/demo-seed-fx-agreements.sql`. 고객 10 + 약정 + 계좌 +
   **거래한도(TRC001M)** 까지. 한도가 없으면 주문이 `32578`(1일한도 초과)로 막힘.

## 주문 실행

주문 payload: `deploy/demo-order-spot.json` (SPOT USD/KRW 매수 10만달러). 발사 전
`ordnYmd`/`ordnValdYmd`(오늘)와 가격 필드(현재 시세 근처)를 갱신할 것 — 아래 주의점 표 참조.

```bash
JWT=$(curl -s -XPOST http://127.0.0.1:8080/v1/login \
  -H 'Content-Type: application/json' -d '{"data":{"lgnId":"demo01"}}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

curl -s -XPOST http://127.0.0.1:8080/v1/tx \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  --data @deploy/demo-order-spot.json
```

성공 응답: `rcod=00003 주문이 정상처리 되었습니다`, `ordnNo` 채번 (접두 = 날짜 인코딩).
DB 확인: `SELECT ORDN_NO, ORDN_STTS_DCD FROM FXPL.TB_FXB_TRG001L;` — 접수 직후 `0`(접수대기),
mat 체결 후 `3`(체결). 체결내역은 TRG003L, 가격 leg(시장가/본점가/고객가)는 TRG005L.

## 주문 입력에서 주의할 점 (검증 관문에서 배운 것)

| 필드 | 값/규칙 | 안 지키면 |
|-----|---------|----------|
| `ordnYmd`/`ordnValdYmd` | **엔진 기준영업일과 정확히 일치** — today 모드에선 오늘 날짜 | `10119` 영업일 오류 |
| `expiSttgYmd`/`expiFnshYmd` | 결제일 (SPOT T+2) | `32507` 결제일 오류 |
| `cvrSpr`+`slsSpr`+`cusSpr` | 합 > 0 (고객직거래) | `32580` 스프레드 오류 |
| `imdrYn` | `N` (TRG001L NOT NULL) | `32101` 주문내역 생성 오류(ORA-01400) |
| 거래한도(TRC001M) | 주문금액 ≤ 1일/1회 한도 | `32578` 한도초과 |
| `ordnPrcCncd`+`fxOrdnPntmPrc` | 시장가는 `"M"` + 주문시점가격을 현재 시세(best-stats mid) 근처로 | `-82012` 슬리피지 (SlipPip 3원) |

> **영업일 = today 모드**: 재기동 시 `WB500101 <today>` 배치가 CMG012M/CMG006M 을
> 오늘 영업일로 롤한다 (매 영업일 0600 자동 실행 — mymqappd 스케줄러). 주문일자는
> 오늘 날짜로 맞추면 된다. `demo-order-spot.json` 의 `ordnYmd`/`ordnValdYmd`/결제일은
> 발사 전 오늘 기준으로 갱신할 것. 주문번호 접두는 날짜 인코딩 (20260729 → `F729...`).

## 체결(TRG003L) — 매칭 엔진 mat 연동 (완성)

주문은 상태 `0`(접수대기)로 등록되고, W3200A01 이 broker `MESORD` 로 **체결엔진(mat)**
에 전송한다. mat 이 매칭하면 `MESEXE` 를 publish, trn `WD300002`(체결수신, mymqappd
관리)가 소비해 TRG001L 상태 `3`(체결) + TRG003L(체결내역) + TRG005L(가격 leg)을 기록한다.

**mat 은 EC2 에 빌드·기동돼 있다** — proc_d(process.cfg)가 mat 7-proc
(mat_rcv/ord/mat/exe/snd/sis + 시세 브리지)을 spawn·감시·respawn 한다.
기동은 `mat/bin/mat-start.sh` 1회 (clean-slate → create_smq/create_mat → mat_bat →
proc_d 데몬).

- **시세**: mat-sise-bridge 가 mci-price `SubscribeAlgo`(raw BEST) → APSISE 128B →
  UDP 127.0.0.1:30022 → mat_sis. 다중 pair (USD/KRW, EUR/KRW 등) 공급.
- **고객마진**: mat(WLM003)이 mci-price `GET /v1/customer-margin` 을 직접 call —
  표시가(quote)와 체결가 마진 정합 (시세 + 본점 SHM + 영업점 = quote 총마진).
- **체결 push**: WD300002 → mci-push HTTP push → ws `/v1/subscribe` 로 체결 RTS 실시간 전달.

검증된 커버리지: **SPT/BAR/FWD/MAR × BUY/SELL × 시장가/지정가**, USD/KRW(직접) +
EUR/KRW(재정) 반복 체결. SWP(2-leg)만 잔여. 리테일 지정가는 설계상 즉시환전(BAR)이라
지정가 한도 resting 은 인터뱅크(시장거래) 도메인.

> **재기동 순서 주의**: broker(mymqd) 재시작 시 mat 의 MES 라우팅이 desync 되어 주문이
> hang(/v1/tx 500) 된다. 반드시 **mymqreboot(broker+trn) → `mat-start.sh`** 순서로 동반
> 재기동할 것. 재부팅 직후 1~2분은 settle 대기 후 검증.
