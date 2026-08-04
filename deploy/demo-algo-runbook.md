# 데모 algo(조건부주문) 런북 — algotrd 브링업/재기동/검증

EC2 엔진 연동 환경에서 **algotrd**(STOP/OCO/트레일링 조건부주문 봇)를 기동하고
트리거 → 주문 → 매칭 → 체결통보까지 검증하는 절차. 선행: `demo-order-runbook.md`
의 리테일 주문/체결 경로(mat 기동 포함)가 살아 있어야 한다.

> 소스/바이너리는 WTG repo 가 아니라 엔진 미러(`mywork/nh`, EC2
> `/home/winway/nh-fxallone-server/autotrd`) 에 있다. 이 문서는 운영 절차만 담는다.
> 상세 이력은 세션 메모리 `project_algotrd_bringup` / `project_algo_autotrd` 참조.

## 구성 요소

| 구성 | 위치 | 역할 |
|-----|------|-----|
| `algotrd` / `algotrdcli` | EC2 `/fxwin/app/autotrd/` | 조건부주문 봇 본체 / 주문 주입 CLI. **mat-start 후크 + 영업일 배치(0600)가 `algotrd-restart.sh` 로 자동 재기동** (ALGO_TYMD=오늘) |
| algo 시세 브리지 | `mat/bin/mat-sise-bridge` 2번째 인스턴스 (`algo-bridge-run.sh`) | mci-price gRPC `SubscribeAlgo` → APSISE 128B → UDP `127.0.0.1:30122` (mat_sis 용 `:30022` 와 별개). **proc_d(process.cfg `abridge`) 감시 — mci-price 재시작 시 자동 respawn** |
| WTG 브링업 stub | `autotrd/wtg-build/{market_stub,rta_stub}.c` | mds SHM 대체(excode 'B'→"SMB") / 체결통보 `ATOEXE` 구독 |
| 주문 trn | `W3100A01` (시장거래 `ordnOrgnDcd=5`) | 검증 7게이트 → broker `MESORD` → mat (FEP 대신 mat 재배선됨) |
| 체결통보 | `WD300002` → exchange `ATOEXE`/rkey `AEX` | 체결 시 `fnWtgAlgoExeSend` publish → algotrd `proc_fill` (OCO 자동취소/트레일링 재무장의 전제) |

전체 경로:

```
mci-price (BEST) ─gRPC SubscribeAlgo→ 브리지 ─UDP:30122→ algotrd 트리거(prcwatch)
  → W3100A01 (7게이트) → broker MESORD → mat 매칭 → MESEXE → WD300002
  → TRG001L/003L/005L 기록 + ATOEXE publish → algotrd proc_fill → OCO 취소/트레일링
```

## 기동 절차 (재기동 포함)

algo 스택은 **전부 자동 관리**된다 — 수동 단계는 mymqreboot → mat-start.sh 뿐.

```bash
# 반드시 winway 유저로. root 실행 금지, & 백그라운드 금지
sudo -u winway bash -lc "cd mymq/bin && yes y | ./mymqreboot"
sudo -u winway bash /home/winway/nh-fxallone-server/mat/bin/mat-start.sh
```

mat-start.sh 하나로: mat 스택(proc_d) + **algo 브리지(:30122, proc_d `abridge`)** +
**algotrd(`algotrd-restart.sh`, ALGO_TYMD=오늘 자동)** 가 함께 올라온다.

자동 복구 매트릭스:

| 이벤트 | algo 복구 주체 |
|-----|------|
| mci-price 재시작 (WTG deploy) | proc_d 가 브리지 respawn (수 초). algotrd 는 영향 없음 |
| algotrd/브리지 개별 사망 | 브리지=proc_d respawn / algotrd=다음 mat-start 또는 영업일 배치 (급하면 `algotrd-restart.sh` 수동 1회) |
| mymqreboot (broker 재시작) | mat-start.sh 재실행 → 전부 복원 |
| 영업일 전환 | 영업일 배치(0600, mymqappd)가 `algotrd-restart.sh` 로 ALGO_TYMD 갱신 재기동 |

- `ALGO_TYMD` 는 **엔진 기준영업일과 일치**해야 한다 — 위 자동 경로는 모두 오늘 날짜로
  계산한다. 수동 기동이 필요하면 `algotrd-restart.sh [yyyymmdd]` 를 쓸 것 (pidfile
  정리 + pkill -9 + env 세팅 포함 — 직접 setsid 기동 금지).
- 재기동 직후 1~5분은 broker 응답 라우팅이 flaky (`call_W3100A01 failed` 간헐) —
  settle 후 검증할 것. 판정 기준은 mymq_call 성공 여부가 아니라 **mat 로그 도달 여부**.

## 주문 주입 / 검증

```bash
cd /fxwin/app/autotrd
LD_LIBRARY_PATH=<mymq/lib:win/lib:instantclient:dbhome/lib> \
  ./algotrdcli stoporder <side B/S> <limit가격> <trigger가격>   # STOP
  ./algotrdcli ocoorder                                          # OCO (BUY+SELL 2-leg)
```

- **즉발 트리거**: BUY 는 trigger 를 시세보다 낮게(bid≥trig 발화), SELL 은 높게(ask≤trig).
- **체결 (크로싱)**: algo 는 시장거래(OrgnGb=5)라 mat 이 자가체결하지 않는다 —
  같은 지정가의 반대 방향 주문을 counterparty 로 발사해 book 크로싱시킨다.
  예) OCO SELL@1327.4 체결 = `stoporder B 1328 1300` (BUY limit > SELL limit).
- **OCO 자동취소 확인**: 한 leg 체결 → WD300002 로그 `fnWtgAlgoExeSend(ATOEXE) rc=207`
  → algotrd 로그 `proc_fill` → `ocoorder_cancel done!!` (반대 leg 취소).
- book pollution 주의 — 이전 테스트의 resting 주문이 남아 있으면 counterparty 가
  엉뚱한 가격에 크로싱된다. 깨끗한 검증은 mat 재기동(book clear) 후.
- DB 확인은 리테일과 동일: TRG001L 상태 `3` + TRG003L/TRG005L.

## DB 시드 의존 (한 번만, 재적용 무해)

| 시드 | 없으면 |
|-----|--------|
| `CSC004M` 사용자(S15556) `FX_USER_DCD='E3'` (딜러) | `32673` 사용자권한 / `32669` 딜러PU |
| `CMC003D` 에 `FX_USER_DCD` 코드 `E3`/`XX` 행 | `10585` 부점등록 |
| `CMC003D` `AUTO_TRADE_INFO`/`A` (`CMCD_VL_REF_CON1='PU105'`) | WD300002 fnMakeAlg ORA-01403 → **ATOEXE 미발행** (체결은 되나 OCO 취소 불발) |

## 재빌드 레시피 (algotrd 수정 시)

```bash
bash autotrd/wtg-build/build_algotrd.sh          # 7개 .c 컴파일 (링크는 stub 누락으로 실패)
# stub 수동 컴파일 + 재링크 (build script 가 market_stub/rta_stub 누락)
gcc $CFLAGS $INC -c autotrd/wtg-build/market_stub.c -o /tmp/agobj/market_stub.o
gcc $CFLAGS $INC -c autotrd/wtg-build/rta_stub.c   -o /tmp/agobj/rta_stub.o
gcc /tmp/agobj/*.o -o /tmp/algostage/algotrd $LIBS   # 변수는 build_algotrd.sh 참조
cp /tmp/algostage/algotrd /fxwin/app/autotrd/algotrd && algotrd 재기동
```

trn 측(WD300002/libwwstr) 수정 시: instantclient proc 로 빌드(winway 로그인셸),
WD300002 링크에 `wtgprice/libwtgprice.a` + `wtgpush/libwtgpush.a` 수동 추가 필요.
**Pro*C char 호스트변수는 NUL 미종료(blank-pad) — strcpy 금지, memcpy(8)+명시 NUL.**

## 검증된 커버리지 / 잔여

- ✅ STOP 실체결 e2e (트리거→7게이트→MESORD→mat 매칭→체결 기록)
- ✅ OCO 자동취소 e2e (ATOEXE 체결통보 → proc_fill → 반대 leg 취소)
- ✅ 트리거 로직 코드 검증 (STOP 방향별 / OCO 2-leg / 트레일링 cancel-replace)
- ⬜ 트레일링 스탑 실전 시연 (배선은 열려 있음 — proc_fill 활성화로 재무장 가능)
- ⬜ 실 인터뱅크 counterparty (딜링데스크 도메인 — 현재는 크로싱 쌍 시뮬레이션)

## 트러블슈팅

| 증상 | 원인/조치 |
|-----|----------|
| `82052` 테너 마진정보 검색 오류 | 결제일-테너 부정합. 영업일 롤 후 `WB200002 <today>` 재실행 (today 모드 스케줄러가 자동화). 과거 원인이었던 W3100 결제일 공백은 WTR005 fnBaseInfoQry memcpy 패치로 해결됨 |
| `call_W3100A01 failed` 간헐 | 재기동 직후 broker 라우팅 settle 대기 (수분). 지속되면 mymqreboot→mat-start 전체 재기동 |
| 주문이 mat 미도달 + W3200/W3100 워커 churn | stale `libwwstr.so` — 재빌드 후 W3200·W3100 **둘 다** pkill (런처 respawn) |
| `not valid excode` 로 시세 드롭 | 브리지 excode='B' ↔ algotrd market 테이블(market_stub) 매핑 확인 |
| `mkfifo EEXIST` / `index not found` | mat clean-slate 누락 — `mat-start.sh` 가 FIFO/IPC 정리 포함, 재실행 |
| 체결됐는데 OCO 취소 안 됨 | WD300002 로그에 `오토헷지 fail 1403` 이면 `AUTO_TRADE_INFO` 시드 누락 (위 표) |
