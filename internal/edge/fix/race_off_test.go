//go:build !race

package fix

// raceEnabled — race build tag 비활성 시 false. 일반 go test 에서는 이 상수를
// 통해 race-특수 skip 을 건너뜀.
const raceEnabled = false
