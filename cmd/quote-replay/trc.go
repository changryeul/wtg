package main

import (
	"bytes"
	"regexp"
	"strconv"
	"time"
)

// soh 는 FIX 필드 구분자. .trc 캡처는 실제 SOH(0x01) 바이트를 보존한다.
const soh = "\x01"

const fixBegin = "8=FIX.4.4"

var (
	// mds 원형 (manual_feed/replay_smb2.c) 과 동일 필터:
	// symbol(55) 이 있고 MsgType 이 W(전량) 또는 X(증분) 인 시세만 재생.
	tagSymbol = []byte(soh + "55=")
	tagMsgW   = []byte(soh + "35=W" + soh)
	tagMsgX   = []byte(soh + "35=X" + soh)

	// 로그 prefix 의 시각 — [YYYY-MM-DD HH:MM:SS.ffffff] 에서 시각부만.
	stampRe = regexp.MustCompile(`(\d{2}):(\d{2}):(\d{2})\.(\d{6})`)
)

// extractFIX 는 .trc 로그 라인에서 재생 대상 FIX 시세 원문을 추출한다.
// 라인 안의 "8=FIX.4.4" 부터 닫는 괄호(로그 wrapper) 직전까지 — 괄호가
// 없으면 라인 끝까지 (CR/LF 제거). 대상이 아니면 nil.
func extractFIX(line []byte) []byte {
	start := bytes.Index(line, []byte(fixBegin))
	if start < 0 {
		return nil
	}
	msg := line[start:]
	if end := bytes.IndexByte(msg, ')'); end >= 0 {
		msg = msg[:end]
	}
	msg = bytes.TrimRight(msg, "\r\n")
	if !bytes.Contains(msg, tagSymbol) {
		return nil
	}
	if !bytes.Contains(msg, tagMsgW) && !bytes.Contains(msg, tagMsgX) {
		return nil
	}
	return msg
}

// parseStamp 는 라인의 HH:MM:SS.ffffff 를 자정 기준 offset 으로 파싱한다.
func parseStamp(line []byte) (time.Duration, bool) {
	m := stampRe.FindSubmatch(line)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(string(m[1]))
	mi, _ := strconv.Atoi(string(m[2]))
	s, _ := strconv.Atoi(string(m[3]))
	us, _ := strconv.Atoi(string(m[4]))
	return time.Duration(h)*time.Hour +
		time.Duration(mi)*time.Minute +
		time.Duration(s)*time.Second +
		time.Duration(us)*time.Microsecond, true
}

// paceDelay 는 직전/현재 스탬프와 배속으로 송신 전 대기 시간을 계산한다.
// mds 원형은 "오늘 같은 시각" 절대 정렬이라 캡처 시각 이후에만 실행
// 가능했지만, 본 도구는 메시지 간 상대 간격을 재현한다 — 아무 때나 실행
// 가능하고 배속 조절이 자연스럽다. 역행 (자정 wrap / 순서 뒤섞임) 은 대기 0.
func paceDelay(prev, cur time.Duration, speed float64) time.Duration {
	if speed <= 0 {
		return 0
	}
	d := cur - prev
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) / speed)
}
