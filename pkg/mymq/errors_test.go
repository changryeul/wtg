package mymq

import (
	"errors"
	"testing"
)

func TestErrorString(t *testing.T) {
	e := &Error{Errn: ErrAuth, Msg: "Authentication failed"}
	got := e.Error()
	want := "mymq: errn=1020 (MEAUTH): Authentication failed"
	if got != want {
		t.Errorf("Error()\n got: %q\nwant: %q", got, want)
	}

	// Msg 비어있을 때.
	e2 := &Error{Errn: ErrTimeout}
	got2 := e2.Error()
	want2 := "mymq: errn=1021 (METIMEOUT)"
	if got2 != want2 {
		t.Errorf("Error()\n got: %q\nwant: %q", got2, want2)
	}
}

func TestErrorIs(t *testing.T) {
	e := &Error{Errn: ErrAuth, Msg: "details"}
	if !errors.Is(e, ErrAuthErr) {
		t.Error("같은 errn 의 *Error 는 sentinel 과 동등해야 함")
	}
	if errors.Is(e, ErrTimeoutErr) {
		t.Error("다른 errn 은 동등하지 않아야 함")
	}
}

func TestErrorAs(t *testing.T) {
	e := &Error{Errn: ErrSvcTimeout, Msg: "service stalled"}
	var target *Error
	if !errors.As(e, &target) {
		t.Fatal("errors.As 가 *Error 추출 실패")
	}
	if target.Errn != ErrSvcTimeout {
		t.Errorf("Errn 추출 실패: %d", target.Errn)
	}
	if target.Msg != "service stalled" {
		t.Errorf("Msg 추출 실패: %q", target.Msg)
	}
}

func TestReplyAsError(t *testing.T) {
	// 정상 reply.
	ok := &Reply{Errn: 0, ErrMsg: ""}
	if err := ok.AsError(); err != nil {
		t.Errorf("정상 reply 는 nil error 여야 함, got %v", err)
	}

	// 에러 reply.
	bad := &Reply{Errn: ErrAuth, ErrMsg: "bad password"}
	err := bad.AsError()
	if err == nil {
		t.Fatal("에러 reply 는 non-nil error 여야 함")
	}
	if !errors.Is(err, ErrAuthErr) {
		t.Errorf("ErrAuthErr 와 일치해야 함")
	}

	// nil safety.
	var nilReply *Reply
	if err := nilReply.AsError(); err != nil {
		t.Errorf("nil reply 는 nil error: %v", err)
	}

	// errn=0 + msg 만 있는 경우도 에러로 취급 (broker 가 errn 안 채울 수도 있음).
	msgOnly := &Reply{ErrMsg: "soft fail"}
	if err := msgOnly.AsError(); err == nil {
		t.Error("msg 가 있으면 에러로 인식해야 함")
	}
}

func TestErrnNameAllKnown(t *testing.T) {
	// 알려진 모든 ME* 가 이름을 가진다.
	known := []uint32{
		ErrSystem, ErrBroker, ErrNoReceiver, ErrNoOrigin, ErrNoDestination,
		ErrQueueName, ErrQueueAttr, ErrNoQueue, ErrQueueBusy, ErrNoBound,
		ErrBadArg, ErrBadFunc, ErrBadPath, ErrTooBig, ErrTooShort,
		ErrNoMsg, ErrMsgForm, ErrBadAddr, ErrBusy, ErrExist,
		ErrAuth, ErrTimeout, ErrMsgIo, ErrResource, ErrFunc,
		ErrConnRefused, ErrSvcBusy, ErrNoSvc, ErrForkPro, ErrSvcTimeout, ErrSvcAborted,
	}
	for _, errn := range known {
		name := errnName(uint32(errn))
		if name == "" || name == "MEUNKNOWN" {
			t.Errorf("errn %d 는 이름이 있어야 함, got %q", errn, name)
		}
	}
	// 모르는 코드는 MEUNKNOWN.
	if got := errnName(9999); got != "MEUNKNOWN" {
		t.Errorf("errnName(9999) = %q, want MEUNKNOWN", got)
	}
}
