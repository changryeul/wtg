// Package krxshm 은 KRX 파생 시세 SHM(/dev/shm/mfsise, MFSISE_T)을 Go 로 byte-exact
// 적재한다 — C sise 피드(kbfut_sise) 흡수(트랙2, mci-price-krx). yuanta trn AP 는
// 기존 libmfsise(l_mfread)로 무수정 read, 피드(수신+파싱+SHM write)는 WTG 가 담당.
//
// 레이아웃 상수는 **linux x86-64 에서 실제 sise 헤더로 검증한 값** (offsetof/sizeof).
// long double(ldouble)이 linux 16B / mac(arm64) 8B 라 반드시 타깃(EC2)에서 확정해야 함
// — 아래 값은 EC2 오라클(shmlayout.c) 산출. 변경 시 반드시 재검증(scripts/krxshm-verify.sh).
package krxshm

// MFSISE_T 전체 크기 (헤더 + KBFUT_T[MAX_FITEM]). shm_open("/mfsise") = /dev/shm/mfsise.
const (
	ShmPath = "/dev/shm/mfsise"
	ShmSize = 44957040 // MFSISE_SZ (linux, MAX_FITEM=400)
	MaxItem = 400      // MAX_FITEM
)

// MFSISE_T 헤더 오프셋.
const (
	offMaxN = 32 // int32 최대갯수
	offUseN = 36 // int32 사용갯수
	// cDate@0(YMD_T,4) oTime@16(HMS_T,8) — 필요 시 세팅
	offKbfut = 128 // kbfut[] 시작
)

// KBFUT_T 스트라이드 + 엔트리내 오프셋.
const (
	kbfutStride = 112112 // sizeof(KBFUT_T)
	offFutCd    = 0      // [12] 표준종목코드 (l_mfread bsearch 키)
	stdCdLen    = 12
	offShrtCd   = 12 // [9] 단축코드
	shtCdLen    = 9
	offFsise    = 2048 // FSISE_T 시작 (엔트리 기준)
)

// FSISE_T 내부 오프셋 (fsise 기준). 가격은 double(8B, LE), rate 는 float32, 부호는 char.
const (
	fsBPrc = 0   // 기준가
	fsOPrc = 8   // 시가
	fsHPrc = 16  // 고가
	fsLPrc = 24  // 저가
	fsEPrc = 32  // 종가(현재가)
	fsAVol = 40  // 누적거래량 (ulong 8B)
	fsAAmt = 48  // 누적거래대금 (ldouble 16B)
	fsYPrc = 64  // 전일종가
	fsDiff = 72  // 전일대비
	fsSPrc = 80  // 정산가
	fsLsPr = 88  // 최종결제가
	fsRate = 96  // 전일대비 등락률 (float32)
	fsSPcd = 100 // 정산가구분코드 [2]
	fsSign = 103 // 전일대비 부호 char
	fsHalt = 105 // 거래정지 char
)

// entryOff — 슬롯 i(0-based)의 KBFUT_T 시작 바이트 오프셋.
func entryOff(i int) int { return offKbfut + i*kbfutStride }
