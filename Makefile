# Winway Trading Gateway (WTG) - 빌드 진입점
#
# 컨벤션은 MyMQ src/Makefile (/Users/winwaysystems/mywork/mymq/src/Makefile)
# 와 동일한 monorepo 철학을 따른다. cmd/<service>/*.go 가 존재하는
# 서비스는 자동으로 빌드 대상에 포함된다.

GO       ?= go
GOFLAGS  ?= -trimpath
LDFLAGS  ?= -s -w
PREFIX   ?= /opt/wtg
BINDIR   ?= $(PREFIX)/bin
ETCDIR   ?= $(PREFIX)/etc

# go install 로 받은 도구를 PATH 와 무관하게 실행할 수 있도록.
# GOBIN 이 명시되어 있으면 그걸, 아니면 GOPATH/bin 으로 fallback.
GOBIN := $(shell $(GO) env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

# Build only directories under cmd/ that contain Go source. New services
# are picked up automatically once they have a main.go.
CMDS := $(notdir $(patsubst %/,%,$(sort $(dir $(wildcard cmd/*/*.go)))))

.PHONY: all build test test-v test-race test-integration vet fmt fmt-check tidy clean install \
        lint staticcheck vulncheck ci coverage ckey-echo proto cside cside-clean test-cside $(CMDS)

all: build

build: $(CMDS)

$(CMDS):
	@echo "==> building $@"
	@mkdir -p build/bin
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o build/bin/$@ ./cmd/$@

test:
	$(GO) test $(GOFLAGS) ./...

test-v:
	$(GO) test $(GOFLAGS) -v ./...

# CI 와 동일한 race detector + coverage.
test-race:
	$(GO) test $(GOFLAGS) -race -coverprofile=coverage.out -covermode=atomic ./...

# embedded etcd 통합 테스트 — build tag integration 활성, 시간 더 길게.
test-integration:
	$(GO) test $(GOFLAGS) -tags=integration -timeout=120s ./...

# Phase 2.6 — C SDK (cside/wtgpush) 빌드 + Go side e2e test.
# sample 바이너리 빌드 (외부 의존 0 — POSIX socket / make).
cside:
	$(MAKE) -C cside/wtgpush

cside-clean:
	$(MAKE) -C cside/wtgpush clean

# C SDK ↔ mci-push 핸들러 wire 호환성 검증 (build tag=cside).
# 선결: cside 타겟 먼저 빌드 필요.
test-cside: cside
	$(GO) test $(GOFLAGS) -tags=cside -run CSide ./pkg/push/...

# coverage HTML 리포트.
coverage: test-race
	$(GO) tool cover -func=coverage.out
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "==> coverage.html 생성됨"

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# CI 에서 fmt 미적용 파일을 검출하기 위한 read-only 체크.
fmt-check:
	@diff=$$(gofmt -l .); \
	if [ -n "$$diff" ]; then \
		echo "다음 파일들이 gofmt 미적용 상태입니다:"; \
		echo "$$diff"; \
		exit 1; \
	fi

tidy:
	$(GO) mod tidy

# staticcheck 는 처음 실행 시 자동 설치 (GOBIN 또는 GOPATH/bin 으로 자동 매핑).
staticcheck:
	@test -x "$(GOBIN)/staticcheck" || \
		$(GO) install honnef.co/go/tools/cmd/staticcheck@latest
	$(GOBIN)/staticcheck ./...

# govulncheck 도 첫 실행 시 자동 설치.
vulncheck:
	@test -x "$(GOBIN)/govulncheck" || \
		$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GOBIN)/govulncheck ./...

# composite lint 타겟 — 정적 분석 묶음.
lint: fmt-check vet staticcheck

# CI 가 수행하는 전체 검증을 로컬에서 한 번에.
# commit/PR 전에 `make ci` 로 사전 검증 권장.
ci: lint vulncheck test-race build
	@echo "==> CI 검증 통과"

clean:
	rm -rf build/ coverage.out coverage.html

install: build
	@mkdir -p $(BINDIR) $(ETCDIR)
	cp build/bin/* $(BINDIR)/
	cp -r etc/* $(ETCDIR)/ 2>/dev/null || true

# Phase 1 GO/NO-GO 검증: mymqd 가 ckey 를 echo 하는지 확인.
# 실행 전 mymqd 가 가동 중이어야 하며, 호스트는 환경변수로 덮어쓸 수 있다.
ckey-echo: build
	./build/bin/mci-test --ckey-echo --host=localhost

# proto 생성 — api/proto/ 하위 .proto 변경 후 호출.
# 결과: pkg/wtgpb/v1/*.pb.go (커밋 대상).
proto:
	@test -x "$(GOBIN)/protoc-gen-go" || \
		$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@test -x "$(GOBIN)/protoc-gen-go-grpc" || \
		$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@command -v protoc >/dev/null || (echo "protoc 미설치 — brew install protobuf" && exit 1)
	@rm -rf pkg/wtgpb/v1
	@mkdir -p pkg/wtgpb/v1
	PATH=$(GOBIN):$$PATH protoc \
		--proto_path=api/proto \
		--go_out=pkg/wtgpb/v1 --go_opt=paths=source_relative \
		--go_opt=Mwtg/v1/price.proto=github.com/winwaysystems/wtg/pkg/wtgpb/v1 \
		--go_opt=Mwtg/v1/push.proto=github.com/winwaysystems/wtg/pkg/wtgpb/v1 \
		--go_opt=Mwtg/v1/quote_validation.proto=github.com/winwaysystems/wtg/pkg/wtgpb/v1 \
		--go-grpc_out=pkg/wtgpb/v1 --go-grpc_opt=paths=source_relative \
		--go-grpc_opt=Mwtg/v1/price.proto=github.com/winwaysystems/wtg/pkg/wtgpb/v1 \
		--go-grpc_opt=Mwtg/v1/push.proto=github.com/winwaysystems/wtg/pkg/wtgpb/v1 \
		--go-grpc_opt=Mwtg/v1/quote_validation.proto=github.com/winwaysystems/wtg/pkg/wtgpb/v1 \
		wtg/v1/price.proto wtg/v1/push.proto wtg/v1/quote_validation.proto
	@mv pkg/wtgpb/v1/wtg/v1/*.go pkg/wtgpb/v1/ 2>/dev/null || true
	@rm -rf pkg/wtgpb/v1/wtg
	@echo "==> proto 생성 완료: pkg/wtgpb/v1/"
