package registry

import (
	"testing"

	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
)

func TestStaticAdvertiser(t *testing.T) {
	adv := Static(ProtocolHTTP, "10.0.0.1:8080")
	eps := adv.Endpoints()
	if len(eps) != 1 || eps[0] != (Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"}) {
		t.Fatalf("unexpected endpoints: %+v", eps)
	}

	if got := Static(ProtocolHTTP, "").Endpoints(); got != nil {
		t.Fatalf("empty hostPort must yield nil, got %+v", got)
	}
}

func TestHTTPAdvertiser(t *testing.T) {
	// Start 前 Addr() 为空 → Endpoints 返回 nil。
	s := lynxhttp.NewServer(nil)
	if got := HTTP(s, ProtocolHTTP).Endpoints(); got != nil {
		t.Fatalf("pre-Start must yield nil, got %+v", got)
	}

	// AdvertiseAddr 非空优先（Start 前也可用）。
	s = lynxhttp.NewServer(nil, lynxhttp.WithAdvertiseAddr("192.168.1.10:8080"))
	eps := HTTP(s, ProtocolHTTPS).Endpoints()
	if len(eps) != 1 || eps[0] != (Endpoint{Protocol: ProtocolHTTPS, Address: "192.168.1.10:8080"}) {
		t.Fatalf("unexpected endpoints: %+v", eps)
	}

	// protocol 空视为 http，绝不读 TLS 猜协议。
	if eps := HTTP(s, "").Endpoints(); eps[0].Protocol != ProtocolHTTP {
		t.Fatalf("empty protocol must default to http, got %q", eps[0].Protocol)
	}
}

func TestGRPCAdvertiser(t *testing.T) {
	s := lynxgrpc.NewServer()
	if got := GRPC(s).Endpoints(); got != nil {
		t.Fatalf("pre-Start must yield nil, got %+v", got)
	}

	s = lynxgrpc.NewServer(lynxgrpc.WithAdvertiseAddr("192.168.1.10:9090"))
	eps := GRPC(s).Endpoints()
	if len(eps) != 1 || eps[0] != (Endpoint{Protocol: ProtocolGRPC, Address: "192.168.1.10:9090"}) {
		t.Fatalf("unexpected endpoints: %+v", eps)
	}
}
