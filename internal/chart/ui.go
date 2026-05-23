package chart

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiAssets 는 mci-chart 의 챠트 SPA 정적 파일.
//
// 단일 HTML — Tailwind CDN + TradingView lightweight-charts CDN + vanilla JS.
// 빌드 step 없이 바이너리에 그대로 embed. 운영팀은 mci-chart 한 바이너리만 배포.
//
//go:embed ui/*
var uiAssets embed.FS

// uiSubFS 는 ui/ prefix 제거된 sub-fs — / 루트에서 index.html 이 보이게.
func uiSubFS() http.FileSystem {
	sub, err := fs.Sub(uiAssets, "ui")
	if err != nil {
		panic("chart: ui assets embed 누락 — " + err.Error())
	}
	return http.FS(sub)
}

// UIHandler 는 정적 파일 서버.
//
// 현재 인증 미들웨어 미적용 — REST /v1/chart 와 WS /v1/chart/stream 도 미인증
// (mci-edge-chart 같은 DMZ 게이트가 생기면 그쪽에서 인증 부착).
// 운영 배포 전에 보안 정책 합의 필요 (사내망 전용 / VPN 등).
//
// Cache-Control: no-store — admin SPA 와 동일 (markup/JS 부정합 방지).
func UIHandler() http.Handler {
	fs := http.FileServer(uiSubFS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	})
}
