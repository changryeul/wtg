package fix

// orderrej_reason.go — mymq.Error 의 errn 코드 → FIX 4.4 OrdRejReason (tag 103).
//
// FIX 4.4 §4.3 OrdRejReason values:
//
//	0  = Broker / Exchange option
//	1  = Unknown symbol
//	2  = Exchange closed
//	3  = Order exceeds limit
//	4  = Too late to enter
//	5  = Unknown order
//	6  = Duplicate order (e.g. dupe ClOrdID)
//	7  = Duplicate of a verbally communicated order
//	8  = Stale order
//	9  = Trade along required
//	10 = Invalid Investor ID
//	11 = Unsupported order characteristic
//	12 = Surveillance option
//	13 = Incorrect quantity
//	14 = Incorrect allocated quantity
//	15 = Unknown account(s)
//	18 = Invalid Price Increment
//	99 = Other
//
// 운영 매핑 — mymq.Error 의 errn 코드 (NH 매매 엔진 컨벤션) 에서 위 값으로.
// 미매핑 errn 은 99 (Other) — 매매 엔진의 errn 변경 시 본 매핑도 확장 필요.
//
// 참고: pkg/mymq 의 Error 정의는 errn=int. 운영 errn 카탈로그는 매매 엔진
// 측에 있어 본 매핑은 NH 의 실제 errn 을 받기 전까지는 placeholder. Phase C
// 이상 작업에서 NH 엔진 측과 합의 후 확정.

// mapOrdRejReason — mymq errn → FIX OrdRejReason. 빈 입력 / 미매핑 시 "".
//
// rejText 는 ExecutionReport 의 tag 58 (Text) 에 첨부할 사람-읽기용 사유.
func mapOrdRejReason(errn string) (fixReason string, rejText string) {
	switch errn {
	case "":
		return "", ""
	// === NH 매매 엔진의 운영 errn 매핑 placeholder ===
	case "1029":
		return "99", "quote id expired"
	case "1030":
		return "2", "kill switch active"
	case "1031":
		return "3", "order exceeds limit"
	case "1032":
		return "1", "unknown symbol"
	case "1033":
		return "6", "duplicate order"
	case "1034":
		return "13", "incorrect quantity"
	case "1035":
		return "11", "unsupported order characteristic"
	case "1036":
		return "8", "stale order (too late)"
	case "1037":
		return "5", "unknown order"
	default:
		// 미매핑 — Other + 사유 텍스트에 errn 보존 (디버그/audit).
		return "99", "unmapped errn=" + errn
	}
}

// IsRejection — exec_type / ord_status 가 거부 의미인지.
func IsRejection(execType, ordStatus string) bool {
	// FIX 4.4: ExecType 8 = Rejected, OrdStatus 8 = Rejected.
	return execType == "8" || ordStatus == "8"
}
