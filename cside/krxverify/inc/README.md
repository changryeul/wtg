# vendored KRX TR 헤더 (오라클 대조용)

`oracle.c` 가 KRX 원 TR 을 **실제 구조체로 캐스팅**해 Go 디코더와 값 대사(`make verify-krx`)
하는 데 쓰는 헤더의 **리포 내 사본**. 순수 `char[]` 고정폭 struct (무의존).

- 출처: NH/유안타 sise 피드 `sise/src/inc/*.h` (A306F 파생체결 / B606F 파생호가 /
  A006F 파생마스터 / H306F 정산가 · A301K 채권체결 / B601K 채권호가 / A001B 채권마스터).
- vendoring 이유: **WTG 리포**를 외부 sise 폴더에서 분리 — 트랙2(mci-edge-krx)가 WTG 의
  시세 클라 배포를 전담하므로, WTG 가 sise 에 갖던 유일한 의존(대사 오라클 헤더)을 리포로 흡수.
  ※ sise 폴더/kbfut_sise 자체의 은퇴는 별개: WTG 밖 **다른 시스템에 C피드 소비처가 있어**
    (win/src 엔 없지만 별도 작업 영역에 존재) 그 소비처 이관·확인 후에나 org 차원 폐기 가능.
- 갱신: KRX 가 TR 레이아웃(필드/폭)을 바꾸면 이 사본도 원본과 함께 갱신하고
  `make verify-krx` 로 Go 디코더와 재대사.
