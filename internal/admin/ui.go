package admin

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiAssets 는 mci-admin 의 control UI 정적 파일.
//
// 단일 HTML SPA — Tailwind CDN + vanilla JS, 빌드 step 없이 바이너리에 그대로 embed.
// 운영팀은 별도 프론트 빌드/배포 없이 mci-admin 한 바이너리만 배포하면 된다.
//
//go:embed ui/*
var uiAssets embed.FS

// uiSubFS 는 ui/ prefix 가 제거된 sub-fs — http.FileServer 가 / 루트로 보이게.
func uiSubFS() http.FileSystem {
	sub, err := fs.Sub(uiAssets, "ui")
	if err != nil {
		panic("admin: ui assets embed 누락 — " + err.Error())
	}
	return http.FS(sub)
}

// UIHandler 는 정적 파일 서버. SPA 가 단일 HTML 이라 / 만 노출하면 충분하지만,
// 향후 자산 분리 시 /static/* 도 자연 동작하도록 일반 FileServer 사용.
//
// 인증 우회 — auth.md §3 의 로그인 화면 자체가 SPA 안에 있으므로 정적 파일은
// 인증 없이 노출되어야 한다. 보호는 API 측 (/v1/*) 미들웨어가 담당.
//
// Cache-Control: no-store 강제 — SPA 가 자주 갱신되는데 브라우저 캐시 때문에
// markup/JS 부정합 (옛 markup + 새 JS) 으로 NPE 가 발생하는 사고를 방지. 자산
// 자체가 단일 HTML 100KB대 라 운영 비용 영향 무시 가능.
func UIHandler() http.Handler {
	fs := http.FileServer(uiSubFS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	})
}
