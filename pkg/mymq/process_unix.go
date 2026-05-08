//go:build unix

package mymq

import "os"

// processID 는 현재 프로세스의 PID 를 반환한다.
// DECLARE_SESSION 의 pid 필드에 들어가서 broker 의 whois 에 노출된다.
func processID() int {
	return os.Getpid()
}
