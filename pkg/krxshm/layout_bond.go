package krxshm

// 채권 SHM(/dev/shm/mbsise, MBSISE_T) 레이아웃 — 파생(mfsise)과 평행 구조, 별도 SHM.
// 상수는 EC2(linux) 오라클(shmlayout.c) 산출. BSISE_T 는 수익률(yld) 필드 포함.

const (
	BondShmPath = "/dev/shm/mbsise"
	BondShmSize = 69843536 // MBSISE_SZ (linux, MAX_BITEM=500)
	MaxBond     = 500      // MAX_BITEM
)

// MBSISE_T 헤더 + KBOND_T 스트라이드/엔트리 오프셋.
const (
	bOffMaxN  = 32     // int32
	bOffUseN  = 36     // int32
	bOffBond  = 128    // bond[] 시작
	bondStr   = 139408 // sizeof(KBOND_T)
	bOffCd    = 0      // bondCd[12]
	bOffShort = 16     // ksNm (단축코드 대용 — 실제 KBOND 엔 shrtCd 없고 ksNm)
	bOffBsise = 592    // BSISE_T
	bOffBhoga = 784    // BHOGA_T
)

// BSISE_T (bsise 기준) 오프셋.
const (
	bsBPrc = 0   // 기준가
	bsEPrc = 32  // 종가(현재가)
	bsOYld = 40  // 시가수익률
	bsHYld = 48  // 고가수익률
	bsLYld = 56  // 저가수익률
	bsEYld = 64  // 종가수익률
	bsAVol = 72  // 누적거래량 (ulong)
	bsYPrc = 96  // 전일종가
	bsYYld = 104 // 전일수익률
	bsDiff = 112 // 전일대비
	bsRate = 120 // 전일대비율 (float32)
	bsSign = 124 // 부호 char
)

// BHOGA_T (채권 호가, bhoga 기준) 오프셋. BHOGA_U: prc@0 vol@8 yld@16 (24B).
const (
	bhStVol = 16
	bhBtVol = 24
	bhShoga = 64
	bhBhoga = 184
	bhUStr  = 24
	NBHoga  = 5
)

func bondEntryOff(i int) int { return bOffBond + i*bondStr }
