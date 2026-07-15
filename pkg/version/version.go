// Package version 는 빌드 시 -ldflags -X 로 주입되는 git SHA 를 담는다.
// 배포 파이프라인이 "설치된 바이너리 == 기대 커밋" 을 검증하는 데 쓴다
// (stale artifact 배포를 조용히 성공시키지 않고 실패시키기 위함).
package version

// SHA 는 빌드 커밋 (git rev-parse --short). ldflags 미주입 시 "dev".
var SHA = "dev"
