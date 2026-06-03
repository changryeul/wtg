# dev_main 운영 가이드

`win/src/lib/db2stub/dev_main.c` — 운영 trn 서비스 (W1100 / W3100 / WECHOSTD
등) 의 공통 main wrapper. `wtg-mymqd` 컨테이너 DB-free 모드에서 svcmain 우회.

## 1. 위치 + 빌드

```
win/src/lib/db2stub/dev_main.c      소스
win/src/lib/db2stub/Makefile.standalone   빌드 룰
```

`make -f Makefile.standalone all` → `dev_main.o` 생성. 운영 trn 의 standalone
Makefile 이 `biz_main_standalone.o` 대신 `dev_main.o` 와 link → 운영 svcmain
(SHM/cfg 의존) 우회.

## 2. 실행 인자

```
./W1100 -h 127.0.0.1:11217 -e WTRN -n W1100
```

| 인자 | 의미 | default |
|------|------|---------|
| `-h host:port` | broker endpoint | `127.0.0.1:11217` |
| `-e exchange` | bind exchange | `ECHOSVC` |
| `-n queue` | 자체 큐 이름 | argv[0] basename |

## 3. 운영 관측 기능

### 3.1 LOG_LEVEL — `DEV_MAIN_LOG`

| 레벨 | 동작 | 운영 권장 |
|------|------|-----------|
| `debug` | 매 frame raw hex_dump (16-byte 폭) | dev 만 |
| `info` (default) | `evt=recv / evt=reply` structured log | 운영 |
| `warn` | error / no_handler / sndb_nearfull 만 | 부하 시 |
| `error` | error 만 | (사실상 silent) |

```bash
DEV_MAIN_LOG=info ./W1100 ...
DEV_MAIN_LOG=debug ./W1100 ...   # raw frame 분석 필요 시
```

### 3.2 structured log 포맷

```
[dev_main] evt=recv rkey=[W1101S01] len=120 trcid=abc123...
[dev_main] evt=reply rkey=[W1101S01] trcid=abc123... ret=0 lat_us=145 sndl=64
[dev_main] evt=no_handler rkey=[BAD_KEY] trcid=...
[dev_main] evt=sndb_nearfull rkey=[W3100] trcid=... sndl=262128 sbsz=262144
```

- `lat_us`: procedure 처리 시간 (CLOCK_MONOTONIC)
- `ret`: procedure 반환값 (0 = 성공)
- `sndl/sbsz`: 응답 버퍼 사용량

`grep "evt=reply"` 로 latency 분포, `grep "evt=no_handler"` 로 잘못된 routing
key 추적.

### 3.3 per-rkey 통계 — `SIGUSR1`

```bash
kill -USR1 $(pgrep W1100)
```

stderr 출력:

```
[dev_main] === stats dump pid=12345 ===
[dev_main]   rkey=W1101S01     count=1023 err=2 avg_us=187 max_us=4521
[dev_main]   rkey=W1101S02     count=445  err=0 avg_us=92  max_us=3210
[dev_main]   no_handler count=0
```

- 누적값 (프로세스 시작 이후)
- `err_count` = procedure ret != 0 횟수
- `no_handler` = bind 된 rkey 인데 callme[] 의 strcmp 매칭 실패 (broker rkey
  변환 anomaly 의심 — 일반적으로 0)

### 3.4 crash handler

`SIGSEGV / SIGABRT / SIGBUS / SIGFPE` 발생 시 마지막 처리 컨텍스트를 stderr 에
남기고 default 동작 (core dump) 으로 재발생:

```
[dev_main] FATAL sig=11 in_procedure=1 rkey=[W1101S01] trcid=abc123... pid=12345
```

- `in_procedure=1` — procedure 안에서 죽음 (스택 trace 의 top 이 핸들러)
- `in_procedure=0` — main loop 의 idle / recv 또는 외부 SIGSEGV
- `rkey/trcid` — 직전 처리 중이던 컨텍스트

core 분석:

```bash
ulimit -c unlimited   # 컨테이너 entrypoint 에서 설정
gdb /opt/mymq/bin/W1100 /tmp/core.NNN
(gdb) bt
```

### 3.5 SIGPIPE 무시

broker 단절 시 `mymq_send / mymq_reply` 가 받는 SIGPIPE → main loop 종료 방지.
errno 로 처리됨 — log 에 `evt=reply ret=-1` 로 표시.

## 4. 운영 시나리오

### 4.1 평상시

```bash
DEV_MAIN_LOG=info /opt/mymq/bin/W1100 -h $BROKER -e WTRN -n W1100 \
  >> /opt/mymq/log/W1100.log 2>&1 &
```

stderr 를 log 파일로 redirect. structured log 만 흐름.

### 4.2 부하 시 — log level 조정

```bash
DEV_MAIN_LOG=warn /opt/mymq/bin/W1100 ...    # info evt log off
```

또는 runtime 변경:
- log level runtime 변경은 미지원 — 재기동 필요
- 대신 SIGUSR1 으로 누적 통계 dump (log 없이도 운영자에게 visible)

### 4.3 troubleshoot — 특정 호출의 wire 분석

```bash
DEV_MAIN_LOG=debug ./W1100 ...   # raw frame hex_dump 켜짐
```

`evt=recv` 다음에 16-byte 폭 hex+ASCII dump. mqhdr 100B (offset 0..99) +
body 분석.

### 4.4 정기 통계 수집

```bash
# 5분마다 SIGUSR1 → log 에 누적 stats 라인 추가
*/5 * * * * pkill -USR1 -f W1100 2>/dev/null
```

`grep "stats dump"` 또는 `grep "rkey=W1101S01.*count="` 로 시계열 추출.

## 5. 신규 trn 적용

운영 svcmain 사용 trn 을 dev_main 으로 전환:

1. trn 의 Makefile.standalone 작성 (WECHO/WECHOSTD/W1100 참고)
2. link 대상: `biz_main_standalone.o` 대신 `dev_main.o`
3. callme[] 그대로 사용 — `extern CALLME callme[]` 호환

`win/src/trn/W1100/Makefile.standalone` 이 reference 구현 — DB2 stub +
dev_main + libcom_min 으로 운영 svcmain 우회.

## 6. 한계 + 후속

| 한계 | 후속 |
|------|------|
| log level 동적 변경 X | SIGUSR2 → level rotate 가능성 |
| 통계는 stderr 만 | `/tmp/<APPL>.stats` 파일 또는 broker 로 publish (옵션) |
| panic 시 단일 procedure abort X | `sigsetjmp` 패턴 (복잡 — 현재는 default 동작 = 종료) |
| in_procedure 1 인 핸들러의 다른 thread crash | dev_main 은 single-thread 라 무관 |

## 7. 검증 — A 트랙 e2e

```bash
docker exec -d mymqd sh -c \
  'DEV_MAIN_LOG=info /opt/mymq/bin/W1100 -h 127.0.0.1:11217 -e WTRN -n W1100 > /tmp/w1100.log 2>&1'

# 6회 call
for i in 1 2 3 4 5 6; do
  docker exec mymqd /opt/mymq/bin/test_client \
    -h 127.0.0.1:11217 -n W1T$i -e WTRN -r W1101S01 -m call "MSG_$i"
done

# stats dump
docker exec mymqd sh -c 'kill -USR1 $(pgrep -f W1100)'
docker exec mymqd cat /tmp/w1100.log | tail -3
# [dev_main] === stats dump pid=NN ===
# [dev_main]   rkey=W1101S01 count=6 err=0 avg_us=162 max_us=844
# [dev_main]   no_handler count=0

# crash handler 검증 (외부 SIGSEGV)
docker exec mymqd sh -c 'kill -SEGV $(pgrep -f W1100)'
docker exec mymqd cat /tmp/w1100.log | tail -1
# [dev_main] FATAL sig=11 in_procedure=0 rkey=[W1101S01] trcid=- pid=NN
```

자세히는 `docs/broker-tracing.md` §10 (e2e 검증).
