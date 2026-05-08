package mymq

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestDiscardLoggerNoOutput(t *testing.T) {
	c := &Client{} // Logger 없음 → discardLogger
	lg := c.logger()
	if lg == nil {
		t.Fatal("logger() 가 nil 반환")
	}
	// 호출해도 패닉 없어야 함.
	lg.Info("test")
}

func TestLogBaseAttachesAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	c := &Client{
		opts: Options{
			ApplName: "mci-test",
			Channel:  ChannelWeb,
			Logger:   slog.New(h),
		},
		host: "localhost",
		port: 11217,
	}
	c.logBase().Info("hello")

	out := buf.String()
	for _, s := range []string{
		LogKeyComponent + "=" + logComponent,
		LogKeyApplName + "=mci-test",
		LogKeyChannel + "=WEB",
		LogKeyHost + "=localhost",
		LogKeyPort + "=11217",
		"msg=hello",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("출력에 %q 없음\n전체: %s", s, out)
		}
	}
}
