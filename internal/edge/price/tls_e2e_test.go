package price

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pricesvc "github.com/winwaysystems/wtg/internal/price"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// gRPC mTLS round-trip — 자체발급 cert 로 server (mci-price) + client.
//
// edge/price 의 upstreamCreds 가 만든 TLS config 가 server 의 mTLS 요구를 통과
// 하는지 검증.
func TestGRPCMTLSEndToEnd(t *testing.T) {
	ss, err := tlsutil.GenerateSelfSigned(tlsutil.SelfSignedOptions{
		CommonName: "wtg-test",
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath, err := ss.WriteToFiles(dir, "tls")
	if err != nil {
		t.Fatal(err)
	}

	// 서버 측 TLS — 자체발급 cert 가 자신의 CA 도 됨.
	srvTLS, err := tlsutil.LoadServer(tlsutil.ServerOptions{
		CertFile:     certPath,
		KeyFile:      keyPath,
		ClientCAFile: certPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvTLS)))
	priceGRPC := pricesvc.NewGRPCServer(nil, 16)
	wtgpb.RegisterPriceServiceServer(gs, priceGRPC)

	go func() { _ = gs.Serve(lis) }()
	defer gs.GracefulStop()

	// 클라이언트 측 — edge/price 의 upstreamCreds 가 만드는 TLS config 와 동등.
	cliTLS, err := tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     certPath,
		KeyFile:      keyPath,
		ServerCAFile: certPath,
		ServerName:   "localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(cliTLS)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Subscribe RPC 한 번 시도 — handshake 가 통과하는지 확인.
	client := wtgpb.NewPriceServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "test"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// stream 만 열고 즉시 닫음 — 핸드셰이크 통과 자체가 mTLS 검증.
	cancel()
	_, _ = stream.Recv() // 컨텍스트 취소로 종료
}

// 서버가 mTLS 요구하는데 클라이언트가 cert 없이 dial → handshake 실패.
func TestGRPCMTLSRejectsNoClientCert(t *testing.T) {
	ss, _ := tlsutil.GenerateSelfSigned(tlsutil.SelfSignedOptions{
		DNSNames: []string{"localhost"},
		IPs:      []net.IP{net.ParseIP("127.0.0.1")},
	})
	dir := t.TempDir()
	certPath, keyPath, _ := ss.WriteToFiles(dir, "tls")

	srvTLS, _ := tlsutil.LoadServer(tlsutil.ServerOptions{
		CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath,
	})
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	defer lis.Close()
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvTLS)))
	priceGRPC := pricesvc.NewGRPCServer(nil, 16)
	wtgpb.RegisterPriceServiceServer(gs, priceGRPC)
	go func() { _ = gs.Serve(lis) }()
	defer gs.GracefulStop()

	// 클라이언트 cert 없음 — 서버 검증만.
	cliTLS, _ := tlsutil.LoadClient(tlsutil.ClientOptions{
		ServerCAFile: certPath,
		ServerName:   "localhost",
	})
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(cliTLS)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := wtgpb.NewPriceServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "test"})
	if err != nil {
		return // dial / handshake 실패 — 기대한 결과
	}
	if _, err := stream.Recv(); err == nil {
		t.Error("client cert 없는데 stream 통과")
	}
}
