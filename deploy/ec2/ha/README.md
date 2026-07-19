# 시세 HA (다중 인스턴스) — 단일 인스턴스 → HA 전환

> 기본 배포는 **단일 인스턴스** (`deploy/ec2/wtg-quote-forwarder.service` grpc +
> `wtg-mci-price.service`). 이 디렉토리는 **다중 인스턴스 Active-Active** 로 갈 때만
> 사용하는 변형. 설계·검증은 `docs/price-ha-grpc.md`, `make price-ha-verify`.

## 토폴로지 차이

| | 단일 (기본) | HA (본 디렉토리) |
|---|---|---|
| forwarder | `--publish-mode=grpc --price-grpc=<mci-price>` (push, 1대) | `--publish-mode=hub --tick-listen=<hub>` (서버, fan-out) |
| mci-price | 1대, forwarder 가 PublishTick push | N대, 각자 `--tick-source=<hub>` dial-in 구독 |
| dedup | 불요 | 다중 forwarder 시 자동 (source,seq) |
| edge | 단일 upstream | gRPC round_robin (N 인스턴스) |

## 전환 절차

```bash
cd /home/winway/nh-fxallone-server/wtg/deploy/ec2

# 1. env 에 허브 주소 추가 (wtg.env)
echo 'WTG_TICK_HUB=127.0.0.1:50060' | sudo tee -a /home/winway/nh-fxallone-server/wtg/wtg.env

# 2. forwarder 를 hub 모드로 교체
sudo systemctl stop wtg-quote-forwarder
sudo cp ha/wtg-quote-forwarder-hub.service /etc/systemd/system/wtg-quote-forwarder.service

# 3. 단일 mci-price 유닛 정지, 다중 인스턴스 템플릿 설치
sudo systemctl disable --now wtg-mci-price
sudo cp ha/wtg-mci-price@.service /etc/systemd/system/

# 4. 활성화 (인스턴스 2대 예시 — 포트 8081/50051, 8082/50052)
sudo systemctl daemon-reload
sudo systemctl enable --now wtg-quote-forwarder wtg-mci-price@1 wtg-mci-price@2
```

## edge 라우팅 (round_robin)

`mci-edge-price` 의 upstream 을 N개 mci-price gRPC 로 → grpc `round_robin` + healthcheck.
mci-price 는 warm-up 중 `/v1/ready` 503 → LB/edge 가 skip (조용한 합류). 상세
`docs/mci-price-ha.md` §2.1.

## 검증

```bash
make price-ha-verify   # 로컬에서 1 hub → 2 mci-price 결정성+warm-up+failover
# 운영: 각 인스턴스 /v1/best-stats 가 동일 BEST, 하나 죽여도 나머지 지속.
```
