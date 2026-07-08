package mymq

import "errors"

// LZO1X (mini-LZO) 순수 Go 디코더 — 압축 해제 전용.
//
// mymq C 프레임워크 (mq_send.c) 는 navi.zipf != 0 && 본문 > 1024B 일 때
// mymq_compress() 로 본문을 압축한다. 운영 기본값은 zipf=1 (ZIPF_MLZO,
// minilzo 의 LZO1X-1). WTG 는 해제만 필요하므로 (Go → C 방향은 비압축 전송)
// 압축기는 구현하지 않는다.
//
// 스트림 문법 (minilzo lzo1x_d.ch 와 동일):
//   - opcode t 에 따라 literal run / match(M1~M4) 로 분기
//   - match 후 trailing literal 0~3개는 직전 distance 바이트 하위 2비트
//   - EOF 는 M4 opcode 0x11 + distance 0x0000
// 모든 입출력 접근은 경계 검사 (lzo1x_decompress_safe 상당).

var (
	// ErrLZOCorrupt — 스트림 문법 위반 / 입력 소진 / lookbehind 범위 초과.
	ErrLZOCorrupt = errors.New("mymq: LZO1X 스트림 손상")
	// ErrLZOOutputMismatch — 해제 결과 길이가 prefix 의 orig_size 와 불일치.
	ErrLZOOutputMismatch = errors.New("mymq: LZO1X 해제 길이가 orig_size 와 불일치")
)

// decompressLZO1X 는 LZO1X 압축 payload 를 origSize 길이의 평문으로 복원한다.
func decompressLZO1X(in []byte, origSize uint32) ([]byte, error) {
	if origSize == 0 || origSize > MaxMsgSize {
		return nil, ErrLZOCorrupt
	}
	out := make([]byte, origSize)
	ip, op, n := 0, 0, len(in)

	readU8 := func() (int, bool) {
		if ip >= n {
			return 0, false
		}
		b := int(in[ip])
		ip++
		return b, true
	}
	// 길이 연장: 0x00 연속 = +255, 마지막 비제로 바이트 + base.
	extendLen := func(base int) (int, bool) {
		t := 0
		for {
			b, ok := readU8()
			if !ok {
				return 0, false
			}
			if b == 0 {
				t += 255
				if t > int(origSize) {
					return 0, false
				}
				continue
			}
			return t + base + b, true
		}
	}
	copyLit := func(cnt int) bool {
		if cnt < 0 || ip+cnt > n || op+cnt > len(out) {
			return false
		}
		copy(out[op:], in[ip:ip+cnt])
		ip += cnt
		op += cnt
		return true
	}
	// match 복사 — 겹침(dist < cnt) 허용이라 byte 단위.
	copyMatch := func(dist, cnt int) bool {
		mp := op - dist
		if mp < 0 || op+cnt > len(out) {
			return false
		}
		for i := 0; i < cnt; i++ {
			out[op] = out[mp]
			op++
			mp++
		}
		return true
	}

	// state — 직전 명령의 trailing literal 개수 (1~3) 또는 literal run 직후 (4).
	// 다음 opcode 가 16 미만일 때의 해석을 결정한다.
	state := 0

	// 첫 바이트 특례: 17 초과면 (b-17) 개 literal 로 시작.
	if n == 0 {
		return nil, ErrLZOCorrupt
	}
	if in[0] > 17 {
		t := int(in[0]) - 17
		ip = 1
		if !copyLit(t) {
			return nil, ErrLZOCorrupt
		}
		if t < 4 {
			state = t
		} else {
			state = 4
		}
	}

	for {
		t, ok := readU8()
		if !ok {
			return nil, ErrLZOCorrupt
		}
		var dist, cnt, trailSrc int

		switch {
		case t < 16:
			switch {
			case state == 0: // literal run
				cnt = t
				if cnt == 0 {
					if cnt, ok = extendLen(15); !ok {
						return nil, ErrLZOCorrupt
					}
				}
				if !copyLit(cnt + 3) {
					return nil, ErrLZOCorrupt
				}
				state = 4
				continue
			case state == 4: // literal run 직후 M1 특례 — dist 2049~, len 3
				b2, ok2 := readU8()
				if !ok2 {
					return nil, ErrLZOCorrupt
				}
				dist = (t >> 2) + (b2 << 2) + 2049
				cnt = 3
				trailSrc = t
			default: // 직전 match 의 trailing literal 뒤 M1 — dist 1~, len 2
				b2, ok2 := readU8()
				if !ok2 {
					return nil, ErrLZOCorrupt
				}
				dist = (t >> 2) + (b2 << 2) + 1
				cnt = 2
				trailSrc = t
			}

		case t >= 64: // M2 — len 3~8, dist 1~2048
			b2, ok2 := readU8()
			if !ok2 {
				return nil, ErrLZOCorrupt
			}
			dist = ((t >> 2) & 7) + (b2 << 3) + 1
			cnt = (t >> 5) + 1
			trailSrc = t

		case t >= 32: // M3 — dist 1~16384
			cnt = t & 31
			if cnt == 0 {
				if cnt, ok = extendLen(31); !ok {
					return nil, ErrLZOCorrupt
				}
			}
			cnt += 2
			ds1, ok1 := readU8()
			ds2, ok2 := readU8()
			if !ok1 || !ok2 {
				return nil, ErrLZOCorrupt
			}
			dist = (ds1 >> 2) + (ds2 << 6) + 1
			trailSrc = ds1

		default: // 16~31: M4 — dist 16384~49151, EOF 마커 포함
			distHigh := (t & 8) << 11
			cnt = t & 7
			if cnt == 0 {
				if cnt, ok = extendLen(7); !ok {
					return nil, ErrLZOCorrupt
				}
			}
			cnt += 2
			ds1, ok1 := readU8()
			ds2, ok2 := readU8()
			if !ok1 || !ok2 {
				return nil, ErrLZOCorrupt
			}
			d := (ds1 >> 2) + (ds2 << 6)
			if distHigh == 0 && d == 0 {
				// EOF (0x11 0x00 0x00). 입력이 정확히 소진되고 출력이 꽉 차야 정상.
				if ip != n {
					return nil, ErrLZOCorrupt
				}
				if op != len(out) {
					return nil, ErrLZOOutputMismatch
				}
				return out, nil
			}
			dist = distHigh + d + 0x4000
			trailSrc = ds1
		}

		if !copyMatch(dist, cnt) {
			return nil, ErrLZOCorrupt
		}
		state = trailSrc & 3
		if state > 0 && !copyLit(state) {
			return nil, ErrLZOCorrupt
		}
	}
}
