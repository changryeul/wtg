package price

// 수동 스왑포인트 등록 — POST /v1/pricing/swap.
//
// 딜러 (trn W2006A01, cside/wtgswap 경유) 가 스왑포인트를 등록/해제하는
// 거래 경로 endpoint. mci-admin 이 아니라 mci-price 에 있는 이유: admin 은
// 없어도 거래가 진행되는 운영 콘솔이라 거래 흐름이 의존하면 안 된다.
// 반영은 etcd pricing doc CAS write — 모든 mci-price 인스턴스가 watch 로
// 즉시 hot reload (자기 자신 포함).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/pricing"
)

// DocStore 는 pricing doc 의 낙관적 갱신 저장소 (etcd 추상화 — 테스트 격리용).
type DocStore interface {
	Load() ([]byte, int64, error)             // 현재 doc + revision (없으면 nil, 0)
	CAS(next []byte, rev int64) (bool, error) // revision 일치 시에만 교체
}

// EtcdDocStore 는 DocStore 의 etcd 구현.
type EtcdDocStore struct {
	Cli *clientv3.Client
	Key string
}

func (s *EtcdDocStore) Load() ([]byte, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := s.Cli.Get(ctx, s.Key)
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, nil
	}
	return resp.Kvs[0].Value, resp.Kvs[0].ModRevision, nil
}

func (s *EtcdDocStore) CAS(next []byte, rev int64) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	txn, err := s.Cli.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(s.Key), "=", rev)).
		Then(clientv3.OpPut(s.Key, string(next))).
		Commit()
	if err != nil {
		return false, err
	}
	return txn.Succeeded, nil
}

// SwapPointDeps — SwapPointHandler 의존성.
type SwapPointDeps struct {
	Store  DocStore
	Logger *slog.Logger
}

// SwapPointRequest — POST /v1/pricing/swap 본문.
type SwapPointRequest struct {
	Pair    string               `json:"pair"`
	Clear   bool                 `json:"clear,omitempty"` // true = pair 의 스왑포인트 전체 삭제 (mds regTp=2 동등)
	Updates []pricing.SwapUpdate `json:"updates,omitempty"`
}

// SwapPointHandler — POST /v1/pricing/swap.
func SwapPointHandler(deps SwapPointDeps, devMode bool) http.HandlerFunc {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if devMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if deps.Store == nil {
			swapPointError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성 — 스왑포인트 저장 불가")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			swapPointError(w, http.StatusBadRequest, "read", err.Error())
			return
		}
		var req SwapPointRequest
		if err := json.Unmarshal(body, &req); err != nil {
			swapPointError(w, http.StatusBadRequest, "validation", "JSON 파싱 실패: "+err.Error())
			return
		}
		if req.Pair == "" {
			swapPointError(w, http.StatusBadRequest, "validation", "pair 필수")
			return
		}
		if !req.Clear && len(req.Updates) == 0 {
			swapPointError(w, http.StatusBadRequest, "validation", "updates 비어있음 (해제는 clear=true)")
			return
		}
		for i := range req.Updates {
			if req.Updates[i].Tenor == "" {
				swapPointError(w, http.StatusBadRequest, "validation", "updates[].tenor 필수")
				return
			}
			if req.Updates[i].Pair == "" {
				req.Updates[i].Pair = req.Pair
			}
		}

		// CAS 루프 — 동시 writer (mci-admin PUT 등) 경합 시 최신본으로 재시도.
		for attempt := 0; attempt < 3; attempt++ {
			cur, rev, err := deps.Store.Load()
			if err != nil {
				swapPointError(w, http.StatusInternalServerError, "etcd", err.Error())
				return
			}
			next, err := pricing.ApplySwapToDoc(cur, req.Pair, req.Updates, req.Clear)
			if err != nil {
				swapPointError(w, http.StatusBadRequest, "apply", err.Error())
				return
			}
			ok, err := deps.Store.CAS(next, rev)
			if err != nil {
				swapPointError(w, http.StatusInternalServerError, "etcd", err.Error())
				return
			}
			if ok {
				logger.Info("스왑포인트 반영", slog.String("pair", req.Pair),
					slog.Int("updates", len(req.Updates)), slog.Bool("clear", req.Clear))
				writeJSON(w, http.StatusOK, map[string]any{
					"pair": req.Pair, "applied": len(req.Updates), "clear": req.Clear,
				})
				return
			}
		}
		swapPointError(w, http.StatusConflict, "cas", fmt.Sprintf("경합 재시도 초과 (pair=%s)", req.Pair))
	}
}

// swapPointError 는 표준 오류 응답 {error, message}.
func swapPointError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}
