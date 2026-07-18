package fxsync

import "context"

// Backend — 마스터 데이터 source 추상화.
//
// 운영: OracleBackend (TB_FXB_* 직접 SELECT).
// dev / 테스트: FileBackend (JSON 파일에서 read).
//
// 모든 Load* 메소드는 idempotent — 같은 input 에 같은 결과. 부분 failure 시
// non-nil err + partial 결과 가능 — 호출자가 err 우선 검증.
type Backend interface {
	// LoadCurrencies — TB_FXB_CMG005M 또는 currency.json.
	// Active=false entry 도 반환 — sync agent 가 etcd 에 삭제 mark 로 사용.
	LoadCurrencies(ctx context.Context) (Currencies, error)

	// LoadPairs — TB_FXB_CMG004M + TB_FXB_CMG006M 의 join 결과 또는 pair.json.
	// direct + cross 산식 통합. Active=false 도 포함.
	LoadPairs(ctx context.Context) (Pairs, error)

	// LoadSwapPoints — TB_FXB_CMG021M 또는 swap_point.json. 현재 유효한 swap 만.
	LoadSwapPoints(ctx context.Context) (SwapPoints, error)

	// LoadHQMargins — TB_FXB_CMG019M (그룹별 HQ) 의 SPOT 매핑 또는 hq_margin.json.
	LoadHQMargins(ctx context.Context) (HQMargins, error)

	// LoadSiteMargins — TB_FXB_CMG015M (표준영업점) 또는 site_margin.json.
	LoadSiteMargins(ctx context.Context) (SiteMargins, error)
	// LoadUserProfiles — 고객 마스터(등급) 또는 user_profile.json.
	// 고객 등급 → 시세 Profile(Site/Tier). Oracle backend 는 raw 코드를
	// GradeMapper 로 enum 화, File backend 는 이미 enum 형태 JSON 을 읽는다.
	// Active=false 도 반환 — sync 가 etcd 삭제 mark 로 사용.
	LoadUserProfiles(ctx context.Context) (UserProfiles, error)
	// LoadCustomerSpreads — 고객 스프레드 마스터 또는 customer_margin.json.
	// 고객별 절대 스프레드 → pricing CustomerMargin(override). Active=false 도 반환.
	LoadCustomerSpreads(ctx context.Context) (CustomerSpreads, error)
}
