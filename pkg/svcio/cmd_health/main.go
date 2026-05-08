// 헤더 디렉터리 일괄 파싱 health check — 잠시 사용 도구.
//
//	go run ./pkg/svcio/cmd_health [path]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/winwaysystems/wtg/pkg/svcio"
)

func main() {
	defaultDir := os.Getenv("HOME") + "/mywork/win/src/inc/trn"
	dir := flag.String("dir", defaultDir, "헤더 디렉터리")
	verbose := flag.Bool("v", false, "실패 헤더의 에러 출력")
	flag.Parse()

	matches, _ := filepath.Glob(filepath.Join(*dir, "*.h"))
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "헤더 없음: %s\n", *dir)
		os.Exit(1)
	}

	var ok, fail int
	type failItem struct {
		path string
		err  error
	}
	var fails []failItem
	for _, p := range matches {
		s, err := svcio.ParseFile(p)
		if err != nil {
			fail++
			fails = append(fails, failItem{p, err})
			continue
		}
		_ = s
		ok++
	}
	fmt.Printf("dir=%s\n", *dir)
	fmt.Printf("총 %d 개 헤더 — OK %d (%.1f%%) / FAIL %d\n",
		len(matches), ok, 100*float64(ok)/float64(len(matches)), fail)

	if *verbose {
		// 에러 종류별 grouping 으로 빈도 표시.
		group := map[string]int{}
		for _, f := range fails {
			key := f.err.Error()
			if len(key) > 80 {
				key = key[:80] + "..."
			}
			group[key]++
		}
		type kv struct {
			k string
			n int
		}
		var sorted []kv
		for k, v := range group {
			sorted = append(sorted, kv{k, v})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })
		fmt.Println("\n실패 패턴 빈도 (상위):")
		for i, kv := range sorted {
			if i >= 10 {
				break
			}
			fmt.Printf("  %4d × %s\n", kv.n, kv.k)
		}
	}
}
