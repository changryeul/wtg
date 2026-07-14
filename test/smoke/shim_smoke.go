//go:build ignore

// shim 스모크 — trn W2006A01 역할로 dom/W9504A01 을 1회 호출한다.
// 실행: go run test/smoke/shim_smoke.go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := mymq.Open(ctx, "127.0.0.1", 11217, mymq.Options{
		ApplName: "shim-smoke", Channel: mymq.ChannelAdmin,
	})
	if err != nil {
		fmt.Println("접속 실패:", err)
		os.Exit(1)
	}
	defer cli.Close()

	pad := func(v string, w int) string {
		for len(v) < w {
			v += " "
		}
		return v
	}
	body := "1" + pad("USD/KRW", 7) + "1" + "000001" +
		pad("M01", 3) + pad("15", 10) + pad("25", 10) +
		pad("", 10) + pad("", 10) + pad("", 10) + pad("", 10) + pad("", 10) + pad("", 10)

	reply, err := cli.Call(ctx, &mymq.FrameInput{
		Func: mymq.FCTran, Dirf: mymq.DirForward,
		Xchg: "dom", Rkey: "W9504A01", Body: []byte(body),
	})
	if err != nil {
		fmt.Println("Call 실패:", err)
		os.Exit(1)
	}
	fmt.Printf("응답: errn=%d body=%q\n", reply.Errn, reply.Body)
}
