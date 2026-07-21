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

주문 payload: `deploy/demo-order-spot.json` (SPOT USD/KRW 매수 10만달러 지정가).

```bash
JWT=$(curl -s -XPOST http://127.0.0.1:8080/v1/login \
  -H 'Content-Type: application/json' -d '{"data":{"lgnId":"demo01"}}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

curl -s -XPOST http://127.0.0.1:8080/v1/tx \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  --data @deploy/demo-order-spot.json
```

성공 응답: `rcod=00003 주문이 정상처리 되었습니다`, `ordnNo=Dxxxxxxxxxx` 채번.
DB 확인: `SELECT ORDN_NO, ORDN_STTS_DCD FROM FXPL.TB_FXB_TRG001L;` → 상태 `0`(접수대기).

## 주문 입력에서 주의할 점 (검증 관문에서 배운 것)

| 필드 | 값/규칙 | 안 지키면 |
|-----|---------|----------|
| `ordnYmd`/`ordnValdYmd` | **엔진 기준영업일과 정확히 일치** (현재 `20240708` 로 고정/stale) | `10119` 영업일 오류 |
| `expiSttgYmd`/`expiFnshYmd` | 결제일 (SPOT T+2) | `32507` 결제일 오류 |
| `cvrSpr`+`slsSpr`+`cusSpr` | 합 > 0 (고객직거래) | `32580` 스프레드 오류 |
| `imdrYn` | `N` (TRG001L NOT NULL) | `32101` 주문내역 생성 오류(ORA-01400) |
| 거래한도(TRC001M) | 주문금액 ≤ 1일/1회 한도 | `32578` 한도초과 |

> 영업일이 2024-07-08 로 멈춰 있어 데모 주문일자를 거기 맞춰야 한다. 실제 시연이면
> 영업일 캘린더(CMG012M)를 실날짜로 전진시키는 별도 작업이 필요하다.

## 체결(체결/TRG003L) — 매칭 엔진 필요

주문은 상태 `0`(접수대기)로 등록되고, W3200A01 이 이를 **체결엔진(mat, 내부 매칭)**
으로 전송한다(`trdTypeDcd=1`). 체결이 일어나면 상태가 `2`(체결)로 바뀌고 TRG003L 에
체결내역이 쌓인다.

**현재 EC2 에는 매칭 엔진(mat)이 빌드/기동돼 있지 않다** (`/home/winway/nh-fxallone-server/mat`
디렉토리만 존재, bin 에 `mon` 뿐, systemd unit 없음). 따라서 주문은 접수대기까지만
간다. 체결까지 태우려면 mat 빌드·기동 + MES-broker 어댑터(`docs/mes-broker-adapter.md`)
연결이 별도로 필요하다.
