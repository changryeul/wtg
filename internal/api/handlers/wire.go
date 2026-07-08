package handlers

import (
	"encoding/json"
	"fmt"

	"github.com/winwaysystems/wtg/pkg/svcio"
)

// svc I/O 명세 기반 자동 marshalling — web 클라이언트가 오프셋 계산 없이
// JSON object 로 고정폭 전문 svc 를 호출할 수 있게 한다.
//
//	{"routing_key":"W1101T01","data":{"prGb":"1"}}
//	  → [COMHDR 512B][W1101T01_I] 조립 → 엔진 → 응답 [COMHDR][_O]
//	  → {"header":{...rcod/mesg...},"data":{...출력 필드...}}
//
// 적용 조건 (전부 만족 시에만 — 그 외는 기존 raw passthrough 유지):
//   - Deps.SvcIO 등록됨 (--svc-inc-dir 부팅 옵션)
//   - data 가 JSON object
//   - routing_key(=transaction code) 의 명세가 registry 에 존재
//
// COMHDR 값 규칙: trxc=routing_key, usid=인증 주체 (서버 강제 — 클라이언트
// header 로 덮어쓸 수 없음), 나머지는 기본값 위에 클라이언트 header 를 overlay.

// wireHeaderDefaults 는 COMHDR 기본값을 만든다.
func wireHeaderDefaults(rkey, usid string) map[string]interface{} {
	return map[string]interface{}{
		"trxc": rkey,
		"usid": usid,
		"ctyp": "A",  // API
		"cont": "H",  // 고객
		"rtyp": "M",  // 메시지 그대로 출력
		"ltyp": "KR", // 한국어
	}
}

// wireBuildBody 는 명세 기반 요청 body 를 조립한다.
// 적용 대상이 아니면 (nil, nil, nil) — 호출자는 기존 경로를 유지한다.
// enforceUsid 가 비어있지 않으면 header 의 usid/trxc 를 서버 값으로 강제한다.
func wireBuildBody(reg *svcio.Registry, rkey, enforceUsid string,
	clientHeader map[string]interface{}, data json.RawMessage) ([]byte, *svcio.SvcSpec, error) {
	if reg == nil || rkey == "" || len(data) == 0 {
		return nil, nil, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, nil, nil // object 아님 — raw passthrough
	}
	spec, ok := reg.Get(rkey)
	if !ok {
		return nil, nil, nil // 명세 없음 — raw passthrough (JSON 엔진 등)
	}

	usid := enforceUsid
	if usid == "" {
		if v, ok := clientHeader["usid"].(string); ok {
			usid = v
		}
	}
	hdr := wireHeaderDefaults(rkey, usid)
	for k, v := range clientHeader {
		if k == "usid" && enforceUsid != "" {
			continue // 인증 주체 강제 — 위조 방지
		}
		if k == "trxc" {
			continue // 항상 routing_key
		}
		hdr[k] = v
	}

	body, err := svcio.SerializeWithHeader(spec.HeaderFields, hdr, spec.Input, obj)
	if err != nil {
		return nil, nil, fmt.Errorf("svc 명세 직렬화 (%s): %w", rkey, err)
	}
	return body, spec, nil
}

// wireParseReply 는 엔진 응답 body 를 [COMHDR][Output] 으로 파싱한다.
// 파싱 실패 시 err — 호출자는 raw passthrough 로 폴백한다.
func wireParseReply(spec *svcio.SvcSpec, body []byte) (hdr, out map[string]interface{}, err error) {
	if spec == nil {
		return nil, nil, fmt.Errorf("spec nil")
	}
	return svcio.DeserializeWithHeader(spec.HeaderFields, spec.Output, body)
}
