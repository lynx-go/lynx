package registry

import "testing"

func TestProtocolConstants(t *testing.T) {
	if ProtocolHTTP != "http" || ProtocolHTTPS != "https" || ProtocolGRPC != "grpc" {
		t.Fatalf("unexpected protocol constants: %q %q %q", ProtocolHTTP, ProtocolHTTPS, ProtocolGRPC)
	}
}

func TestStatusValues(t *testing.T) {
	// 顺序即语义：Unknown 为零值。
	if StatusUnknown != 0 || StatusPassing != 1 || StatusWarning != 2 || StatusCritical != 3 {
		t.Fatalf("unexpected status values: %d %d %d %d",
			StatusUnknown, StatusPassing, StatusWarning, StatusCritical)
	}
}

func TestErrorMessages(t *testing.T) {
	if got := ErrNoInstance.Error(); got != "registry: no healthy instance" {
		t.Fatalf("ErrNoInstance = %q", got)
	}
	if got := ErrBadName.Error(); got != "registry: empty or invalid service name" {
		t.Fatalf("ErrBadName = %q", got)
	}
}
