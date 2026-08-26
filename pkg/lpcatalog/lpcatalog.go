// Package lpcatalog — FX 시세 LP(유동성 공급자) 카탈로그.
//
// LP 코드 → 분류(중계사/은행/외국중계사) + 멀티캐스트 group/port 매핑을 보관한다.
// **config 베이스** — 코드 하드코딩 금지. 파일(etc/lp.json) + etcd(wtg/catalog/lp/,
// hot-reload)로 구동한다. mci-price 의 FX multicast 수신부가 이 카탈로그로 어느 group 을
// join 할지 + 수신 패킷을 어느 LP(Source)로 태깅할지 결정한다.
//
// OMS 가 LP별 multicast group 으로 시세를 재송출 → WTG 는 group→LP 로 per-source 산정
// (docs/order-architecture.md §5a). pkg/instrument 와 동일한 atomic snapshot + etcd watch 패턴.
package lpcatalog

import "sync/atomic"

// Category — LP 분류.
type Category string

const (
	CategoryBroker  Category = "broker"  // 중계사(ECN) — SMB/KMB/EBS/CMB
	CategoryBank    Category = "bank"    // 은행 — SHB/NHB
	CategoryForeign Category = "foreign" // 외국중계사 — JPM/DUH
	CategoryNDF     Category = "ndf"     // NDF(차액결제선물환) — JPN/DUN
)

// LP — 카탈로그 1건. 멀티캐스트는 group(IP)로 LP 를 구분(그룹-per-LP), port 는 공통 가능.
type LP struct {
	Code     string   `json:"code"`     // 예: SMB, KMB, SHB, JPM
	Category Category `json:"category"` // broker|bank|foreign
	Group    string   `json:"group"`    // multicast 그룹 IP (예: 227.10.40.11)
	Port     int      `json:"port"`     // multicast port (예: 45010)
	Active   bool     `json:"active"`
}

// catalogData — immutable snapshot + 역색인.
type catalogData struct {
	byCode      map[string]LP
	byGroupPort map[string]LP // "group:port" → LP (수신부 태깅용)
}

// Catalog — LP 카탈로그의 immutable snapshot 을 atomic 으로 보관.
type Catalog struct {
	p atomic.Pointer[catalogData]
}

// NewCatalog — 빈 카탈로그.
func NewCatalog() *Catalog {
	c := &Catalog{}
	c.Replace(nil)
	return c
}

func gpKey(group string, port int) string {
	return group + ":" + itoa(port)
}

// Replace — 전체 카탈로그 통째 교체 (atomic). 동일 code 중복 시 뒤가 이긴다.
func (c *Catalog) Replace(items []LP) {
	d := &catalogData{
		byCode:      make(map[string]LP, len(items)),
		byGroupPort: make(map[string]LP, len(items)),
	}
	for _, lp := range items {
		d.byCode[lp.Code] = lp
		d.byGroupPort[gpKey(lp.Group, lp.Port)] = lp
	}
	c.p.Store(d)
}

// Lookup — LP 코드로 조회.
func (c *Catalog) Lookup(code string) (LP, bool) {
	d := c.p.Load()
	if d == nil {
		return LP{}, false
	}
	lp, ok := d.byCode[code]
	return lp, ok
}

// ByGroupPort — 수신 패킷의 (group, port) → LP. FX mcast 수신부가 Source 태깅에 사용.
func (c *Catalog) ByGroupPort(group string, port int) (LP, bool) {
	d := c.p.Load()
	if d == nil {
		return LP{}, false
	}
	lp, ok := d.byGroupPort[gpKey(group, port)]
	return lp, ok
}

// ActiveFeeds — active LP 만 (join 대상). 정렬 보장 X.
func (c *Catalog) ActiveFeeds() []LP {
	d := c.p.Load()
	if d == nil {
		return nil
	}
	out := make([]LP, 0, len(d.byCode))
	for _, lp := range d.byCode {
		if lp.Active {
			out = append(out, lp)
		}
	}
	return out
}

// All — 전체 (진단/admin). 정렬 보장 X.
func (c *Catalog) All() []LP {
	d := c.p.Load()
	if d == nil {
		return nil
	}
	out := make([]LP, 0, len(d.byCode))
	for _, lp := range d.byCode {
		out = append(out, lp)
	}
	return out
}

// Size — 등록 수.
func (c *Catalog) Size() int {
	d := c.p.Load()
	if d == nil {
		return 0
	}
	return len(d.byCode)
}

// itoa — 작은 정수 → 문자열 (strconv 임포트 회피, group:port 키 조립용).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
