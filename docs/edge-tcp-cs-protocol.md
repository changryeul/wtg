# mci-edge-tcp ↔ cs 클라이언트 프로토콜

레거시 cs(win-*-cs 의 MyMQChannel) 가 raw TCP 로 mci-edge-tcp(5021) 에 붙을 때의
wire 프로토콜. 실 캡처(tcpdump) + cs 소스(MyMQChannelImpl.cpp / NymphSocket.cpp)로 검증.

## 프레이밍

```
[4B big-endian length][payload]
```

- length 는 payload 바이트 수 (edge-tcp readFrame 과 동일 — 프레이밍은 호환).
- payload 앞 3B = **TH (transport header)**: `[flags][?][FunctionCode]`
  - flags: BPI(0x04)+EPI(0x08)=0x0c → 단일 완결 패킷, RHI 없음(RH 미첨부).
  - FunctionCode (TH[2]) 가 프레임 종류를 결정.

## Connect() 시퀀스 (MyMQChannelImpl.cpp 검증)

cs 는 TCP 연결 후 아래 순서로 진행. 각 단계가 통과해야 다음으로 감:

1. **TCP connect**
2. **select-server (FC 0x01)** — 무조건 최초. LB 라우팅.
3. **crypto 키교환 (FC 0x02)** — `m_bHTSYN`(HTS 모드) 일 때만. INISAFENET 세션키.
4. RecvThread/WorkThread 기동 + HeartBeat 30초 타이머
5. sign-on (FC 'A') → 로그온 쿠키 (FC 'B') → 일반 전문 (FC 'C')

## select-server (FC 0x01) — 구현됨

요청 (cs → edge-tcp):
```
00 00 00 03   4B length = 3
0c 00 01      TH: flags=0x0c, FC=0x01
```

응답 (edge-tcp → cs), 총 51B payload:
```
[TH echo 3B = 0c 00 01][ip1 24B ASCII null-term][ip2 24B ASCII null-term]
```

- cs 는 `strcmp(ip1, ip2)` 만 본다:
  - `ip1 == ip2` → 현 소켓 유지, 다음 단계 진행 ✅
  - `ip1 != ip2` → 현 연결 끊고 ip2 로 재접속 (LB 리다이렉트)
- edge-tcp 는 **두 필드에 동일 IP** 를 넣어 재접속 없이 진행시킨다.
  IP 값 자체는 cs 가 검증 안 함 (비교만) — `--select-server-ip`, 빈값이면 conn LocalAddr.

구현: `internal/edge/tcp/server.go` `replySelectServer`. TH[2]==0x01 & len==3 감지.

## crypto (FC 0x02) — HTSYN=N 으로 우회 (소스 확정)

MyMQChannelImpl.cpp 분석 결과, INISAFENET 빌드에서 crypto 는 **`if(m_bHTSYN)`**
(MyMQChannelImpl.cpp:1133) 로 가드된다. `m_bHTSYN` 은 config 파일
**`[Application] HTSYN`** 값이 `"Y"` 일 때만 TRUE (:516-518, 기본 FALSE).

→ **cs 를 `HTSYN=N` (또는 미설정) 으로 두면 FC 0x02 를 아예 안 보내고 바로
login 으로 간다.** edge-tcp 는 crypto 구현 불필요 (INISAFE 라이브러리 회피).

주의: 빌드 매크로가 `USE_INISAFENET` 이 아니라 `USE_HEADER_OPENSSL` 이면 crypto 가
무조건(HTSYN 무관) 실행된다 (MyMQChannelImpl.h:13-20). cs 빌드가 INISAFENET
(또는 Xecure 비-'H' userType) 인지 확인 필요. edge-tcp 는 FC 0x02 수신 시 WARN
로그만 남기므로, 로그에 crypto 가 찍히면 HTSYN 또는 빌드 확인.

## 전문 (FC 'A' sign-on / 'B' cookie / 'C' 일반) — 구현됨

프레임: `[4B len][TH 3B][RH 37B (RHI set 시)][COMHDR 512B + body]`.
- **TH flags** (TH[0], NymphSocket.h:155): RHI 0x01 / CPI 0x02 / BPI 0x04 /
  EPI 0x08 / ZIP 0x10 / ERR 0x20 / ENC 0x40. FC = TH[2].
- **RH 37B** (MyMqHeader.h): rhFlag(1)+winKey(4)+seqNo(4)+svcCode(4)+
  exchCode(8)+**routingKey(16)**. RHI 비트가 set 일 때만 첨부. 전문(FC 'C')은 항상 set.
- length prefix 는 **자신 제외** (= TH+RH+body 크기).

edge-tcp 처리 (server.go handleConn):
- payload[0] < 0x20 (제어값=flags) → cs TH-framed. `hdrLen = 3 + (RHI ? 37 : 0)`
  만큼 벗겨 순수 COMHDR body 를 `/v1/tx` raw 로 forward (trxc = body[:16]).
- payload[0] ≥ 0x20 (ASCII trxc) → raw COMHDR (tcp-tester 등 TH 없는 경로), 그대로.
- **응답 재프레이밍**: cs TH-framed 요청은 응답도 `[TH][RH] echo + engine output`
  으로 감싸 회신 (cs recv 파서가 RH.routingKey 로 요청-응답 매칭).
- **cookie (FC 'B')** 는 응답 불필요 (MyMQChannelImpl.cpp:785 — cs 가 안 기다림).
  서버는 usid accept + 로그만.

login 흐름 (MyMQChannelImpl.cpp): FC 'C' transaction (routingKey `SCBS0000Q01`)
또는 SSO → 정상 tx 응답 (ReplyCode "00000") → cs 가 FC 'B' cookie emit → 이후 전문.

## 참고

- FunctionCode 전체표: `NymphSocket.cpp:688-701`
- select-server: `MyMQChannelImpl.cpp:938-948`
- 형제 repo 동일 로직: `win-agg-fx-cs/src/MyMQChannel/MyMQChannelImpl.cpp:888-950`
