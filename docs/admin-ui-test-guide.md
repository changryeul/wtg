# mci-admin UI 테스트 가이드

mci-admin (`http://127.0.0.1:9090/`) 의 모든 화면을 차례대로 둘러보며 동작을
검증하기 위한 시나리오 문서. UI 가 단일 진실이고, 백엔드 endpoint 는 보조 검증용.

---

## 0. 준비

### 0.1 기동
```bash
# broker 없이도 시각 확인 가능 (DevMode)
./build/bin/mci-admin --dev --no-broker --listen :9090

# broker 있는 환경 — wire/passthrough 까지 검증
./build/bin/mci-admin --dev --listen :9090 \
    --broker-host 10.0.0.10 --broker-port 11217
```

### 0.2 로그인 (DevMode)
1. 브라우저로 `http://127.0.0.1:9090/` 접속.
2. 로그인 화면 → **"개발 모드로 진입"** 클릭.
3. ID 입력 (예: `alice`) → **"ID 만으로 입장 (DevMode)"**.
4. 진입 후 우상단 `LIVE` 뱃지와 `stream: open` 이 보이면 ws 정상.
5. 좌측 사이드바 하단에서 로그인한 ID 확인.

### 0.3 공통 UI
- 좌측 사이드바: 16개 페이지 네비.
- 상단 topbar: 현재 페이지 제목 + LIVE 뱃지 + 스트림 상태 + 서버 시간.
- 우상단 토스트 영역 (성공/실패 알림).
- 사이드바 하단: 테마 토글 (다크/라이트/시스템).

### 0.4 검증 도구
- 백엔드 직접 호출 (선택): `curl -H "X-WTG-User: alice" http://127.0.0.1:9090/<path>`
- 감사 로그 (`감사 로그` 페이지) — 모든 mutation 의 사후 검증.

---

## 1. 대시보드 (`page-dashboard`)

### 1.1 무엇을 하는가
실시간 운영 지표 한눈에 — 활성 라우팅 룰 수, API 응답 시간, 브로커 상태,
현재 사용자 정보. 스파크라인은 최근 12개 샘플 (2초 간격 = 24초 윈도우).

### 1.2 화면 요소
| 카드        | 표시                          | 의미                                 |
| --------- | --------------------------- | ---------------------------------- |
| 활성 라우팅 룰  | `kpi-routes` 숫자 + 스파크       | `/v1/admin/routes` 의 active=true 수 |
| API 응답 시간 | `kpi-rtt` ms + avg          | 위 호출 자체의 RTT                       |
| 브로커       | `kpi-broker` CONNECTED/DOWN | broker connection state            |
| 현재 사용자    | `kpi-user` + channel        | 로그인 정보                             |

### 1.3 테스트 시나리오
1. **카드 표시 검증** — 4개 카드 모두 `—` 가 아닌 실제 값으로 채워짐.
2. **자동 갱신** — 24초 머무르며 스파크라인이 좌→우로 흐르는지.
3. **delta 부호** — 우상단 `±N` 가 라우팅 룰 증감에 따라 색 변경 (good/warn/bad).
4. **broker 카드** — broker 가 살아있으면 `CONNECTED` (good), 죽이면 `DOWN` (bad).

---

## 2. 라우팅 룰 (`page-routes`)

### 2.1 무엇을 하는가
alias → exchange / routing_key 매핑 CRUD. mci-api `/v1/tx` 의 `alias` 가
이 룰로 resolve 된다. etcd 에 저장되어 모든 mci-api 인스턴스가 watch.

### 2.2 화면 요소
- 상단 도구: 형식 (API/전문) · 채널 (자동/WEB/MOB/HTS/ADM/EMP) · ↻ 새로고침 · + 신규.
- 테이블: Alias / Exchange / Routing Key / Active / Updated / 액션 버튼.
- 빈 상태 안내.

### 2.3 테스트 시나리오
1. **목록 로드** — 초기 진입 시 테이블이 그려지고, `routes-empty` 가 적절히 토글.
2. **신규 등록** — `+ 신규` 클릭 → 모달에서 `TEST_PING / ECHOSVC / PING / active=true` 입력 → 저장 → 테이블에 즉시 표시.
3. **active 토글** — 룰의 Active 컬럼 토글 → 회색/원색 전환 + 토스트 성공.
4. **▶ 테스트 deeplink** — 룰 우측 `▶` 버튼 클릭 → API 테스터 페이지로 이동 + path/body 가 미리 채워짐.
5. **삭제** — 룰의 X 클릭 → confirm → 테이블에서 사라짐.
6. **감사 검증** — `감사 로그` 페이지 → `라우팅` 칩 → 위 액션들이 `PUT_ROUTE` / `SET_ROUTE_ACTIVE` / `DELETE_ROUTE` 로 기록.

---

## 3. 정책 엔진 (`page-policy`)

### 3.1 무엇을 하는가
- **Kill Switch** — 채널 단위 매매 차단 (login/admin 은 영향 없음).
- **정비 창** — 시작~끝 사이 transaction 차단.
- **차단 심볼** — `envelope.data.symbol` 매칭 (대문자 무관).
- **차단 routing key** — transaction 코드 차단 (예: `CANCEL`).

### 3.2 화면 요소
- 상단: `pol-updated` / `pol-by` / `↻`.
- Kill Switch 카드: 상태 + 토글 + 적용 범위 (전체/고객/직원/사용자 정의).
- 정비창 카드: 시작 / 끝 / 메시지 + 저장/비우기.
- 차단 심볼 카드: 입력 + 칩 목록.
- 차단 rkey 카드: 입력 + 칩 목록.

### 3.3 테스트 시나리오
1. **Kill Switch 전체** — `전체` 클릭 → `WEB,MOB,HTS,ADM,EMP` 자동 체크 → `토글` → 상태가 `ON` (bad) 으로.
2. **Kill Switch 고객만** — `고객만` 클릭 → `WEB/MOB/HTS` 만 체크 → 토글 → 상태 ON + `현재 적용: WEB,MOB,HTS`.
3. **검증** — `감사 로그` 의 `정책` 칩 → `POLICY_KILL_SWITCH attrs.channels=[WEB,MOB,HTS]`.
4. **정비창** — 시작/끝/메시지 입력 → 저장 → 상단 뱃지 `active/inactive` 자동 전환.
5. **정비창 비우기** — `비우기` → start/end 둘 다 zero → 저장 → 뱃지 `inactive`.
6. **차단 심볼** — `USDKRW` 입력 → `+ 추가` → 칩으로 표시 → 칩 우측 × 로 삭제.
7. **차단 rkey** — `CANCEL` 입력 → 동일 흐름.

---

## 4. 브로커 명령 (`page-broker`)

### 4.1 무엇을 하는가
mymqd admin 명령 (Status / Clients / Exchanges / Users / Whois) 직접 호출.
결과는 raw JSON.
	
### 4.2 테스트 시나리오
1. **Status** — broker 정보 + uptime + queue 카운터.
2. **Clients** — 현재 broker 에 연결된 client 목록 (mci-api, mci-push, ...).
3. **Exchanges** — registered exchange 목록.
4. **Users** — broker user table.
5. **Whois** — modal 입력 (xchg/rkey/qnam) → 매칭 client 검색.
6. **DevMode no-broker** 환경에서는 모두 503 또는 connection error — 의도된 동작.

---

## 5. API 테스터 (`page-tester`)

### 5.1 무엇을 하는가
mci-admin 의 어떤 endpoint 든 임의 호출. Postman 미니 버전.
같은 origin 이라 CORS 무영향.

### 5.2 화면 요소
- 사전 설정 (preset) chips — 자주 쓰는 alias 자동 채움.
- 요청 builder: Method (GET/POST/PUT/DELETE/PATCH) + Path + Channel + Body.
- 추가 헤더 (선택 펼침).
- 요청 panel: method/path/headers/body dump (자동 주입 헤더 X-WTG-User, Content-Type 포함).
- 응답 panel: status / time / size / body / headers + 📋 복사.
- 최근 요청 (20) — 클릭으로 재구성.

### 5.3 테스트 시나리오
1. **ping** — `GET /v1/ping` → 200 + `{ok:true}`.
2. **status** — preset 칩 `Status` → 실행 → broker status JSON.
3. **/v1/tx echo** — `POST /v1/tx` + body `{"alias":"WECHO_PING","data":""}` → broker 응답 또는 503 (DevMode no-broker).
4. **채널 spoof** — channel `WEB` 선택 후 정책 (Kill Switch WEB ON) 상태에서 호출 → 503 / 차단 응답.
5. **JSON 정렬** — body 에 minify JSON 붙여넣고 `정렬` → pretty 변환.
6. **히스토리** — 최근 요청 클릭 → 재구성됨. `히스토리 초기화` → 비워짐.
7. **헤더 추가** — `X-Test: hello` 추가 → 요청 panel 에 반영.

---

## 6. WebSocket 모니터 (`page-wsmon`)

### 6.1 무엇을 하는가
edge-push / edge-price / mci-push 등 ws endpoint 에 직접 연결해서
실시간 메시지 + 통계 (메시지 수 / B/sec / peak / drop / uptime) 관찰.

### 6.2 테스트 시나리오
1. **프리셋 선택** — `edge-price (8083)` → URL 자동 채움 (또는 `mci-push (8081)`).
2. **연결** — `▶ 연결` → 상태 dot 초록 + `connected`.
3. **시세 subscribe** (edge-price) — "메시지 전송" 펼침 → preset 칩 `+ subscribe USD/EUR/JPY KRW` 클릭 → **전송** → inline status `✓ 전송됨` → stream 에 시세 envelope 흘러옴 → B/sec / 메시지 카운터 증가.
4. **메시지 도착** — broker → 서버로 push 흐르면 로그에 누적, B/sec 증가.
5. **일시정지** — 체크 → 화면 freeze, peak 는 계속 갱신.
6. **자동 스크롤** — 해제 시 사용자 스크롤 유지.
7. **JSON 정렬** — 체크 시 메시지가 pretty.
8. **필터** — 텍스트 입력 → 매칭 메시지만 표시.
9. **메시지 전송 형식** — edge-price 는 `{"type":"subscribe","pairs":["USD/KRW",...]}` 또는 `{"type":"unsubscribe","pairs":[...]}` 만 받음. 다른 형식은 `bad_request` 응답. mci-push/edge-push 는 단방향 — client 메시지 무시.
10. **unsubscribe** — preset `− unsubscribe USD/KRW` → 전송 → 해당 통화쌍 메시지 중단.
11. **종료** — `■ 종료` → 상태 dot 회색.
12. **메시지 비우기** — log 영역 초기화 (통계는 유지).

### 6.3 트러블슈팅
| 증상 | 원인 / 처치 |
|------|------------|
| `종료 (code=1006 reason=-)` | edge-{price,push,chart} 미기동 또는 CheckOrigin 거부 — `wtgctl edge start` + DevMode 빌드 확인 |
| `401 X-WTG-User 헤더 필요` | edge 서버에 `UserFromQuery` 미들웨어 누락 — 최신 빌드 적용 |
| `✗ 메시지가 비어있음` | input 비어있음. preset 칩 클릭 또는 default value 사용 |
| 연결 OK but 시세 0 | edge-price 는 subscribe 안 하면 안 흐름. preset `+ subscribe ...` 클릭 후 전송 |

---

## 7. 시세 (`page-quote`)

### 7.1 무엇을 하는가
mci-push (`:8081`) ws 에 연결해서 quote-forwarder 가 publish 한 시세를
통화쌍별 호가창 + 최근 체결 50개로 시각화.

### 7.2 테스트 시나리오
1. **연결** — `▶ 연결` → 상태 dot 초록.
2. **시세 흘리기** — 다른 터미널에서:
   ```bash
   ./scripts/load-test.sh low    # 640 tick/s
   ```
3. **통화쌍 카드** — `USD/KRW`, `EUR/KRW` 등이 자동 grid 생성.
4. **호가 갱신** — bid/ask 가 색 점멸 (good/bad).
5. **tick/sec / 통화쌍 / 체결** 카운터 증가.
6. **초기화** — `초기화` 클릭 → grid 비워짐.
7. **종료** — `■ 종료`.

---

## 8. 통화쌍 (`page-symbols`)

### 8.1 무엇을 하는가
외부 심볼 `USDKRW` ↔ 내부 pair `USD/KRW` 매핑 CRUD. 비활성 토글 시
mci-price 가 즉시 해당 tick drop. etcd watch 로 전파.

### 8.2 테스트 시나리오
1. **목록 로드** — etcd 에 등록된 entry 표시 (DevMode 의 embedded etcd 자동 seed).
2. **신규 등록** — `+ 신규` → `USDKRW / USD/KRW / active=true` 저장 → 표시.
3. **active 토글** → 회색/원색.
4. **삭제** → 테이블에서 사라짐.
5. **감사** — `통화쌍` 칩 → `PUT_SYMBOL` / `DELETE_SYMBOL`.

---

## 9. 마진 테이블 (`page-pricing`)

### 9.1 무엇을 하는가
swap point / 본점 / 영업점·채널 마진을 담은 단일 JSON. 저장 시 모든
mci-price 가 atomic 으로 새 테이블로 전환. Version 은 수동 1씩 증가 (감사).

### 9.2 화면 요소
- 도구: ↻ 다시 읽기 / `{ } 포맷` / 💾 저장.
- 메타: 현재 Version / HQ entries / Site entries / Swap entries.
- editor: textarea (`pricing-editor`).
- 에러 표시줄.

### 9.3 테스트 시나리오
1. **다시 읽기** → editor 에 현재 JSON.
2. **포맷** → 들여쓰기 정렬 (offline JSON.parse + stringify).
3. **버전 올리기** — Version 을 `+1` 한 JSON → 저장 → 메타 갱신.
4. **invalid JSON** — 일부러 `}` 삭제 → 저장 → 에러 표시줄 활성 + 빨간 테두리.
5. **검증** — `감사 로그` 의 `마진` 칩 → `PUT_PRICING_TABLE attrs.version=...`.

---

## 10. 프로파일 (`page-profiles`)

### 10.1 무엇을 하는가
(Channel × Site × Tier) 활성 조합. PricingConsumer 가 각 프로파일별로
마진 적용된 시세를 별도 publish.

### 10.2 테스트 시나리오
1. **목록 로드** — 기존 entry (예: `WEB/HQ/VIP`) 표시.
2. **신규** — `Key` 자동/수동 + Channel/Site/Tier 선택 → 저장 → 즉시 등장.
3. **삭제** → 사라짐.
4. **감사** — `프로파일` 칩 → `PUT_PROFILE` / `DELETE_PROFILE`.

---

## 11. 사용자 프로파일 (`page-users`)

### 11.1 무엇을 하는가
`usid → (Site, Tier)` 권위 출처. 로그인 시 Session.Profile 에 박힘.
클라이언트가 body 로 보낸 값은 무시. 미등록 사용자는 raw 시세만 수신.

### 11.2 테스트 시나리오
1. **등록** — `alice / HQ / VIP` → 저장.
2. **edge-price 검증** — alice 가 로그인 후 ws 구독 → `WEB/HQ/VIP` 프로파일의 quote 수신.
3. **변경** → 동일 usid 로 다른 tier 저장 → 즉시 반영 (etcd watch).
4. **삭제** → 차후 로그인 시 raw 만 수신.
5. **감사** — `사용자` 칩 → `PUT_USER_PROFILE` / `DELETE_USER_PROFILE`.

---

## 12. QuoteID 엔진 (RBAC) (`page-quoteid-engines`)

### 12.1 무엇을 하는가
매칭 엔진/도구의 `engine_id` allowlist. permissions (read-only vs read+write)
+ 만료시각 (자동 회수) + contact (감사 추적). 변경 즉시 mci-price hot reload.

### 12.2 테스트 시나리오
1. **빈 상태** — 등록 없음 → "RBAC 비활성 (모든 caller 통과)" 안내.
2. **신규** — `engine_id=ME01`, permissions=`[read,write]`, `expires_at=2027-01-01`, `contact=ops@x.com`.
3. **만료 검증** — `expires_at` 을 과거로 → 운영자 (mci-price 호출) 차단.
4. **삭제** → 표에서 사라짐 + 다시 모든 caller 통과.
5. **감사** — `QuoteID` 칩 → `PUT_QUOTEID_ENGINE` / `DELETE_QUOTEID_ENGINE`.

---

## 13. 마진 재계산 (`page-margin`)

### 13.1 무엇을 하는가
quote_bars 의 raw 시세에 PricingTable 을 다시 적용해 "그 시점에 고객이
받았을 customer quote" 를 재구성. 분쟁 / 회귀 분석용. **read-only** —
실시간 publish 와 무관.

### 13.2 화면 요소
- 조건: From / To (UTC) / Pair / Channel / Site / Tier (다중 비교) / 샘플 수.
- table_override (선택) — 현재 etcd PricingTable 대신 임의 JSON 사용.
- 결과: 통계 (bid 평균/최대/최소, ask 평균/최대/최소) + 봉 view (close only / OHLC / 차트).

### 13.3 테스트 시나리오
1. **단일 profile** — `Pair=USD/KRW / Channel=WEB / Site=BRANCH / Tier=VIP` → 실행 → 통계 + table.
2. **다중 tier 비교** — VIP/GOLD/STD 체크 → 통계 행 3개 등장.
3. **차트 view** — `차트` 버튼 → raw mid (slate), customer bid (cyan), customer ask (amber) 3개 선.
4. **OHLC** — `OHLC 전체` → open/high/low/close 모두 표시.
5. **샘플 수** — `200` → 큰 결과셋, 페이지 끝까지 스크롤.
6. **override 모드** — 체크 → JSON 편집 영역 등장 → 임의 PricingTableDoc 입력 → 실행 → 결과가 그 테이블 기반.
7. **유효성** — From > To 또는 비어있음 → 검증 에러.

---

## 14. 서비스 명세 (`page-svcio`)

### 14.1 무엇을 하는가
매매 svc 의 input/output 구조 — 헤더 파일 (`win/src/inc/trn/*.h`) 자동 파싱.
모든 채널이 동일 metadata, 호출 코드만 채널별로 다르게 자동 생성.
dev 의 svc-headers 는 편집 가능, 운영 헤더는 read-only.

### 14.2 화면 요소
- 우상단: `📋 공통 헤더 (N)` 버튼 + `svcio-stats` 카운터.
- 좌측: 검색 + svc 목록.
- 우측 상세: 코드 / 한글명 / 공통 헤더 / Input / Output / 헤더 source 편집 + 호출 코드 (API/WIRE × 채널 5).

### 14.3 테스트 시나리오
1. **목록 로드** — 부팅 시 파싱된 svc 목록 표시.
2. **검색** — 코드 또는 한글 이름 입력 → 필터링.
3. **상세 표시** — svc 선택 → 우측 panel 채움. Input/Output 필드 트리.
4. **공통 헤더 modal** — 우상단 버튼 → COMHDR/BROADCAST_H 등 표시.
5. **API 코드 생성** — `API` 라디오 + `WEB` 탭 → fetch 호출 코드 보임.
6. **WIRE 코드** — `전문` 라디오 → wire frame 보기 + 채널 row 자동 hide.
7. **▶ wire 테스트** — exchange/rkey 입력 → 전송 → 응답 panel 에 raw bytes + parsed.
8. **헤더 편집** — dev svc-headers 의 svc 선택 → `📝 헤더 source 편집` 펼침 → 일부 필드 추가 → `💾 저장 + 재파싱` → 우측 spec 즉시 갱신, `.bak` 백업 생성. 운영 헤더는 `read-only` 라벨.
9. **감사** — `SvcIO` 칩 → `SVCIO_TEST_WIRE` / `SVCIO_SAVE_SOURCE`.

---

## 15. 감사 로그 (`page-audit`)

### 15.1 무엇을 하는가
모든 admin mutation 의 최신 200개 sliding window (in-memory ring).
운영 sink (7년 보관) 와 별개의 즉시 표시 + ad-hoc 분석.

### 15.2 화면 요소
- 도구: 텍스트 필터 / 자동 새로고침 체크 / ↻ / ⇩ CSV.
- **Resource 칩 필터** — 전체 / 라우팅 / 통화쌍 / 프로파일 / 사용자 / 마진 / QuoteID / 정책 / SvcIO.
- summary: 총 N / 표시 M.
- timeline: 항목 카드 (아이콘 + 라벨 + Resource 칩 + 액션 코드 + 시각 + by/usid + rid + attrs chips).

### 15.3 테스트 시나리오
1. **수집 확인** — 다른 페이지에서 mutation 발생 후 진입 → 즉시 표시 (ws push) 또는 3초 polling.
2. **Resource 칩** — `정책` 클릭 → policy 액션만 남음. `전체` → 복귀.
3. **텍스트 필터** — `alice` 입력 → usid=alice 만 표시.
4. **칩 + 텍스트 조합** — 둘 다 적용 → AND 필터.
5. **CSV export** — `⇩ CSV` → `audit-YYYY-MM-DDTHH-MM-SS.csv` download. Excel 에서 열어 한글 깨지지 않음 (BOM 포함).
6. **자동 새로고침** — 체크 해제 → polling 멈춤. ws stream 이 살아있으면 push 만으로 갱신.
7. **빈 상태** — 새 환경 → "아직 기록된 액션이 없습니다."
8. **필터 미일치** — 검색어 무매칭 → "필터 조건에 일치하는 항목 없음."

---

## 16. 운영 가이드 (`page-guide`)

### 16.1 무엇을 하는가
정적 reference — 운영자가 "이 값은 어디서 바뀌나 / 무엇을 보존하나 / 비상 시
무엇을 누르나" 를 한 화면에서 확인. `docs/operations.md` §A.7~A.11 의 발췌.

### 16.2 테스트 시나리오
1. **로딩** — 진입 즉시 표시 (동적 데이터 없음).
2. **표 가독성** — 각 표가 PC/모바일 모두 가로 overflow 없이 보임.
3. **링크** — 외부 docs 링크 (있다면) 정상 동작.

---

## 17. 통합 시나리오 — "한 바퀴" 스모크

E2E 흐름 검증용 — 운영 배포 직후 5분 안에 핵심 path 모두 확인.

| # | 액션 | 검증 |
|---|------|------|
| 1 | 대시보드 진입 | 4개 카드 채워짐, broker CONNECTED |
| 2 | 라우팅 룰 → `TEST_PING` 신규 → active=true | 테이블에 표시 |
| 3 | API 테스터 → `POST /v1/tx` `{alias:TEST_PING,data:""}` | broker 응답 200 또는 의미 있는 에러 |
| 4 | 통화쌍 → `USDKRW/USD/KRW` 신규 | 테이블 표시 |
| 5 | 마진 테이블 → 다시 읽기 → Version+1 → 저장 | 메타 갱신 |
| 6 | 프로파일 → `WEB/HQ/VIP` 신규 | 표시 |
| 7 | 사용자 → `alice/HQ/VIP` | 표시 |
| 8 | 시세 → 연결 → `./scripts/load-test.sh low` | tick 증가 |
| 9 | 정책 → Kill Switch 고객만 ON | API 테스터에서 채널=WEB 호출 → 503 / 차단 |
| 10 | 정책 → Kill Switch OFF | 다시 정상 |
| 11 | 감사 로그 → 위 모든 액션이 카테고리별 칩에 1+개씩 | OK |
| 12 | 감사 로그 → CSV export | 파일 download, Excel 열림 |

---

## 부록 A. 백엔드 endpoint 매핑 (UI ↔ HTTP)

| UI 페이지 | HTTP path |
|-----------|-----------|
| dashboard | `GET /v1/ping`, `GET /v1/admin/routes`, `GET /v1/admin/status` |
| routes | `GET/PUT/DELETE /v1/admin/routes[/{alias}]`, `POST /v1/admin/routes/{alias}/active` |
| policy | `GET /v1/admin/policy`, `POST /v1/admin/policy/{kill-switch,maintenance,blocked-symbols,blocked-routing-keys}` |
| broker | `POST /v1/admin/cmd`, `GET /v1/admin/{status,clients,exchanges,users,whois}` |
| symbols | `GET /v1/admin/symbols`, `PUT/DELETE /v1/admin/symbols/{symbol}` |
| pricing | `GET/PUT /v1/admin/pricing/table` |
| profiles | `GET /v1/admin/profiles`, `PUT/DELETE /v1/admin/profiles/{key}` |
| users | `GET /v1/admin/user-profiles`, `PUT/DELETE /v1/admin/user-profiles/{usid}` |
| quoteid-engines | `GET /v1/admin/quoteid-engines`, `PUT/DELETE /v1/admin/quoteid-engines/{engine_id}` |
| margin | `POST /v1/admin/margin/recompute` |
| svcio | `GET /v1/admin/svc-io/...`, `PUT /v1/admin/svc-io/{code}/source` |
| audit | `GET /v1/admin/audit?limit=N` |
| (stream) | `GET /v1/admin/stream` (ws) |

## 부록 B. 트러블슈팅

| 증상 | 원인 / 처치 |
|------|------------|
| 진입 후 모든 카드 `—` | broker 미기동 + `--no-broker` 안 줌 → DevMode no-broker 옵션 추가 또는 broker 기동 |
| audit 페이지 "로드 실패" | mci-admin 자체 다운 — `lsof -ti tcp:9090` 확인 |
| 신규 등록이 etcd 503 | DevMode `--dev` 가 embedded etcd 자동 기동하므로 이 경우 거의 없음. 운영은 `--etcd-endpoints` 확인 |
| 정책 토글 후 API 테스터에서 통과 | 채널 spoof (`X-WTG-Channel`) 가 정책 scope 와 다른지 확인 |
| 시세 페이지 tick 0 | mci-push ws URL 확인 + quote-forwarder/load-gen 가 실제로 publish 중인지 |
| CSV 한글 깨짐 | 우리는 BOM 포함 — Excel 이면 정상. macOS Numbers 는 import 시 UTF-8 명시 |
