package mymq

import (
	"testing"
)

func TestConnectRequestRoundTrip(t *testing.T) {
	req := &connectRequest{
		MqHost: "10.0.0.10",
		MqPort: 11217,
		MqUser: "trader01",
		Pid:    0xDEADBEEF,
		MyName: "mci-push-01",
		ChName: "WEB",
		ChIpad: "192.168.1.42",
		ChPort: 443,
		ExName: "EXEC",
		ExType: uint32(ExchangeFanout),
		QuName: "mci_push",
		QuFlag: QfUnsolMsg | QfUnsolHdr,
		QuAttr: uint32(QtClient),
		QuSize: 64,
		QuExpt: "",
	}
	buf := encodeConnectRequest(req)

	if len(buf) != connectionSize {
		t.Fatalf("encodeConnectRequest: 길이 %d, 기대 %d", len(buf), connectionSize)
	}

	// 클라이언트 영역의 핵심 필드가 wire 의 정확한 위치에 들어갔는지 검증.
	if got := trimNulString(buf[0:24]); got != "10.0.0.10" {
		t.Errorf("MqHost @ offset 0..23: %q", got)
	}
	if got := getU32(buf[24:28]); got != 11217 {
		t.Errorf("MqPort @ offset 24: %d", got)
	}
	if got := trimNulString(buf[28:44]); got != "trader01" {
		t.Errorf("MqUser @ offset 28..43: %q", got)
	}
	if got := getU32(buf[44:48]); got != 0xDEADBEEF {
		t.Errorf("Pid @ offset 44: 0x%X", got)
	}
	if got := trimNulString(buf[48:64]); got != "mci-push-01" {
		t.Errorf("MyName @ offset 48..63: %q", got)
	}
	// qu_flag 는 instance 시작 + 24+4+16+4+16+16+20+4+16+4+16 = 140
	if got := getU32(buf[140:144]); got != QfUnsolMsg|QfUnsolHdr {
		t.Errorf("QuFlag @ offset 140: 0x%X", got)
	}
}

func TestConnectResponseDecode(t *testing.T) {
	// instance 부분은 무시하고, 응답 영역만 채워서 디코딩 검증.
	buf := make([]byte, connectionSize)
	off := instanceSize

	putU32(buf[off:off+4], 0x11223344) // SocketID
	off += 4
	putU32(buf[off:off+4], 0x00ABCDEF) // ConnectionID
	off += 4
	copy(buf[off:off+16], "auto_q_42") // QueueName
	off += 16
	putU32(buf[off:off+4], 0xCAFEBABE) // QueueKey
	off += 4
	putU32(buf[off:off+4], 0x12345678) // QueueID
	off += 4
	putU32(buf[off:off+4], 0x87654321) // QueueMsgID
	off += 4
	putU32(buf[off:off+4], 256) // QueueSize
	off += 4
	buf[off] = 0   // ap2ap = BROKER
	buf[off+1] = 0 // ap2br = BROKER
	off += 2
	buf[off] = 1 // HowToBroadcast = MULTICAST
	off++
	buf[off] = 7 // LogSuffix
	off++
	putU32(buf[off:off+4], 30) // Heartbeat (seconds)
	off += 4
	putU32(buf[off:off+4], uint32(ZipfZlib)) // CompressMethod
	off += 4
	putU32(buf[off:off+4], 2048) // CompressSize

	r, err := decodeConnectResponse(buf)
	if err != nil {
		t.Fatalf("decodeConnectResponse: %v", err)
	}
	if r.SocketID != 0x11223344 {
		t.Errorf("SocketID: 0x%X", r.SocketID)
	}
	if r.ConnectionID != 0x00ABCDEF {
		t.Errorf("ConnectionID: 0x%X", r.ConnectionID)
	}
	if r.QueueName != "auto_q_42" {
		t.Errorf("QueueName: %q", r.QueueName)
	}
	if r.QueueKey != 0xCAFEBABE {
		t.Errorf("QueueKey: 0x%X", r.QueueKey)
	}
	if r.HowToBroadcast != 1 {
		t.Errorf("HowToBroadcast: %d", r.HowToBroadcast)
	}
	if r.Heartbeat != 30 {
		t.Errorf("Heartbeat: %d", r.Heartbeat)
	}
	if r.CompressMethod != uint32(ZipfZlib) {
		t.Errorf("CompressMethod: %d", r.CompressMethod)
	}
	if r.CompressSize != 2048 {
		t.Errorf("CompressSize: %d", r.CompressSize)
	}
}

func TestConnectionSizeMatchesC(t *testing.T) {
	// instance 192 + connection 응답 56 = 248. C 컴파일러의 자연 alignment
	// 기준 padding 없이 정렬되도록 필드를 배치했음을 회귀 보장.
	if instanceSize != 192 {
		t.Errorf("instanceSize: %d, 기대 192", instanceSize)
	}
	if connectionSize != 248 {
		t.Errorf("connectionSize: %d, 기대 248", connectionSize)
	}
}

func TestConnectResponseTooShort(t *testing.T) {
	short := make([]byte, connectionSize-1)
	if _, err := decodeConnectResponse(short); err == nil {
		t.Error("짧은 입력에 대해 에러를 기대했으나 없음")
	}
}
