# MCI 테스트 런북

> WTG 전체 stack 을 직접 띄우고 시나리오별로 검증하는 가이드
> 최종 업데이트: 2026-05-03
> 짝 문서: [mci-architecture.md](./mci-architecture.md)

---

## 0. 한 가지만 기억하면 됨 — `wtgctl`

`wtgctl` 한 도구로 전체 stack 을 제어한다. PATH 에 등록되어 있다.

```
wtgctl status                          # 현재 상태 한 줄 dump
wtgctl start                           # 모두 기동 (이미 떠 있는 건 skip)
wtgctl stop                            # mci-* 와 forwarder 종료. broker 컨테이너 유지
wtgctl stop --all                      # broker 컨테이너까지 종료
wtgctl logs <name>                     # api|push|admin|fwd|broker 로그 tail -f
wtgctl test tx                         # /v1/tx echo 한 번
wtgctl test push                       # /v1/admin/push-test 한 번
wtgctl quote SMB USDKRW 1380.5         # 단발 시세 발사
wtgctl burst start [pat]               # 시세 시뮬레이션 — walk(기본)|trend|downtrend|volatile|spike|calm
wtgctl burst stop                      # burst 종료
wtgctl burst status                    # 진행 상태 + 현재 패턴

wtgctl alias                           # 등록 alias 목록 (라우팅 룰 store)
wtgctl alias NAME [data...]            # alias 호출 (자동 JSON wrap)
wtgctl routes add NAME EXCH RKEY [c]   # cfg 에 룰 추가 (≤ 2초 hot reload)
wtgctl routes del NAME                 # cfg 에서 룰 제거
wtgctl routes set NAME field=value...  # 필드 수정

WTG_ROUTES_POLICY=sync wtgctl start    # cfg 가 진실의 원천 (del/set 즉시 반영)
WTG_EDGE=1 wtgctl start                # mci-edge-* DMZ proxy 까지 같이 기동
WTG_EDGE_ALLOW_CIDRS="10/8,192/16"     # edge 의 IP 화이트리스트
wtgctl edge {start|stop|status}        # edge 만 따로 제어
```

`wtgctl` 만 쳐도 사용법이 표시된다.

---

## 1. 사전 점검

```bash
wtgctl status
```

기대 결과 (모두 1/up):

```
broker (docker mymqd) : Up ...
mci-api               : up (PID ...)
mci-push              : up (PID ...)
mci-admin             : up (PID ...)
quote-forwarder       : up (PID ...)

TCP listen:
  mci-api :8080             1
  mci-push (ws) :8081       1
  mci-admin (UI) :9090      1
  broker :11217             1
UDP listen (forwarder):
  SMB :30044                1
  KMB :30045                1
  EBS :30046                1
  REUT :30051               1
```

하나라도 down (0) 이면:

```bash
wtgctl start
```

---

## 2. 시나리오 A — 브라우저 진입

```
http://127.0.0.1:9090/
```

로그인 화면:
1. **"개발 모드로 진입"** 펼치기
2. 사용자 ID 입력 (예: `lcr123`)
3. **"ID 만으로 입장 (DevMode)"** 클릭

진입 후 사이드바: 대시보드 / 라우팅 룰 / 정책 엔진 / 브로커 명령 / API 테스터
/ WS 모니터 / **시세** / 감사 로그.

검증 포인트:
- 헤더의 `stream: ●` 가 connected
- 대시보드 KPI 카드가 2초마다 갱신
- 사이드바 하단에 입력한 ID 가 표시됨

---

## 3. 시나리오 B — Tx echo round-trip

### 3.1 UI 에서

브라우저 **API 테스터** 진입 → 상단 "사전 설정" 줄에서 **`POST Tx echo`** 클릭.

자동 채워지는 body:
```json
{"exchange":"TSTSVC","routing_key":"PING","data":{"hello":"ui-test"}}
```

**▶ 실행** 클릭 → 응답 패널:
- `200 OK · 7~15ms`
- 요청 헤더: `Content-Type: application/json`, `X-WTG-User: lcr123`
- 응답: `{"data":"ECHO:{\"hello\":\"ui-test\"}"}`

### 3.2 음성 검증 — broker 라우팅 거부

body 의 `routing_key` 를 `NOEXIST` 로 바꿔서 실행 →

```
422 Unprocessable Entity
{"errn":1002,"errm":"MB-1002 No applications to receive a message"}
```

이게 떨어져야 broker 의 라우팅 거부 로직이 살아 있다는 검증이 된다.

### 3.3 터미널에서

```bash
wtgctl test tx
# {"data":"ECHO:{\"hello\":\"wtgctl\"}"}
```

### 3.4 부하 검증

```bash
for i in {1..100}; do wtgctl test tx > /dev/null; done
wtgctl logs api    # http 200 N건 / 평균 dur 확인 (Ctrl-C)
```

---

## 3.4 내부 클라이언트 (CS / 백오피스) 호출 — mci 경유 권장

내부 도구도 **mci-api `:8080`** 을 호출한다 (DMZ edge 는 외부용). 이유와
근거는 `mci-architecture.md` §2.1 참조. 실용 cheat sheet:

```bash
# 0. alias 호출 (라우팅 룰 변경에 면역) — 운영 default
curl -H 'X-WTG-User: cs01' -X POST http://127.0.0.1:8080/v1/tx \
    -d '{"alias":"WECHO_PING","data":""}'

# 1. 즉석 검증용 raw envelope (alias 미등록 svc)
curl -H 'X-WTG-User: cs01' -X POST http://127.0.0.1:8080/v1/tx \
    -d '{"exchange":"ECHOSVC","routing_key":"INFO","data":""}'

# 2. wtgctl 한 줄 (라우팅 정보 자동 wrap)
wtgctl alias WECHO_TIME
```

mci 경유 시 자동 적용되는 layer (raw broker 직결로는 우회됨):
- 정책 엔진 (kill switch / maintenance / blocked symbols/rkeys)
- audit ring (mci-admin 의 감사 로그 화면에 prepend)
- IP allowlist / rate limit (edge 경유 시) / JWT 검증

raw broker 직결은 broadcast/sub 패턴 도구나 분당 수만건 batch 같은 *명시 예외*
에만 허용. 그 경우 정책 우회 책임을 운영팀이 등록 수용한다.

---

## 3.5 시나리오 B' — WECHO multi-rkey (운영 svcmain 패턴)

`win/src/trn/WECHO` 의 5 routing key (PING/ECHO/UPPER/TIME/INFO) 가 broker
entrypoint 에 의해 자동 기동. dev-seed 가 alias 6개 (TSTSVC_PING +
WECHO_{PING,ECHO,UPPER,TIME,INFO}) 를 라우팅 룰에 미리 등록.

### 3.5.1 라우팅 룰 화면

브라우저 사이드바 → **라우팅 룰** → 6 alias 가 자동 표시되어야 함:

| alias | exchange | routing_key |
|---|---|---|
| TSTSVC_PING | TSTSVC | PING |
| WECHO_PING | ECHOSVC | PING |
| WECHO_ECHO | ECHOSVC | ECHO |
| WECHO_UPPER | ECHOSVC | UPPER |
| WECHO_TIME | ECHOSVC | TIME |
| WECHO_INFO | ECHOSVC | INFO |

(updated_by="dev-seed". `+ 신규` / 활성 토글 / 수정 / 삭제 모두 동작.)

### 3.5.2 API 테스터 — alias preset

사이드바 **API 테스터** → preset 줄의 다음 buttons 클릭:

```
[POST WECHO PING]   [POST WECHO ECHO]    [POST WECHO UPPER]
[POST WECHO TIME]   [POST WECHO INFO]
```

각 preset 이 *alias 한 줄* 로 채워짐 (`{"alias":"WECHO_PING","data":""}`).
실행하면:

- `WECHO_PING` → `{"data":"PONG"}`
- `WECHO_ECHO` → `{"data":"ECHO:\"hello WECHO\""}`
- `WECHO_UPPER` → `{"data":"MAKE ME UPPERCASE"}`
- `WECHO_TIME` → `{"data":"2026-05-03T..."}`
- `WECHO_INFO` → `{"data":"WECHO pid=N up=Ms"}`

### 3.5.3 터미널에서

```bash
curl -sS -X POST http://127.0.0.1:8080/v1/tx \
    -H 'Content-Type: application/json' -H 'X-WTG-User: tester01' \
    -d '{"alias":"WECHO_PING","data":""}'
# → {"data":"PONG"}
```

운영 패턴과의 차이는 `win/src/trn/WECHO/WECHO.c` 가 svcmain 우회 (자체 main +
mymq_bind_services) 라는 점. svcmain 정상화 후엔 callme[] 와 main wrapper 만
갈아끼우면 운영 W3100 같은 형태.

---

## 4. 시나리오 C — Push (admin → ws fan-out)

### 4.1 ws 연결

브라우저 **WS 모니터** → URL 그대로 (`ws://localhost:8081/v1/subscribe`) →
**▶ 연결**.

상태가 `● live` 가 되어야 정상. (DevMode 라 `?x_wtg_user=lcr123` 자동 첨부됨.)

### 4.2 push 발사 (자기 자신에게)

같은 페이지의 사이드바 → **API 테스터** → preset **`POST Push 발사`** 클릭.

body 의 `user` 를 *방금 로그인한 ID* 로 변경:
```json
{"user":"lcr123","data":{"event":"alert","text":"hello via push"}}
```

**▶ 실행** → `200 OK {"sent":true,"target_uid":"lcr123","body_size":...}`

### 4.3 ws 수신 확인

사이드바 **WS 모니터** 로 돌아가면 스트림에:
```
← {"func":13,"subc":54,"logon_id":"lcr123","data":{"event":"alert","text":"hello via push"}}
```

### 4.4 음성 검증

body 의 `user` 를 다른 ID (`other999`) 로 바꿔서 실행 → broker 로그에
`Published 0/N to 'other999'` (그 user 로 connect 한 ws 가 없음).

```bash
wtgctl logs broker | grep "Published"
```

### 4.5 터미널에서

```bash
wtgctl test push    # tester01 로 발사 — 그 ID 로 ws connect 안 했으면 broker 가 0/N
```

---

## 5. 시나리오 D — 시세 (UDP → broadcast → ws)

### 5.1 시세 화면

브라우저 사이드바 **시세** → 자동 ws connect.

처음에는 "아직 시세 없음" 메시지.

### 5.2 단발 발사

터미널에서:

```bash
wtgctl quote SMB  USDKRW 1380.55
wtgctl quote KMB  EURUSD 1.0851
wtgctl quote EBS  USDJPY 156.21
wtgctl quote REUT GBPUSD 1.2741
```

→ 시세 화면에 4 개 통화쌍 카드 출현 (BID/LAST/ASK 3 셀, qty, ▲▼ 변동, spread).
체결 리스트에 4 줄 prepend.

### 5.3 시세 시뮬레이션 — 패턴 6종

```bash
wtgctl burst start [pattern]
# 브라우저 시세 화면 — 호가가 0.5초마다 펄럭, 체결 리스트 누적 (최대 50)
wtgctl burst stop
wtgctl burst status      # 현재 진행 패턴
```

| 패턴               | delta 분포              | 사용 장면                    |
| ---------------- | --------------------- | ------------------------ |
| `walk` (default) | ±0.05 균등              | 평탄한 random walk          |
| `trend`          | drift +0.02 + ±0.025  | 위쪽 천천히 흐르는 시장            |
| `downtrend`      | drift −0.02 + ±0.025  | 아래쪽 천천히 흐르는 시장           |
| `volatile`       | ±0.20 (4× walk)       | 단기 차트 흔들림 / sparkline 검증 |
| `spike`          | 평소 ±0.025, 5% 확률 ±1.0 | 이상치 / circuit breaker 검증 |
| `calm`           | ±0.01 (5× 작음)         | 저변동 장                    |

상단 KPI 도 갱신: tick/sec, 통화쌍 수, 체결 건수. trend / downtrend 면 시세
카드의 ▲▼ 누적 변동 화살표가 한 방향으로 쏠리고, sparkline 도 한쪽으로 기운다.

내부 동작 메모:
- 매 tick 마다 `burst_delta <pattern>` 가 awk 1회로 delta 계산. srand seed 가
  `$RANDOM` 으로 매 호출 다르게 들어가서 4 feed 가 동일 random 을 받는 macOS
  awk 의 1초 시드 correlation 버그를 피한다.
- UDP 송신은 `nc -u -w 0` (즉시 close). 이전 `-w 1` 은 매 호출 1초 stall 해서
  실효 tick rate 이 ~5s 까지 늘어졌었음 — 6× 이상 throughput 회복.

### 5.4 raw FIX 검증

WS 모니터에서도 시세를 raw envelope 으로 볼 수 있다 (시세 화면이 가공된
호가창을 보여주는 동안 WS 모니터는 원본 JSON 흐름을 그대로 보여줌).

---

## 6. 시나리오 E — 정책 엔진 (kill switch)

### 6.1 전체 차단 (legacy)

브라우저 **정책 엔진**:

1. 적용 범위 = "전체" (default)
2. **Kill Switch** 토글 ON
3. ≤ 2 초 대기 (mci-api 가 mci-admin 의 정책 snapshot 을 poll)
4. 다른 탭에서 **API 테스터 → Tx echo 실행** → `503` 응답
   `{"error":"kill_switch","message":"운영 정책으로 모든 거래가 일시 차단됨"}`
5. Kill Switch 토글 OFF → ≤ 2 초 후 다시 Tx echo → `200` 정상

### 6.2 채널별 차등화 — 사고 시 고객만 차단, 직원 거래 유지

5채널 모델의 핵심 운영 시나리오. UI 의 *적용 범위* 에서 preset `고객만
(WEB/MOB/HTS)` 클릭 → Kill Switch 활성화 → 고객 거래는 모두 503, 직원 (ADM/EMP)
은 비상 거래 가능.

**API 테스터로 검증** — 우상단의 채널 select drop-down 으로 spoof:

1. 채널 선택 = `WEB` → Tx echo → `503 channel "WEB" 거래가 일시 차단됨`
2. 채널 선택 = `EMP` → Tx echo → `200 OK` (직원 거래 유지)

**터미널 검증**:
```bash
# 고객 채널만 차단
curl -X POST -H 'X-WTG-User: admin' http://127.0.0.1:9090/v1/admin/policy/kill-switch \
    -d '{"active":true,"channels":["WEB","MOB","HTS"]}'
sleep 2.5

# WEB 차단
curl -i -X POST -H 'X-WTG-User: cust1' -H 'X-WTG-Channel: WEB' \
    http://127.0.0.1:8080/v1/tx -d '{"alias":"WECHO_PING"}'
# → 503 채널 "WEB" 거래가 일시 차단됨 (kill switch)

# EMP 통과
curl -i -X POST -H 'X-WTG-User: dealer1' -H 'X-WTG-Channel: EMP' \
    http://127.0.0.1:8080/v1/tx -d '{"alias":"WECHO_PING"}'
# → 200 {"data":"PONG"}
```

UI 의 "적용 범위" 라벨에 현재 적용 채널 표시 (예: `WEB, MOB, HTS`). 비활성
상태일 때는 체크박스가 마지막 적용 값과 sync 되어 있음 — 다음 활성화 시 그대로
사용됨.

터미널 검증:
```bash
# admin 측 토글
curl -X POST -H 'X-WTG-User: admin' http://127.0.0.1:9090/v1/admin/policy/kill-switch -d '{"active":true}'
sleep 2.5
# api 측 차단
curl -i -X POST -H 'X-WTG-User: t' http://127.0.0.1:8080/v1/tx -d '{"alias":"WECHO_PING","data":""}'
# → HTTP/1.1 503 Service Unavailable
# → {"error":"kill_switch","message":"..."}
curl -X POST -H 'X-WTG-User: admin' http://127.0.0.1:9090/v1/admin/policy/kill-switch -d '{"active":false}'
```

`block_symbols` / `block_routing_keys` 도 같은 패턴 (envelope 의 `routing_key`
또는 `data.symbol` 매칭). symbol/rkey 차단은 403, kill_switch/maintenance 는 503.

내부 동작:
- 운영에서는 etcd 가 source of truth — mci-admin 변경 → etcd Put → mci-api 가
  watch 받아 `Engine.ApplyRemote` 즉시 반영 (`pkg/policy.StartEtcdSync`).
- DevMode (etcd 없음) 에서는 mci-api 가 `--dev-policy-url=http://...:9090/v1/admin/policy`
  를 2 초 주기로 GET → 받은 State 를 `ApplyRemote` (`pkg/policy.StartHTTPPoll`).
  wtgctl 이 `cmd_start` 에서 자동으로 이 flag 를 주입한다. 이게 없으면 admin 과
  api 가 각자 in-memory Engine 을 가져 *split-brain* — 토글이 admin UI 에서만
  보이고 실제 거래는 차단되지 않는다.

---

## 7. 시나리오 F — 라우팅 룰 (alias)

DevMode 부팅 시 6 alias 가 **자동 시드** (`~/mymq/etc/wtg-routes.json` →
TSTSVC_PING + WECHO_*).

### 7.1 라우팅 룰 화면

브라우저 **라우팅 룰**:

1. 자동 시드 6개 항목 (updated_by=`dev-seed`) 자동 표시
2. 각 entry 의 액션 영역에 **`▶ 테스트` `수정` `삭제`** 버튼

### 7.2 ▶ 테스트 버튼 (deep link) + 채널 spoof

WECHO_PING 행의 **`▶ 테스트`** 클릭:
- API 테스터 화면으로 자동 navigate
- body 자동 채워짐: `{"alias":"WECHO_PING","data":""}`
- toast 알림: "API 테스터로 이동 — alias=WECHO_PING"
- **▶ 실행** → `{"data":"PONG"}`

**채널별 검증**: 라우팅 화면 우상단의 *테스트 채널* select 에서 `WEB`/`MOB`/
`HTS`/`ADM`/`EMP` 중 하나를 고르고 ▶ 테스트 클릭 → 테스터에 같은 채널이 자동
세팅되어 ▶ 실행 시 `X-WTG-Channel` 헤더가 함께 발송됨. 이 select 와 테스터의
채널 select 는 같은 `LS_TESTER_CHANNEL` localStorage 키를 공유 — 한 곳에서
바꾸면 양쪽 모두 즉시 반영. 정책 차등화 시나리오 (§6.2) 검증을 라우팅 화면에서
바로 진행 가능.

### 7.3 동적 alias preset

API 테스터 화면 상단 preset 줄:
```
[정적] [GET 헬스체크] [GET Status] ... [POST Tx echo] [POST Push 발사]
│ ALIAS  ← 구분선
[POST TSTSVC_PING] [POST WECHO_ECHO] [POST WECHO_INFO] ...   ← 동적
                                                              (라우팅 룰 fetch)
```
각 alias button 의 tooltip 에 `exchange/routing_key — comment` 표시.

### 7.4 새 alias 추가 — UI

1. **+ 신규** 클릭 → 모달:
   - alias: `MY_PING`, exchange: `ECHOSVC`, routing_key: `PING`, active: on
2. **저장**
3. 라우팅 룰 화면에 즉시 표시 + `▶ 테스트` 가능
4. API 테스터 재진입 → preset 줄에 `MY_PING` 자동 출현

### 7.5 새 alias 추가 — cfg 파일 (재기동 0)

직접 편집 또는 `wtgctl routes add` 사용:

```bash
# ~/mymq/etc/wtg-routes.json 의 routes 배열에 한 줄 추가:
{"alias":"NEW_PING","exchange":"ECHOSVC","routing_key":"PING","active":true,
 "comment":"hot reload 검증"}

# 또는:
wtgctl routes add NEW_PING ECHOSVC PING "hot reload 검증"
```

≤ 2초 후 양쪽 (mci-admin + mci-api) 가 자동 재시드:
```
mci-admin: dev-routes cfg 변경 감지 — 재시드
mci-api:   dev-routes cfg 변경 감지 — 재시드
```

라우팅 룰 화면 새로고침 → `NEW_PING` 출현 → ▶ 테스트 → 동작.

### 7.6 활성 토글 검증

라우팅 룰 화면에서 어느 alias 의 `● active` 클릭 → `○ inactive` →
**▶ 테스트** → `400` 또는 `404` (alias 비활성). 다시 토글 ON → 정상 응답.

### 7.7 시드 정책 — additive vs sync

`--dev-routes-policy` flag 또는 `WTG_ROUTES_POLICY` env (wtgctl).

**additive (기본)** — UI 편집 보존 모드:
- DevMode (`--dev=true`): cfg 파일 우선, 없으면 hardcode default
- cfg 에 신규 alias → ✓ 자동 추가
- cfg 에서 기존 alias 변경 → ⚠ 무시 (UI 편집 보존)
- cfg 에서 alias 삭제 → ⚠ in-memory 에 그대로

**sync** — cfg 가 진실의 원천 모드:
- 모든 cfg alias 를 upsert (변경 필드 덮어쓰기)
- cfg 에 없는 in-memory alias 는 hot reload 시 Delete
- `wtgctl routes del/set` 이 즉시 in-memory 에 반영 (≤ 2초)
- 주의: UI 만으로 추가한 룰은 hot reload 시 사라짐

**검증 시나리오**:
```bash
# 1. sync 모드로 부팅
WTG_ROUTES_POLICY=sync wtgctl start

# 2. 임시 alias 추가 → 호출 가능 확인
wtgctl routes add MYSYNC_TEST ECHOSVC PING "sync 정책 검증"
sleep 3
wtgctl alias MYSYNC_TEST       # → {"data":"PONG"}

# 3. 삭제 후 ≤ 2초 — in-memory 에서도 자동 제거
wtgctl routes del MYSYNC_TEST
sleep 3
wtgctl alias MYSYNC_TEST       # → "alias 미등록" 류 에러

# 4. 같은 시나리오를 additive (기본) 로 돌리면 step 3 의 alias 호출이 여전히 성공
```

운영 모드 (`--dev=false`) 에서는 dev-seed 자체가 호출되지 않으며 etcd 가
source of truth. 정책 flag 도 무의미.

---

## 7.8 시나리오 F' — 서비스 명세 (svc I/O)

매매 svc 의 input/output 구조 (헤더파일) 를 admin UI 에서 직접 조회 + 채널별
호출 코드 자동 생성.

### 7.8.1 인덱싱 동작 확인

```bash
# 부팅 로그에 인덱싱 결과 — 두 디렉터리 각각.
grep "svcio: 헤더 인덱싱 완료" /tmp/mci-admin.log
# loaded=819 (~/mywork/win/src/inc/trn — 운영 W svc 헤더)
# loaded=6   (~/mymq/etc/svc-headers — dev WECHO/TSTSVC svc 헤더)
```

`--svc-inc-dir` 는 콤마 구분 다중 디렉터리 지원. wtgctl 이 자동으로 두 path 모두
주입 (운영 헤더 + dev 헤더). 다른 경로면 `WTG_SVC_INC_DIR=path1,path2 wtgctl
start` 로 override.

**dev 헤더 (총 6개)** — broker entrypoint 가 자동 기동하는 svc 들의 wire spec:
| 헤더 파일 | alias | 동작 |
|---|---|---|
| `TSTSVC_PING.h` | `TSTSVC_PING` | broker entrypoint test_service single rkey |
| `ECHOSVC_PING.h` | `WECHO_PING` | 빈 입력 → "PONG" 4byte |
| `ECHOSVC_ECHO.h` | `WECHO_ECHO` | "ECHO:" + 입력 |
| `ECHOSVC_UPPER.h` | `WECHO_UPPER` | 입력 ASCII 대문자 변환 |
| `ECHOSVC_TIME.h` | `WECHO_TIME` | YYYYMMDDHHMMSS 14byte |
| `ECHOSVC_INFO.h` | `WECHO_INFO` | "WECHO pid=N up=Ms" 가변 |

dev 헤더 컨벤션은 `<exchange>_<routing_key>.h` — UI 의 cross-ref 매칭이 이
underscore 형식도 alias 매핑으로 인식 (`r.exchange + "_" + r.routing_key`).

**헤더 편집 — UI 에서 즉석 수정 + 자동 재파싱**

서비스 명세 화면의 우측 상세 하단 `📝 헤더 source 편집` 패널 (펼치기) 에서
직접 헤더 내용을 수정하고 저장 가능. 저장 시:
1. 현재 내용을 `<path>.bak` 로 자동 백업
2. UTF-8 그대로 파일에 쓰기
3. parser 가 즉시 재파싱 — input/output 트리, wire form, 채널별 gen 모두 갱신

**보호**:
- dev 헤더 (`~/mymq/etc/svc-headers/`) 만 편집 가능. 패널 헤더에 `(dev 헤더 — 편집 가능)` 표시.
- 운영 헤더 (`~/mywork/win/src/inc/trn/`) 는 **read-only** — UI 가 textarea 잠그고 저장 버튼 비활성. `📝 ... (운영 헤더 — read-only)` 표시. 서버측에서도 `403 read_only` 반환 (이중 가드).
- 빈 내용 거부 (실수로 파일 비우는 사고 방지).
- 파싱 실패 시 422 + 에러 메시지 (.bak 복원 안내).

**활용 — 헤더 바꿔가며 wire 검증 cycle**:
1. ECHOSVC_INFO 의 output 을 `result[64]` 에서 `result[32], extra[16]` 로 변경
2. 저장 → 자동 재파싱 → wire form 갱신
3. ▶ wire 테스트 클릭 → 새 layout 으로 응답 parse
4. 의도한 동작이면 그대로, 아니면 ↻ 재로드 또는 .bak 으로 복구

**dev vs 운영 spec 구분 (UI badge)** — 좌측 목록의 각 항목에 batch:
- 🟢 `dev` : `~/mymq/etc/svc-headers/` 에서 온 헤더. dev broker 에 svc 가 떠 있음 → wire 테스트 동작.
- ⬜ `spec` : `~/mywork/win/src/inc/trn/` 에서 온 운영 헤더. dev broker 에 svc 미기동 → wire 테스트 시 **MB-1002** ("No applications to receive a message").

운영 spec 으로 wire 테스트해서 MB-1002 가 나면 결과 panel 의 parsed 영역에
친절한 안내가 자동 출력됨 ("dev broker 가 운영 svc 를 띄우지 않습니다 — dev
검증은 [dev] badge 항목 사용"). 운영 svc 의 spec 자체는 마이그레이션 매핑/
디버깅 용도로 그대로 valid — 단, 실제 호출은 운영 broker 에서만 가능.

### 7.8.2 UI 화면 — 좌측 목록 + 우측 상세

브라우저 사이드바 → **서비스 명세** 클릭:

1. 좌측 검색창에 `FIX` → 한글 "FIX환경파일조회" 등 일치 항목 표시
2. `W3382S01` 클릭 → 우측 상세
   - Code 헤더 + 한글 Program Name
   - **alias 매핑** — 라우팅 룰에 등록된 alias 가 있으면 표시 (없으면 안내)
   - Input 섹션 — `base_ymd [8] 기준년월일` 등 트리
   - Output 섹션 — `grid01_cnt [6] 그리드건수` + nested grid (orec) children
3. 하단 **호출 코드 gen** — 탭 4종 (curl / wtgctl / JS / Python). 같은 svc data
   에 채널별 template 만 다르게 적용된 예시. 클릭 → 자동 갱신.

### 7.8.3 터미널 검증

```bash
# 목록 (검색 옵션 q)
curl -H 'X-WTG-User: admin' "http://127.0.0.1:9090/v1/admin/svc-io?q=FIX&max=5"

# 단건
curl -H 'X-WTG-User: admin' "http://127.0.0.1:9090/v1/admin/svc-io/W1104S01" | python3 -m json.tool
```

응답에는 `code/name/input/output/records` 가 직렬화된다. UI 가 이걸 그대로
렌더링.

### 7.8.4 호출 형식 — API vs 전문 (wire)

같은 svc 도 호출 형태가 두 가지 — REST/JSON envelope vs 전통 wire frame
(C struct 직렬화). 서비스 명세 화면 우상단의 *형식* 라디오로 전환:

| 형식 | envelope | 출력 | 실행 |
|---|---|---|---|
| **API** | `POST /v1/tx` + JSON | 채널별 호출 코드 (5탭) | ▶ 테스트 (테스터) |
| **전문 (WIRE)** | broker wire frame | C struct offset 표 + sample 코드 + ▶ wire 테스트 form | ▶ wire 테스트 (svcio 직접) |

**WIRE 형식 실행** — *클라이언트는 REST 호출* 하지만 mci-admin 이 *서버 측에서*
SvcSpec.Input layout 으로 wire frame 직렬화 → broker 송신 → 응답 byte 를
SvcSpec.Output layout 으로 parse → JSON 응답.

- broker 는 legacy native client 가 보낼 wire frame 과 *동일한 byte* 수신
- mci 의 정책/감사 layer 는 *그대로 통과* — kill switch / blocked rkeys /
  audit 모두 정상 적용
- 한글 필드는 CP949 인코딩 (legacy 호환), char[N] 은 우측 공백 fill (left-aligned)
- 가변 grid (orec[]) 응답은 직전 *_cnt 필드의 ASCII int 로 record 횟수 결정

**공통 헤더 (header + body 구조)**

운영 svc 의 wire frame 은 **`[COMHDR(256)][TX_BODY]`** 구조 — 모든 W svc 가
공유하는 256 byte transaction 헤더 (전문코드 / 사용자 ID / 화면번호 / IP /
응답코드 등 25 필드) + 그다음 svc 별 Input 본문.

mci-admin 부팅 시 `--svc-common-header=...comhdr.h` (wtgctl 자동 주입) 로
파일 한 번 파싱 → `COMHDR` / `BROADCAST_H` / `CHGHDR` 등 named header 로 등록.

**등록된 헤더 보기** (UI):
서비스 명세 화면 우상단의 `📋 공통 헤더 (N)` 버튼 → 모달 — 등록된 모든
named header 를 펼쳐 표시. 각 항목: 이름 / 필드 수 / 총 byte / 펼쳐서 필드
트리. COMHDR 는 default 로 펼쳐짐.

```
COMHDR              25 필드 · 256 byte    ← 펼쳐서 trxc[16]/scrn[6]/.../udef[45] 트리 표시
BROADCAST_H          6 필드 ·  48 byte
ALERTCAST_H          6 필드 · 612 byte
REPORT_COM_HEADER    4 필드 ·  26 byte
```

API: `GET /v1/admin/svc-io/headers` → `{headers: [{name, fields, size, field_count}]}`.

**spec 의 `header_type` 필드 결정**:
- 운영 dir (`win/src/inc/trn`) → 기본 `COMHDR` (자동)
- dev dir (`svc-headers`) → 기본 비어있음 (raw body)
- 헤더 파일 안에 `// @wtg-header: NAME` 주석으로 svc 별 override 가능

서비스 명세 화면의 우측 상세에 *공통 헤더* 섹션이 spec.header_type 있을 때만
자동 표시 — 25 필드 트리 + 256 byte 합계.

▶ wire 테스트 form 도 동일 — *공통 헤더 입력* details (펼치기) 가 spec.
header_type 있을 때만 보이고, 사용자가 trxc / usid / scrn 등 입력. trxc 는
미입력 시 svc code 자동 채움. 응답 panel 도 *parsed Header* / *parsed Output*
두 영역으로 분리해서 표시.

**WIRE 호출 endpoint**:
```
POST /v1/admin/svc-io/{code}/test-wire
{
  "channel": "EMP",
  "exchange": "ECHOSVC",
  "routing_key": "PING",
  "input": {"lgen_no":"0001", "lgen_nm":"홍길동", ...}
}

→ {
  "code":"W1104S01",
  "channel":"EMP",
  "sent_bytes": 138,
  "sent_hex": "...wire frame hex...",
  "recv_bytes": 4,
  "recv_hex": "504f4e47",  // PONG
  "parsed": {"orec":[{...}]},
  "duration_ms": 25
}
```

UI 사용: 서비스 명세 화면 → 좌측 svc 선택 → 우상단 형식=`전문` 선택 → 입력
form 자동 채워짐 (Input 필드별 char[N]) → 우측 채널/exchange/rkey 조정 →
**▶ wire 테스트** 클릭 → 결과 panel (status / sent hex / recv hex / parsed
Output) 표시.

**WIRE 형식 spec 출력** (display 부분, 예: W1104S01):
```
─── Input (W1104S01_I) ─────────────────────────────
offset  size  name                     type   comment
     0   10   lgen_no                  char   거래참여자번호
    10   70   lgen_nm                  char   거래참여자명
    80   13   cprno                    char   법인등록번호
    ...
   134    4   scrn                     char   화면번호
total Input size: 138 byte (가변 grid 제외)
```

용도:
- **마이그레이션 매핑** — legacy 팀이 wire frame 과 mci API envelope 의
  대응을 한 화면에서 비교
- **디버깅** — broker 로그의 byte dump 와 SvcSpec 필드 매핑 추적
- **교육** — 새 직원에게 두 호출 방식의 관계 설명

**라우팅 화면 ▶ 테스트 의 형식 분기**:
- 형식=`API` 선택: API 테스터로 deep link → ▶ 실행 (현재 동작)
- 형식=`WIRE` 선택: 서비스 명세 화면으로 이동, 해당 alias 의 svc spec 자동
  선택 + 전문 layout 표시. *실행 경로 없음* — toast 안내 + 화면에 spec 만.

라우팅 화면과 서비스 명세 화면의 형식 select 는 같은 `LS_SVCIO_FORMAT`
localStorage 키 공유 — 한 곳에서 바꾸면 양쪽 일관 반영.

### 7.8.5 채널별 gen — 동일 데이터, 다른 출력

`pkg/svcio` 가 *채널 무관* 한 SvcSpec 을 반환하고, UI 의 `SVCIO_GEN` 이
**5채널** template 을 갖는다 (pkg/mymq.ChannelCode 와 정렬). 탭은 `WEB/MOB/HTS
│ ADM/EMP` 순으로 외부(고객) / 내부(직원) 시각 분리:

| 탭 | 사용자 | 진입 | 인증 | 클라이언트 |
|---|---|---|---|---|
| `WEB` | 고객 | mci-edge-api (DMZ) | JWT | 브라우저 fetch |
| `MOB` | 고객 | mci-edge-api (DMZ) | JWT (keychain) | Swift HTTP |
| `HTS` | 고객 | mci-edge-api (DMZ) | 고객 SSO / JWT | cs native desktop (Home Trading System) |
| `ADM` | 직원 | mci-admin /v1/admin/tx-test | 직원 SSO / X-WTG-User | curl / UI |
| `EMP` | 딜러/직원 | mci-api :8080 직결 | 직원 SSO / X-WTG-User | cs native desktop (HTS와 같은 framework) |

**HTS / EMP 의 관계** — 기술 framework 는 같지만 사용자가 정반대:
- HTS = **고객** (DMZ edge 경유, 자기 계좌만, kill switch 시 차단 대상)
- EMP = **딜러** (Internal 직결, 모든 계좌, kill switch 시 비상 거래 허용 가능)

`ChannelCode.IsCSFramework()` 로 *기술 동질성* (rate limit / wire format 정책)
은 묶고, `IsCustomer()` / `IsEmployee()` 로 *권한 그룹* 은 분리.

---

## 8. 시나리오 G — 감사 로그

위 §6/§7 의 모든 mutation (정책 토글, 룰 등록/수정/삭제) 이 **감사 로그**
화면 timeline 에 즉시 prepend (ws push). 직접 새로고침 하지 않아도 실시간
반영된다.

테스트:
1. 라우팅 룰 화면에서 alias 추가/수정/삭제 한 번씩
2. 정책 엔진 화면에서 Kill Switch 토글 한 번
3. 감사 로그 화면 → 4 개 entry 가 timeline 에 박혀 있어야 함

---

## 9. 시나리오 H — 대시보드

**대시보드**:
- KPI 카드 (활성 룰 수 / RTT / broker connected)
- mini sparkline (최근 12 포인트)
- Chart.js 시계열 (60 포인트 / 2분 / 2초 polling)

`wtgctl burst start` 돌리는 동안 broker connect / 메시지 흐름 그래프가 펄럭.

---

## 10. 운영 시나리오 (mci-edge-* DMZ 까지)

DMZ proxy 3종 (`mci-edge-api` / `mci-edge-push` / `mci-edge-price`) 을 같이
띄워 외부 노출 layer 를 시뮬레이션. wtgctl 이 같이 관리한다.

### 10.1 기동 방법

```bash
# 모두 (internal + edge) 한 번에 기동
WTG_EDGE=1 wtgctl start

# 또는 edge 만 따로 (internal 이미 떠 있을 때)
wtgctl edge start
wtgctl edge status
wtgctl edge stop
```

기본 포트:
| 서비스 | DMZ 포트 | upstream (internal) |
|---|---|---|
| mci-edge-api | 8090 | http://127.0.0.1:8080 (mci-api) |
| mci-edge-push | 8084 | 127.0.0.1:50052 (mci-push gRPC, 미구축 시 ws 만 listen) |
| mci-edge-price | 8083 | 127.0.0.1:50051 (mci-price gRPC, 미구축 시 ws 만 listen) |

### 10.2 흐름 검증

```bash
# 브라우저 → DMZ edge → internal mci-api → broker → WECHO 까지
curl -H "X-WTG-User: tester01" -X POST \
     http://127.0.0.1:8090/v1/tx -d '{"alias":"WECHO_PING","data":""}'
# → {"data":"PONG"}
```

직접 `:8080` 호출과 같은 결과 — edge 가 *passthrough* 인지 확인됐다.

### 10.3 IP allowlist

운영 시 외부 출구 IP 만 열고 싶을 때:

```bash
WTG_EDGE=1 WTG_EDGE_ALLOW_CIDRS="10.0.0.0/8,192.168.0.0/16" wtgctl start
# 또는
WTG_EDGE_ALLOW_CIDRS="10.0.0.0/8" wtgctl edge start
```

비-허용 IP 의 요청은 **인증 / rate-limit / proxy 자원 모두 거치기 전** 에 403
으로 즉시 거부 (`pkg/netutil.IPAllowList` 미들웨어가 chain 의 가장 바깥).

```bash
# loopback 만 띄운 dev 환경에서 일부러 외부 CIDR 만 열어서 검증
WTG_EDGE_ALLOW_CIDRS="10.99.0.0/16" wtgctl edge start
curl -sI http://127.0.0.1:8090/v1/ping | head -1
# → HTTP/1.1 403 Forbidden (loopback 은 10.99/16 밖)
```

비고:
- `RemoteAddr` 만 사용 — 실제 운영에서 LB 뒤에 둘 거면 `X-Forwarded-For`
  처리 옵션이 추가로 필요. 현재는 직접 노출 또는 L4 LB (TCP-pass) 만 가정.
- 빈 값 = 모두 허용 (DevMode 기본).

### 10.4 TLS 종단 / mTLS

3 edge 모두 외부 측 TLS / mTLS 와 upstream mTLS 를 옵션으로 지원
(`--tls-cert` / `--tls-key` / `--tls-client-ca` / `--upstream-tls-*` 또는
`grpc-tls-*`). dev-stack wtgctl 은 TLS 미사용 (plaintext) 으로 띄우고,
운영에서는 ingress / LB 가 종단하거나 edge 자체가 종단한다. 자세한 인증서
주입은 `docs/broker-tls.md` 와 동일 패턴.

---

## 11. 빠른 진단 표

| 증상 | 원인 후보 | 확인 명령 / 조치 |
|---|---|---|
| 브라우저 9090 안 열림 | mci-admin down | `wtgctl status`, `wtgctl logs admin` |
| ws 연결 1006 abnormal | mci-push down 또는 캐시 또는 Origin 거부 | `wtgctl status`, 브라우저 `⌘+⇧+R`. mci-push 의 CheckOrigin 은 DevMode 모두 허용 |
| Tx echo 503 | broker 또는 test_service down | `wtgctl logs broker` 에 "No such routing point" 또는 "DESTINATION NOT FOUND" |
| Push ws 안 옴 | body 의 user ≠ ws connect ID | broker 로그 `Published 0/N to '<user>'` |
| 시세 카드 비어있음 | forwarder down 또는 송신 실패 | `wtgctl logs fwd` 에 `forwarded` 메시지 / `wtgctl quote SMB USDKRW 1380.5` 로 단발 검증 |
| HTTP 401 | DevMode 인데 X-WTG-User 안 들어감 | DevTools Console: `localStorage.getItem("wtg_user")` 와 `wtg_dev_mode` 확인. 다시 로그인 |
| HTTP 422 (validation) | envelope 형식 (alias/routing_key 둘 다 없음) | API 테스터 응답 panel 의 errm |
| Push 발사 후 broker 가 0/N | tester user 로 ws 연결한 인스턴스가 없음 | 브라우저 WS 모니터에서 ws connect 후 다시 발사. body 의 user 가 ws ID 와 일치해야 함 |
| WECHO 호출이 404/422 | alias 가 라우팅 룰에 없음 | `curl -H 'X-WTG-User: t' http://127.0.0.1:9090/v1/admin/routes` 로 확인. dev-seed 가 안 돌았으면 mci-admin / mci-api 재기동 |
| forwarder 가 broker 끊김 | heartbeat 타임아웃 | `wtgctl logs fwd` 의 `heartbeat 타임아웃` → Reconnect 가 자동 재접속. `curl http://127.0.0.1:9091/stats` 의 `publish_errors` 카운터 확인 |
| mci-push 가 ws 까지 안 흘림 | dispatcher drop | `curl -H 'X-WTG-User: t' http://127.0.0.1:8081/v1/push-stats` 의 `dispatcher_dropped` 가 늘면 LogonID 매칭 실패 |

---

## 12. 한 번에 시각적 검증 — 추천 시퀀스

브라우저 시세 화면 띄워둔 채 터미널에서:

```bash
wtgctl burst start
# 브라우저: 시세 화면에서 4 통화쌍 펄럭 + sparkline 갱신 + 체결 누적

wtgctl test tx
# 터미널: ECHO:... 응답
# 브라우저 API 테스터에서도 동일하게 실행 → 같은 결과

# 브라우저 WS 모니터로 가서 ws connect (ID = 자기 로그인 ID)
# 그 다음 API 테스터의 Push 발사 preset 으로 body 의 user 를 자기 ID 로 → 실행
# → WS 모니터 화면에 메시지 한 줄 도착

# 브라우저 라우팅 룰 화면 → WECHO_TIME 의 ▶ 테스트 클릭 → 실행
# → API 테스터에서 YYYYMMDDHHMMSS 응답

# 새 alias 추가 (cfg hot reload 검증) — 재기동 없이:
echo '{"alias":"DEMO","exchange":"ECHOSVC","routing_key":"PING","active":true,
       "comment":"hot reload"}' | python3 -c "
import sys, json
p = '$HOME/mymq/etc/wtg-routes.json'
d = json.load(open(p))
d['routes'].append(json.loads(sys.stdin.read()))
json.dump(d, open(p, 'w'), indent=2, ensure_ascii=False)"
sleep 3
# 라우팅 룰 화면 새로고침 → DEMO 자동 출현 → ▶ 테스트 → "PONG"

wtgctl burst stop
```

---

## 13. 종료

```bash
wtgctl stop          # mci-* + forwarder 종료. broker 컨테이너 유지
wtgctl stop --all    # broker 컨테이너까지 종료
wtgctl burst stop    # burst 만 종료
```

---

## 14. 자주 쓰는 호스트 명령 모음

### 14.1 broker 안 직접 확인

```bash
docker exec mymqd ps -ef                                # broker 안 프로세스
docker exec mymqd ls /opt/mymq/etc/                     # cfg 파일들
docker exec mymqd tail -f /opt/mymq/log/mymqd-*.log     # broker 메시지 로그
docker exec mymqd /opt/mymq/bin/test_client \
    -h 127.0.0.1:11217 -e TSTSVC -r PING -m call "hi"   # broker 안에서 echo 호출
```

### 14.1.1 metrics / stats endpoint

```bash
# forwarder
curl -s http://127.0.0.1:9091/stats | python3 -m json.tool   # uptime, received, published, errors
curl -s http://127.0.0.1:9091/metrics | grep quote_           # Prometheus

# mci-push
curl -sH 'X-WTG-User: t' http://127.0.0.1:8081/v1/push-stats | python3 -m json.tool
curl -sH 'X-WTG-User: t' http://127.0.0.1:8081/metrics | grep mci_push_dispatcher

# mci-admin (라우팅 룰 / 정책 / 감사)
curl -sH 'X-WTG-User: t' http://127.0.0.1:9090/v1/admin/routes | python3 -m json.tool
curl -sH 'X-WTG-User: t' http://127.0.0.1:9090/v1/admin/policy | python3 -m json.tool
curl -sH 'X-WTG-User: t' http://127.0.0.1:9090/v1/admin/audit  | python3 -m json.tool
```

`wtgctl status` 도 forwarder / mci-push 카운터를 한 줄 요약으로 표시.

### 14.2 호스트에서 mci-test 로 wire 검증

```bash
/Users/winwaysystems/mywork/wtg/build/bin/mci-test \
    --host=127.0.0.1 --port=11217 --ckey=0xCAFEBABE
# PASS 면 broker 가 ckey 를 echo 함 (option C 멀티플렉싱 가능)
```

### 14.3 mds 의 replay 도구 (옵션, 빌드 가능시)

```bash
# mds 의 trace 파일 재생 — 빌드된 바이너리가 있을 때만
/path/to/replay_smb2     # /app/fxwin/mds/test/SMB.trc → 127.0.0.1:30044
/path/to/replay_kmb2     # → 127.0.0.1:30045
/path/to/replay_ebs2     # → 127.0.0.1:30046
```

mds 의 cooker (WD9500) 까지 띄울 필요는 없다 — quote-forwarder 가 그 자리를
대신한다 (옵션 1 셋업). mds 가공된 시세를 받고 싶으면 옵션 2/3 으로 가야 함
([mci-architecture.md §3.3](./mci-architecture.md#33-시세-udp--broadcast--ws-fan-out) 참조).

---

## 15. 체크리스트 — 새 환경에 셋업할 때

1. [ ] mymq 빌드된 docker 이미지 `wtg-mymqd` 존재 (`docker images | grep wtg-mymqd`)
2. [ ] WTG 빌드 산출물 8개 (`mci-api`, `mci-push`, `mci-admin`, `mci-edge-*`, `mci-price`, `mci-test`)
3. [ ] `quote-forwarder` 빌드
4. [ ] `wtgctl` 스크립트 PATH 등록 (`/opt/homebrew/bin/wtgctl` 심링크)
5. [ ] `~/mymq/etc/` 에 broker cfg (`mymqd.cfg`, `mymqport.cfg`, `mymq.tab`)
6. [ ] **`~/mymq/etc/wtg-routes.json`** 존재 — alias 시드 cfg (없어도 hardcode default 동작)
7. [ ] `wtgctl start` → `wtgctl status` 모두 1/up
8. [ ] `http://127.0.0.1:9090/` 진입 + DevMode 입장
9. [ ] 시나리오 B (Tx echo) 통과
10. [ ] 시나리오 B' (WECHO multi-rkey, alias) 통과
11. [ ] 시나리오 C (Push) 통과
12. [ ] 시나리오 D (시세) 통과 (`wtgctl burst start`)
13. [ ] 시나리오 F (라우팅 룰 — ▶ 테스트 / cfg hot reload) 통과

---

## 부록 — 환경별 경로

| 항목 | 호스트 경로 |
|---|---|
| WTG 소스 | `/Users/winwaysystems/mywork/wtg` |
| WTG 빌드 산출물 | `/Users/winwaysystems/mywork/wtg/build/bin/` |
| mymq 소스 | `/Users/winwaysystems/mywork/mymq` |
| 호스트 운영 cfg | `~/mymq/etc/` (컨테이너 `/opt/mymq/etc` 로 mount) |
| 호스트 운영 로그 | `~/mymq/log/` (컨테이너 `/opt/mymq/log` 로 mount) |
| WTG alias 시드 | `~/mymq/etc/wtg-routes.json` (mci-admin/api 가 부팅 + 2초 polling 으로 hot reload) |
| win 운영 소스 | `/Users/winwaysystems/mywork/win/src/` (운영 reference, 손대지 않음) |
| win standalone 산출물 | broker container 안 `/opt/mymq/{bin,lib}/` (WECHO, WECHOSTD, logbackup, libcom_min.a, libwinmain_min.a) |
| mds 소스 | `/Users/winwaysystems/mywork/nmds/mds` |
| wtgctl 스크립트 | `~/mymq/bin/wtgctl` (PATH 심링크) |
| 컴포넌트 로그 | `/tmp/mci-api.log`, `/tmp/mci-push.log`, `/tmp/mci-admin.log`, `/tmp/quote-fwd.log` |
