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

## crypto (FC 0x02) — 미구현 (막힌 관문)

HTS 모드(`m_bHTSYN`, 현 빌드 INISAFENET 활성) 에서 select-server 다음에 옴.
`[len][01 00 02][RH 37B][INISAFE 암호화 블롭]` 송신 → 서버가 복호화·세션키 응답.

edge-tcp 는 이 프레임을 감지해 WARN 로그만 남기고 미처리 (연결 유지). 진행하려면:
- **옵션 A**: cs 쪽 HTS 모드(`m_bHTSYN`) 비활성 → 3단계 생략, 바로 sign-on.
- **옵션 B**: edge-tcp 가 INISAFENET v7.2 세션키 교환 구현 (무거움).

→ 운영/cs 팀과 A 가능 여부 합의가 선행. 합의 후 이 문서에 결정 기록.

## 전문 (FC 'A'/'B'/'C') — crypto 이후 (미구현)

crypto 통과 후 cs 가 보내는 실제 업무 프레임. TH(+RH) 를 벗기고 COMHDR(512B)+body
를 `/v1/tx` raw 모드로 forward 해야 함. 현재 edge-tcp 는 payload 를 그대로 전문으로
취급하므로, TH/RH 파싱을 crypto 합의 후 함께 구현.

## 참고

- FunctionCode 전체표: `NymphSocket.cpp:688-701`
- select-server: `MyMQChannelImpl.cpp:938-948`
- 형제 repo 동일 로직: `win-agg-fx-cs/src/MyMQChannel/MyMQChannelImpl.cpp:888-950`
