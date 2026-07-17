# MES broker 어댑터 (trn ↔ mat, wfa TCP 대체)

> **상태**: 구현 완료 (2026-07-17). EC2 빌드·배포 + 검증 게이트 실행은 대기.
> 레거시 wfa (SHM 큐 ↔ TCP 브리지) 를 MyMQ broker 큐로 치환한 마지막 조각.
> 배경/의사결정은 `docs/mds-replacement-plan.md` §4 (wfa 제거) 참조.

## 1. 왜 (큐는 큐로)

trn ↔ mat(매칭엔진) 주문·체결은 원래 wfa 를 거쳤다: trn 이 SHM 큐
(`IBA_MES_ORD_QUE_PUSH`) 에 넣으면 wfa 데몬이 TCP(:26003/:26004) 로 중계 →
반대편 wfa 가 다시 SHM 큐로. wfa 는 mds 와 같은 방향(레거시 브리지)으로 제거 대상.

TCP 직결안은 **기각** — 큐의 존재 이유가 안정성(mat 순단 시 버퍼링, 발신 논블로킹)
이므로 큐는 큐로 대체한다. 주문/체결은 저빈도라 broker 부하(시세 폭주) 와 무관.

## 2. wire 계약 (단일 출처)

| 방향 | exchange | rkey | queue | FC | 전문 |
|---|---|---|---|---|---|
| 주문 trn → mat | `MESORD` | `ORD` | `mat_ord` | `FC_NOTIFY` | `ORDER_RECV` (원본 바이트 무변형) |
| 체결 mat → trn측 | `MESEXE` | `EXE` | `mes_exe` | `FC_NOTIFY` | `ORDER_SEND` (원본 바이트 무변형) |

- **publisher 는 xchg/rkey 만 지정**, **subscriber 가 exchange·queue·bind 선언** (mymq 컨벤션).
  - 주문: trn publish(선언 X) / mat 이 `MESORD`·`mat_ord` 선언·bind.
  - 체결: mat publish(선언 X) / WD300002 가 `MESEXE`·`mes_exe` 선언·bind.
- 전문은 broker 페이로드로 **raw struct 바이트 그대로** — wfa 의 MAT_PACKET/COMM_HEAD
  (LINK/HTBT/DATA·ack) 프레이밍은 TCP 전용 산물로 broker 가 전부 대체.

## 3. 코드 (양측 대칭 어댑터)

| 측 | 파일 | 함수 | 역할 |
|---|---|---|---|
| trn (win/src) | `lib/wws/wwstr/wtgmes_wtg.c` | `fnWtgMesOrdSend(mymq,buf,len)` | 주문 publish MESORD/ORD |
| trn (win/src) | 〃 | `fnWtgMesExeRecv(buf,size,timeout)` | WD300002 체결 수신 (MESEXE/EXE 구독) |
| mat | `match/wtgmes_mes.c` | `fnWtgMesOrdRecv(buf,size,timeout)` | mat 주문 수신 (MESORD/ORD 구독) |
| mat | 〃 | `fnWtgMesExeSend(buf,len)` | mat 체결 publish MESEXE/EXE |

호출부:
- 주문 push: `win/src/trn/W3200/W3200A0{1,2,3}.pc` (기존 `IBA_MES_ORD_QUE_PUSH` 치환).
- 체결 수신: `win/src/mon/wwstr/WD300002.pc` (기존 `IBA_MES_EXE_QUE_POP` 치환).
- mat 수신: `mat/match/mat_rcv/main.c` — broker 모드에서 `fnWtgMesOrdRecv` → `Smq_Send`
  (기존 TCP accept/DATA→Smq 경로 대체, 매칭엔진 SHM 큐 진입은 동일).
- mat 송신: `mat/match/mat_snd/main.c` — broker 모드에서 `Smq_GetRecord` → `fnWtgMesExeSend`
  (기존 SmqProcess→SendPacket(TCP) 대체).

## 4. 전환 토글 (롤백 레버)

mat_rcv/mat_snd 는 환경변수 `MAT_MES_TRANSPORT` 로 전송 방식 선택:
- **미설정 / 그 외** → 레거시 TCP select 루프 (기존 동작 무손상, 롤백).
- `broker` → broker 큐 루프 (`MES broker mode ...` 로그 후 진입, TCP listen skip).

big-bang 전환 시 trn(W3200/WD300002)·mat 동시 배포 + mat 서비스에 `MAT_MES_TRANSPORT=broker`
주입. 문제 시 env 제거 + 재기동으로 즉시 TCP 롤백.

## 5. 빌드 (EC2)

mat 은 기존에 mymq 를 링크하지 않았으므로 makefile 에 추가함:
- `mat/match/mat_rcv/makefile`, `mat/match/mat_snd/makefile`
  - `SOURCE += ../wtgmes_mes.c` (공용 어댑터, `-I..` 로 헤더 참조)
  - `INC += -I$(INC_DIR)` (mymq.h — `$(PREFIX)/include`)
  - link `+= -L$(LIB_DIR) -lmymq` (`$(PREFIX)/lib/libmymq.so`)
- `INC_DIR`/`LIB_DIR` 는 `mat/lib/mymq.env → mymq/src/environments` 에서 제공.
- trn 측은 기존 wtgrta(mci-push) 배선과 동일 빌드 경로 — 신규 링크 변경 없음.

## 6. 검증 게이트 (배포 후 실행)

`docs/mds-replacement-plan.md` §4 게이트:

```
주문 1건 → mat 체결 → WD300002 수신 → DB 반영 + RTA push (mci-push) 까지 end-to-end
```

절차:
1. broker(mymqd) 기동, mat_rcv/mat_snd 를 `MAT_MES_TRANSPORT=broker` 로 기동.
2. W3200 주문 서비스로 주문 1건 → broker `MESORD` 큐 → mat_rcv `Smq_Send` 로그 확인.
3. 매칭 후 mat_snd `Smq_GetRecord` → `MESEXE` publish 로그.
4. WD300002 `fnWtgMesExeRecv` 수신 → 체결 DB 반영 + `fnWtgRtaPush` 로 client push.
5. broker 큐 카운터(`mes_exe`/`mat_ord`) 로 유실 0 확인.

## 관련
- `docs/mds-replacement-plan.md` §4 — wfa 제거 표 + 검증 게이트
- `win/src/lib/wws/wwstr/wtgmes_wtg.c` / `mat/match/wtgmes_mes.c` — 양측 어댑터
- `docs/conventions.md` — exchange/rkey/queue 카탈로그
