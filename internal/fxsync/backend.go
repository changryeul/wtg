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
}
