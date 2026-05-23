package mymq

// 이 파일은 WTG (Winway Trading Gateway) 의 명명 컨벤션을 코드 상수로 고정한다.
//
// 운영팀과 합의된 디폴트값이며, 환경별로 달라져야 하는 부분(Exchange 이름 등)은
// 설정 파일로 덮어쓸 수 있도록 추후 cmd/<service>/config.go 에서 노출한다.

// ─── ApplName 컨벤션 ────────────────────────────────────────────────────────
//
// DECLARE_SESSION 의 appl_name 필드 (최대 16바이트). broker 의 whois 와
// 모니터링에 그대로 노출되므로 사람이 읽을 수 있게 짧고 명확하게 짓는다.
//
// 형식:
//   <service>           단일 인스턴스
//   <service>-<NN>      다중 인스턴스 (NN: 01..99 권장)
//
// 호스트/PID 는 broker 가 connect IP 로 알 수 있으므로 ApplName 에 포함하지 않는다.

const (
	ApplMciAPI    = "mci-api"
	ApplMciPush   = "mci-push"
	ApplMciPrice  = "mci-price"
	ApplMciAdmin  = "mci-admin"
	ApplMciEAPI   = "mci-eapi"  // mci-edge-api (DMZ)
	ApplMciEPush  = "mci-epush" // mci-edge-push (DMZ)
	ApplMciEPrice = "mci-epric" // mci-edge-price (DMZ)
)

// ─── Channel 코드 (mqhdr.chan[4]) ───────────────────────────────────────────
//
// 메시지 헤더의 chan 필드(4바이트)에 들어가는 사용자 단말 식별자.
// 정확히 4바이트 (부족분 space padding) 로 인코딩된다. 감사 로그와 라우팅
// 정책에서 "어느 채널에서 온 사용자인지" 구분하는 용도.
//
// 신규 채널은 이곳에 상수 추가 후 Channel.Bytes() 헬퍼로 변환해서 사용.

type ChannelCode string

// 채널 5종 — 호출 출처를 구분. URL prefix / 인증 헤더 / DMZ vs Internal /
// 권한 등급 / 표준 client 가 채널별로 다르게 결정. svc I/O gen 이 이 값으로 분기.
//
// 두 가지 axis 가 직교한다:
//
//	            │ 외부 (고객권)            │ 내부 (직원권)
//	────────────┼──────────────────────────┼────────────────────────
//	web/native  │ WEB (브라우저)            │ ADM (운영 콘솔)
//	            │ MOB (모바일 앱)           │
//	cs-native   │ HTS (고객 desktop client) │ EMP (딜러 desktop client)
//
// HTS / EMP 는 같은 cs framework (전통 native desktop) 위에 있지만 사용자가
// 정반대 — HTS 는 고객, EMP 는 딜러/직원. 권한 등급이 다르고 노출 위치도 다르다
// (HTS 외부 / EMP 내부) 그래서 별도 채널로 다룬다. `IsCSFramework()` 로 기술
// 동질성 그룹은 묶을 수 있다 (rate limit 정책 등).
const (
	ChannelWeb    ChannelCode = "WEB" // 외부 — 웹 브라우저 (고객)
	ChannelMobile ChannelCode = "MOB" // 외부 — 모바일 앱 (고객)
	ChannelHTS    ChannelCode = "HTS" // 외부 — 고객용 cs native desktop client (Home Trading System)
	ChannelAdmin  ChannelCode = "ADM" // 내부 — 직원 운영 콘솔 (mci-admin)
	ChannelEMP    ChannelCode = "EMP" // 내부 — 딜러/직원용 cs native desktop client
	ChannelFix    ChannelCode = "FIX" // 외부 FIX 카운터파티 (향후)
	ChannelAPI    ChannelCode = "API" // 외부 REST API 통합 (향후)
	ChannelBot    ChannelCode = "BOT" // 자동매매 봇 (향후)
)

// IsCSFramework — 전통 cs (native desktop) 기술 framework 그룹. HTS / EMP.
// rate limit / wire format 등 *기술 차원* 정책을 같이 적용하고 싶을 때 사용.
// 권한 등급은 채널별로 다르게 적용해야 함 — 묶지 말 것.
func (c ChannelCode) IsCSFramework() bool {
	return c == ChannelHTS || c == ChannelEMP
}

// IsCustomer — 고객 채널 (WEB / MOB / HTS). 직원 (ADM / EMP) 와 정책 적용
// 범위가 다른 경우 (예: kill switch 시 고객만 차단, 딜러는 통과) 사용.
func (c ChannelCode) IsCustomer() bool {
	return c == ChannelWeb || c == ChannelMobile || c == ChannelHTS
}

// IsEmployee — 직원 채널 (ADM / EMP). 운영 권한 또는 딜러 권한.
func (c ChannelCode) IsEmployee() bool {
	return c == ChannelAdmin || c == ChannelEMP
}

// Bytes 는 ChannelCode 를 mqhdr.chan[4] 크기의 4바이트 (right-pad with space)
// 로 변환한다. 4바이트 초과 시 잘려나간다.
func (c ChannelCode) Bytes() [4]byte {
	var b [4]byte
	for i := range b {
		b[i] = ' '
	}
	for i := 0; i < len(c) && i < 4; i++ {
		b[i] = c[i]
	}
	return b
}

// ─── Exchange / RoutingKey 카탈로그 ─────────────────────────────────────────
//
// 비즈니스 도메인별 표준 exchange. mymqd.cfg 에 동일 이름으로 선언되어야
// 한다. 운영팀 합의 후 변경 가능.

const (
	ExchangeOrder  = "ORDER"  // DIRECT — 주문 트랜잭션
	ExchangeExec   = "EXEC"   // FANOUT — 체결/주문상태
	ExchangePrice  = "PRICE"  // FANOUT — raw FX 시세 (cooker/quote-forwarder → mci-price)
	ExchangeQuote  = "QUOTE"  // TOPIC  — Profile 별 마진 적용된 고객 시세 (mci-price → edge-price)
	ExchangeAlert  = "ALERT"  // DIRECT — 시스템/리스크 알림
	ExchangeSignal = "SIGNAL" // FANOUT — 시그널 메시지
	ExchangeAdmin  = "ADMIN"  // DIRECT — 관리 명령
	ExchangeAudit  = "AUDIT"  // FANOUT — 감사 로그
)

// 주문(ORDER) routing key — DIRECT exchange, 정확 매칭.
const (
	RKeyOrderNew    = "NEW"    // 신규 주문
	RKeyOrderCancel = "CANCEL" // 취소
	RKeyOrderModify = "MODIFY" // 정정
	RKeyOrderQuery  = "QUERY"  // 조회
)

// 어드민(ADMIN) routing key.
const (
	RKeyAdminStatus   = "STATUS"   // 브로커 상태
	RKeyAdminReload   = "RELOAD"   // 정책 리로드
	RKeyAdminShutdown = "SHUTDOWN" // 그레이스풀 셧다운
)

// EXEC, PRICE, SIGNAL, AUDIT 는 FANOUT 모드라 routing key 를 사용하지 않는다.
// (필요하면 FrameInput.Rkey 비워두기)

// ─── Quote (시세) routing key ───────────────────────────────────────────────
//
// 시세는 두 단계로 흐른다:
//
//   1. raw   : cooker / quote-forwarder → ExchangePrice (FANOUT) → mci-price
//              모든 raw tick 이 동일하게 배포된다. routing-key 미사용.
//   2. quote : mci-price → ExchangeQuote (TOPIC) → mci-edge-price → 고객 ws
//              같은 raw tick 이라도 Profile (Channel.Site.Tier) 별로 마진을
//              다르게 적용한 고객 시세가 별도 publish 된다.
//
// ExchangeQuote 의 routing-key 컨벤션:
//
//   routing_key = "<Channel>.<Site>.<Tier>"     // = session.Profile.Key()
//                 ↑ 모두 string-enum, 점(.) 구분, ≤16 바이트 (LRkey)
//
//   예) "WEB.BRANCH.VIP"   "MOB.HQ.STD"   "FIX.HQ.GOLD"
//
// 통화쌍(pair)은 routing-key 에 포함하지 않는다 — 메시지 본문에서 식별하고
// edge 가 세션별로 분기. 이유는 두 가지:
//   (1) LRkey=16바이트 한도 안에 Profile + pair 까지 넣기 어려움
//   (2) Profile 단위로 ws 라우팅하면 edge 가 받는 stream 이 자연히 통화쌍별로
//       conflation 가능 (mci-price 의 Conflation 과 동일 패턴).
//
// 구독자(edge-price)는 자기 담당 Profile 의 정확한 routing-key 로 subscribe.

// RKeyQuote 는 Profile key (session.Profile.Key() 의 결과) 를 ExchangeQuote 의
// routing-key 로 그대로 사용한다. 본 함수는 식별성을 위해 노출만 한다 (no-op).
//
// 호출자 책임:
//   - profileKey 는 ≤ LRkey(16) 바이트여야 한다.
//   - 비어있으면 broker 는 메시지를 처리할 수 없다.
func RKeyQuote(profileKey string) string {
	return profileKey
}

// ─── Queue 이름 ─────────────────────────────────────────────────────────────
//
// mymqd.cfg 에서 선언되는 큐 이름. 서비스가 unsolicited 모드로 connect 할 때
// 자동으로 이 큐에 바인드된다.

const (
	QueueMciAPI   = "mci_api"
	QueueMciPush  = "mci_push"
	QueueMciPrice = "mci_price"
	QueueMciAdmin = "mci_admin"
)
