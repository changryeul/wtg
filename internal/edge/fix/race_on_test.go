//go:build race

package fix

// raceEnabled — race build tag 활성 시 true. quickfix upstream race 감지되는
// test 를 -race 모드에서 skip 하기 위해 사용.
const raceEnabled = true
