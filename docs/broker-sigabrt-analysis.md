# mymqd broker SIGABRT 진단 — publish.c 분석 + 후속 진단 절차

부하 (load-gen rate=100 × 4 pair × 4 feed = ~1600 msg/s) 시 mymqd C broker
가 약 5초 내 silent SIGABRT (ExitCode 134) 로 종료되는 현상의 코드 분석 + 후속
정확 진단 절차.

## 1. 정황

| 측정 | 값 |
|------|----|
| broker | docker container `wtg-mymqd:latest` |
| trigger | quote-forwarder publish (mode=broker) — UDP 시세 → broker PRICE FANOUT |
| ExitCode | **134** = 128 + 6 = **SIGABRT** |
| OOMKilled | `false` — 메모리 부족 아님 |
| broker stderr (docker logs) | 비어있음 — glibc/stack-smash 자동 메시지도 없음 |
| broker file log (`mymqd-0.log`) | `Publish broadcasting message : #N to 1f` 직후 abrupt 종료. fatal log 없음 |
| 부하 강도 | 약 1600 msg/s 5초 누적 → ~8000 publish |
| 발생 일관성 | 100% reproducible — broker mode 부하 시작 후 수 초 내 |

## 2. 임시 회피 (현재 운영 상태)

`94a78b2` ~ `e8f67a4` 의 broker 우회 path:

- **forwarder** `--publish-mode=grpc` (default) — broker 우회로 mci-price 에 직접 push
- **mci-price** `--quote-publish-broker=false` (default) — customer quote 도 grpc only
- 결과: broker 가 받는 시세 부하 **0**. SIGABRT trigger 사라짐.
- 현 운영: broker 는 매매 RPC (mci-api ↔ test_service/WECHO) 만. 부하 낮음.

→ **현재 critical 아님**. 본 진단은 미래 safety net + 운영 backup path 의 안정성.

## 3. publish.c 정독 결과

`/Users/winwaysystems/mywork/mymq/src/mqd/publish.c` (241 lines).

### 3.1 구조

```
publish_packet(client, pktbuf, pktlen)        ← packet_proc 가 호출
  │
  ├─ 1. validation (size, broadcast header)
  ├─ 2. packet_alloc(_pktbuf) + memcpy 원본 → 복사
  ├─ 3. pubmsg 채움 (user/exchange/chan/logon_id 매칭 정보)
  ├─ 4. publish_q lock + enqueue (MAX_PUBLISH_Q=40)
  │
  └─ (별 thread)
     publisher() ← pthread_create 로 boot 시 시작
       │
       ├─ publish_q.cond_wait
       ├─ pubmsg dequeue
       ├─ for ii in clientmap->many:        ← 무뮤텍스 iter
       │     - client 검사 (sock, flag, chck, q_parms.qptr)
       │     - exchange/user/chan 매칭
       │     - iofunc[client->ioix].send(client, pktbuf)
       └─ packet_free(pktbuf)
```

### 3.2 의심 후보 (SIGABRT 가능 위치)

| # | 위치 | 시나리오 | 영향 |
|---|------|---------|------|
| **A** | publisher thread 의 `clientmap` iter (line 178~234) | 다른 thread 가 client_alloc / client_close 중. `memset(client, 0, ...)` (client.c:300) 와 동시 진행 | client 필드가 garbage. `client->ioix` out-of-bounds → `iofunc[]` array overflow → 다른 함수 pointer 호출 → 임의 abort |
| **B** | `MAX_PUBLISH_Q = 40` (line 10) | 부하 시 publish_q 가득 → `cond_timedwait` 3초 block. broker reader thread 가 block 되면 TCP buffer 누적 | 직접 SIGABRT 아니지만 메모리 / heap 압박 트리거 |
| **C** | `iofunc[client->ioix].send(...)` (line 232) | client_close 와 race — `client->mqio.pktbuf=NULL` set 후 send 가 access | NULL deref → 보통 SIGSEGV (139). SIGABRT 아님 |
| **D** | `packet_free(pktbuf)` (line 237) | publisher 가 처리 끝나고 free. 한편 동일 pktbuf 가 다른 path 에서 참조 중일 수 있음 | double-free 또는 use-after-free → glibc abort (134) |
| **E** | `strcasecmp(client->xchg, pubmsg.exchange)` (line 226) | `client->xchg` 의 메모리가 client_alloc 시점에 memset 되었거나 새 string 으로 reset 중 | strcasecmp 가 invalid pointer 읽으면 SIGSEGV. SIGABRT 아님 |

### 3.3 가장 강한 후보 — A (clientmap race) + D (double-free)

**메커니즘**:
1. publisher thread 가 `clientmap->client[ii]` 의 `q_parms.qptr` 보고 통과
2. 그 사이 다른 thread 가 client_close 호출:
   - `queue_remove(client)` (client.c:660) — q_parms 정리
   - `packet_free(client->mqio.pktbuf)` (client.c:662)
3. publisher 가 line 232 의 `iofunc[client->ioix].send(...)` 호출
4. send 내부에서 `client->q_parms.qptr` 의 freed memory 접근
5. 또는 send 가 별도 send_q 에 packet push — 그 send_q 가 freed → glibc heap corruption 검출 → `abort()`

glibc 의 검출 메시지가 stderr 로 안 가는 이유:
- docker container 의 stderr 가 redirect 되어 있거나
- glibc 가 빠른 abort 호출로 buffer flush 전 process 종료
- 또는 `MALLOC_CHECK_=0` 으로 silent 모드

### 3.4 보조 의심 — MAX_PUBLISH_Q

`MAX_PUBLISH_Q=40` 은 매우 작음. 부하 1600 msg/s + publisher thread single → 40 슬롯이 ms 단위로 채워짐. 채워지면:

```c
while (publish_q.free <= 0)
    cond_timedwait(..., 3sec);
```

reader thread 가 3초 block — TCP 수신 멈춤 → kernel buffer 누적. 무한 누적은 아니지만 broker 의 다른 시점에 메모리 압박 트리거.

직접 abort 원인은 아니지만 race 확률 증가시킴.

## 4. 후속 정확 진단 절차

### 4.1 ASAN 빌드 (권장 우선)

mymq C 소스 빌드 시 AddressSanitizer 활성:

```bash
# /Users/winwaysystems/mywork/mymq/Makefile 또는 환경변수
CFLAGS="-fsanitize=address -fno-omit-frame-pointer -g -O1"
LDFLAGS="-fsanitize=address"
make clean && make
```

빌드 후 mymqd 실행 → 부하 reproduce → 첫 illegal access 시 stderr 에 **정확한 backtrace** + memory state 출력. ~10% 성능 저하 있지만 진단 단계에선 무관.

### 4.2 core dump + gdb

container 에 core 허용:

```yaml
# docker run 옵션
--cap-add=SYS_PTRACE
--ulimit core=-1
-v /tmp/cores:/cores
# inside container
ulimit -c unlimited
# /etc/sysctl: kernel.core_pattern = /cores/core.%e.%p
```

SIGABRT 시 core dump → host 의 `/tmp/cores/` 에 저장. `gdb mymqd /tmp/cores/core.XXX` → `bt full` → 함수 / line 정확히.

### 4.3 lock 추가 patch (CLAUDE.md "C 엔진 무수정" 원칙 위반)

clientmap 의 read/write 보호:

```c
// client.h 또는 mymqd.h 에 RWLock 선언
pthread_rwlock_t clientmap_lock = PTHREAD_RWLOCK_INITIALIZER;

// publish.c publisher 의 iter 전:
pthread_rwlock_rdlock(&clientmap_lock);
for (ii = 0; ii < clientmap->many; ii++) { ... }
pthread_rwlock_unlock(&clientmap_lock);

// client.c client_alloc / client_close 의 critical section:
pthread_rwlock_wrlock(&clientmap_lock);
// ... memset(client, 0, ...) / sock 변경
pthread_rwlock_unlock(&clientmap_lock);
```

추가로 `MAX_PUBLISH_Q` 를 40 → 1024 또는 4096 으로 — 부하 흡수.

### 4.4 broker 자체 운영 권장

mymqreboot 의 `--rm` 제거 (이미 patch 됨) — 컨테이너 죽어도 evidence 보존.
추가 권장:
- `--restart=unless-stopped` — broker 죽으면 Docker 가 자동 재기동
- core dump 저장 경로 마운트
- log volume 영속화 (이미 `-v $MYMQ_LOG:/opt/mymq/log`)

## 5. 다른 의심 path (덜 가능)

| path | 검증 |
|------|------|
| `message.c` 의 packet_transfer | publish 와 별개 path, 매매 transaction 만 — 시세 부하와 무관 |
| `dispatch.c` 의 routing | 같은 thread 안 — race 적음 |
| `mmapq.c` / `shmq.c` | shared memory queue — broker 가 사용 안 하면 무관 |

## 6. 결론

| 항목 | 결과 |
|------|------|
| 정확한 SIGABRT 위치 | **A (clientmap race) 또는 D (double-free) 가능성 높음** — 코드 분석만으론 확정 불가 |
| 다음 단계 | **ASAN 빌드 + reproduce 가 최단 path** (3~5시간) |
| 운영 우선순위 | 현재 critical 아님 — broker 가 매매 RPC 만 처리 (부하 매우 낮음) |
| 장기 fix | Go mci-broker 로 마이그레이션 (Phase 2a, 1~2주) — SIGABRT 위험 영구 제거 |

## 7. 참고

- `/Users/winwaysystems/mywork/mymq/src/mqd/publish.c` — 본 분석 대상
- `/Users/winwaysystems/mywork/mymq/src/mqd/client.c` — clientmap 관리
- `/Users/winwaysystems/mywork/mymq/src/mqd/packet.c` — packet_alloc/free
- `/Users/winwaysystems/mymq/log/mymqd-*.log` — broker 자체 log (host bind-mount)
- `94a78b2` `e8f67a4` — broker 우회 path commit (현 회피)
- CLAUDE.md "C 엔진 무수정" 원칙 — 4.3 옵션은 단기 patch 만 의미. 장기는 Go mci-broker.
