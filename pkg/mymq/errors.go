package mymq

import (
	"errors"
	"fmt"
)

// Error 는 broker 가 반환한 비즈니스/시스템 에러를 나타낸다.
//
// Reply 의 Errn 이 0 이 아니면 호출자는 fmt.Errorf("%w", &Error{...}) 형태로
// 받게 된다. errors.Is / errors.As 로 에러 종류를 분기할 수 있다:
//
//	r, err := c.Call(ctx, in)
//	var mqErr *mymq.Error
//	if errors.As(err, &mqErr) && mqErr.Errn == mymq.ErrAuth { ... }
type Error struct {
	Errn uint32 // ME* 에러 코드 (1000~1030)
	Msg  string // broker 가 반환한 에러 메시지 (errm)
}

// Error 는 error 인터페이스 구현.
func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("mymq: errn=%d (%s)", e.Errn, errnName(e.Errn))
	}
	return fmt.Sprintf("mymq: errn=%d (%s): %s", e.Errn, errnName(e.Errn), e.Msg)
}

// Is 는 sentinel error 와의 비교 ( errors.Is 호환).
//
// 같은 errn 을 가진 *Error 는 동등하다고 본다. 그래서 errors.Is(err, ErrAuthErr)
// 같은 검사가 가능하다.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Errn == t.Errn
}

// 자주 쓰는 errn 을 sentinel 로 노출. errors.Is 비교용.
//
// 사용 예:
//
//	if errors.Is(err, mymq.ErrAuthErr) { /* 401 */ }
//	if errors.Is(err, mymq.ErrTimeoutErr) { /* 504 */ }
//	if errors.Is(err, mymq.ErrNoSvcErr) { /* 알 수 없는 트랜잭션 */ }
var (
	ErrSystemErr        = &Error{Errn: ErrSystem}
	ErrBrokerErr        = &Error{Errn: ErrBroker}
	ErrNoReceiverErr    = &Error{Errn: ErrNoReceiver}
	ErrNoOriginErr      = &Error{Errn: ErrNoOrigin}
	ErrNoDestinationErr = &Error{Errn: ErrNoDestination}
	ErrBadArgErr        = &Error{Errn: ErrBadArg}
	ErrTooBigErr        = &Error{Errn: ErrTooBig}
	ErrTooShortErr      = &Error{Errn: ErrTooShort}
	ErrAuthErr          = &Error{Errn: ErrAuth}
	ErrTimeoutErr       = &Error{Errn: ErrTimeout}
	ErrConnRefusedErr   = &Error{Errn: ErrConnRefused}
	ErrNoSvcErr         = &Error{Errn: ErrNoSvc}
	ErrSvcTimeoutErr    = &Error{Errn: ErrSvcTimeout}
	ErrSvcAbortedErr    = &Error{Errn: ErrSvcAborted}
)

// errnName 은 알려진 errn 의 사람 친화적 이름을 반환한다 (디버깅/로깅용).
func errnName(errn uint32) string {
	switch errn {
	case ErrSystem:
		return "MESYSTEM"
	case ErrBroker:
		return "MEBROKER"
	case ErrNoReceiver:
		return "MENORCVER"
	case ErrNoOrigin:
		return "MENOORGN"
	case ErrNoDestination:
		return "MENODSTN"
	case ErrQueueName:
		return "MEQUENAME"
	case ErrQueueAttr:
		return "MEQUEATTR"
	case ErrNoQueue:
		return "MENOQUEUE"
	case ErrQueueBusy:
		return "MEQUEBUSY"
	case ErrNoBound:
		return "MENOBOUND"
	case ErrBadArg:
		return "MEBADARG"
	case ErrBadFunc:
		return "MEBADFUNC"
	case ErrBadPath:
		return "MEBADPATH"
	case ErrTooBig:
		return "METOOBIG"
	case ErrTooShort:
		return "METOOSHORT"
	case ErrNoMsg:
		return "MENOMSG"
	case ErrMsgForm:
		return "MEMSGFORM"
	case ErrBadAddr:
		return "MEBADADDR"
	case ErrBusy:
		return "MEBUSY"
	case ErrExist:
		return "MEEXIST"
	case ErrAuth:
		return "MEAUTH"
	case ErrTimeout:
		return "METIMEOUT"
	case ErrMsgIo:
		return "MEMSGIO"
	case ErrResource:
		return "MERESOURCE"
	case ErrFunc:
		return "MEFUNC"
	case ErrConnRefused:
		return "MECONNREFUSED"
	case ErrSvcBusy:
		return "MESVCBUSY"
	case ErrNoSvc:
		return "MENOSVC"
	case ErrForkPro:
		return "MEFORKPRO"
	case ErrSvcTimeout:
		return "MESVCTIMEOUT"
	case ErrSvcAborted:
		return "MESVCABORTED"
	default:
		return "MEUNKNOWN"
	}
}

// AsError 는 Reply 의 errn/msg 가 비어있지 않으면 *Error 로 감싸서 반환한다.
// 정상 응답이면 nil. 호출자는 다음과 같이 사용:
//
//	r, err := c.Call(ctx, in)
//	if err != nil { return err }
//	if e := r.AsError(); e != nil { return e }
func (r *Reply) AsError() error {
	if r == nil {
		return nil
	}
	if r.Errn == 0 && r.ErrMsg == "" {
		return nil
	}
	return &Error{Errn: r.Errn, Msg: r.ErrMsg}
}

// 컴파일 타임 인터페이스 검증 — *Error 가 항상 error 인터페이스를 만족함을 보장.
var _ error = (*Error)(nil)

// errors.Is/As 를 통한 표준 unwrap 동작이 되는지 빌드 타임에 강제하기 위한
// 인터페이스 어서션 (실행되지 않음).
var _ = func() bool {
	var e *Error
	_ = errors.Is(e, ErrAuthErr)
	return true
}
