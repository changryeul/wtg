# Charset 처리 (CP949 ↔ UTF-8)

## 결정 (2026-07-15)

**엔진/DB/broker 전문은 CP949 로 유지하고, charset 변환은 WTG 경계에서 한다.**
(C 엔진 최소 수정 원칙 — DB charset 이관 대공사 회피)

```
[레거시 EUC-KR HTS/EMP] ──CP949 raw─▶ WTG(무손상) ──CP949─▶ 엔진   ← 변환0, 클라수정0
[신규 web/UTF-8]        ──UTF-8 JSON─▶ WTG(변환) ──CP949─▶ 엔진   ← WTG가 UTF-8↔CP949
```

- **외부(클라이언트)**: 레거시 EUC-KR / 신규 UTF-8 공존.
- **경계(WTG)**: JSON(UTF-8) 경로만 변환. raw 전문 경로는 무손상 통과.
- **내부(엔진/DB)**: CP949 고정.

## 경로별 처리

| 경로 | Content-Type | charset 처리 |
|---|---|---|
| **raw 전문** (HTS/EMP 매매) | `application/octet-stream` | **무손상 통과** — WTG 가 바이트를 해석/변환하지 않음. 클라 EUC-KR ↔ 엔진 CP949 그대로. 클라이언트 수정 0. |
| **JSON `/v1/tx`** (svcio) | `application/json` | 송신 UTF-8→CP949, 수신 CP949→UTF-8 (`pkg/svcio/wire.go`). char[N] 은 CP949 byte-width. |
| **Quote WS** | ws text (UTF-8) | 심볼·숫자만 → 한글 없음. legacy envelope 도 ASCII. 무해. |
| **에러 메시지** (COMHDR `mesg`/errm) | — | JSON 모드는 svcio decode 가 CP949→UTF-8. raw 모드는 무손상(클라가 디코드). |

## 구현 (`pkg/svcio/wire.go`)

- **`encodeWire`** (송신, JSON→wire): UTF-8 문자열을 **rune 단위로 CP949 로 인코딩**해
  char[N] 에 배치(우측 공백 fill). N 초과 시 다음 글자를 통째로 버려 **CP949 다중
  바이트(한글 2byte) 문자가 반쪽으로 잘리지 않게** 보장. CP949 미매핑 rune 은 `?`.
- **`decodeWire`** (수신, wire→JSON): UTF-8 valid(=ASCII 등)면 그대로, 아니면
  CP949→UTF-8 변환. 그마저 실패 시 raw 바이트 보존.

예: 한글 1자 = CP949 2byte.
- `"가나"` → char[4] → `B0 A1 B3 AA`
- `"가나다"`(CP949 6byte) → char[4] → `B0 A1 B3 AA` (`"다"` 버림, 반쪽 없음)
- `"가나"` → char[3] → `B0 A1 20` (`"나"`는 남은 1byte 에 못 들어감)

## 클라이언트 가이드

### 레거시 EUC-KR HTS/EMP (매매)
- **바꿀 것 없음.** `Content-Type: application/octet-stream` + `X-WTG-Alias` 헤더로
  raw 전문 그대로 송수신하면 CP949 바이트가 무손상 왕복. (`docs/routing.md` §5)

### 신규 web (UTF-8)
- JSON 은 항상 UTF-8. 브라우저 `<meta charset="utf-8">`, 응답 `application/json; charset=utf-8`.
- 한글 필드도 UTF-8 로 보내면 WTG 가 CP949 로 변환해 엔진에 전달, 응답은 UTF-8 로 복원.
- **주의**: char[N] 의 N 은 CP949 byte 기준이다. 한글 1자=2byte, 즉 `char[20]` 은
  한글 10자. 클라 UI 의 입력 길이 제한을 byte 기준으로 맞출 것.

### 시세 WS
- 항상 UTF-8. 심볼/숫자뿐이라 EUC-KR 클라도 그대로 파싱 가능.
  (`docs/client-quote-subscribe.md`)

## 관련
- `docs/routing.md` §5 — raw 전문 무손상 통과
- `docs/client-quote-subscribe.md` — 시세 WS
- `pkg/svcio/wire.go` — encodeWire/decodeWire 구현
