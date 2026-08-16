/* libcomsise read-shim — 최소 공통. 벤더 sise 공통 lib 대체 placeholder.
 * trn 이 실제 참조하는 comsise 심볼이 링크에서 undefined 로 뜨면 여기 추가한다.
 * 현재는 emsg 전역만 (일부 sise 코드가 참조). */
char emsg[1024];
