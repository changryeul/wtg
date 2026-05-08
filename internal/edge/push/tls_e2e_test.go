package push

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pushsvc "github.com/winwaysystems/wtg/internal/push"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// gRPC mTLS round-trip — 자체발급 cert 로 server (mci-push) + client.
func TestPushGRPCMTLSEndToEnd(t *testing.T) {
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
	pushGRPC := pushsvc.NewGRPCServer(nil, 16)
	wtgpb.RegisterPushServiceServer(gs, pushGRPC)

	go func() { _ = gs.Serve(lis) }()
	defer gs.GracefulStop()

	// 클라이언트 측 — edge/push 의 upstreamCreds 가 만드는 TLS config 와 동등.
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

	client := wtgpb.NewPushServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.Subscribe(ctx, &wtgpb.PushSubscribeRequest{SubscriberId: "test"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	_, _ = stream.Recv()
}

// 서버가 mTLS 요구하는데 클라이언트가 cert 없이 dial → handshake/통신 실패.
func TestPushGRPCMTLSRejectsNoClientCert(t *testing.T) {
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
	pushGRPC := pushsvc.NewGRPCServer(nil, 16)
	wtgpb.RegisterPushServiceServer(gs, pushGRPC)
	go func() { _ = gs.Serve(lis) }()
	defer gs.GracefulStop()

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

	client := wtgpb.NewPushServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := client.Subscribe(ctx, &wtgpb.PushSubscribeRequest{SubscriberId: "test"})
	if err != nil {
		return
	}
	if _, err := stream.Recv(); err == nil {
		t.Error("client cert 없는데 stream 통과")
	}
}
