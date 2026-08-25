package registry

import (
	"encoding/json"
	"errors"
	"testing"
)

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

// TestStatusString 锁定 String 的小写字符串形式（RC-16）。
func TestStatusString(t *testing.T) {
	cases := map[Status]string{
		StatusUnknown:  "unknown",
		StatusPassing:  "passing",
		StatusWarning:  "warning",
		StatusCritical: "critical",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Fatalf("Status(%d).String() = %q, want %q", int(s), got, want)
		}
	}
	// 越界值不 panic，返回带数值的形式便于定位。
	if got := Status(42).String(); got != "status(42)" {
		t.Fatalf("out-of-range String = %q, want %q", got, "status(42)")
	}
}

// TestStatusStringNegative 锁定复审 A 残余：负数枚举值曾使 statusNames
// 负下标访问直接 runtime panic（MarshalJSON 一并 panic）——必须与超上界
// 一样回落到 "status(N)" 形式。
func TestStatusStringNegative(t *testing.T) {
	if got := Status(-1).String(); got != "status(-1)" {
		t.Fatalf("Status(-1).String() = %q, want %q", got, "status(-1)")
	}
	// MarshalJSON 同走 String：越界值序列化为字符串，而非 panic。
	b, err := json.Marshal(Status(-1))
	if err != nil {
		t.Fatalf("Marshal(Status(-1)) error = %v", err)
	}
	if string(b) != `"status(-1)"` {
		t.Fatalf("Marshal(Status(-1)) = %s, want %q", b, `"status(-1)"`)
	}
	if got := Status(999).String(); got != "status(999)" {
		t.Fatalf("Status(999).String() = %q, want %q", got, "status(999)")
	}
}

// TestStatusMarshalJSON 锁定 Status 的小写字符串序列化（RC-16）。
func TestStatusMarshalJSON(t *testing.T) {
	for s, want := range map[Status]string{
		StatusUnknown:  `"unknown"`,
		StatusPassing:  `"passing"`,
		StatusWarning:  `"warning"`,
		StatusCritical: `"critical"`,
	} {
		got, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("MarshalJSON(Status(%d)) = %s, want %s", int(s), got, want)
		}
	}
	// Instance 整体序列化时 status 字段为字符串。
	data, err := json.Marshal(passing("svc", "i1"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || string(data) == "" {
		t.Fatalf("instance marshal broken: %s", data)
	}
	var round struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	if round.Status != "passing" {
		t.Fatalf("instance status serialized as %q, want \"passing\"", round.Status)
	}
}

// TestStatusUnmarshalJSON 锁定复审 B：UnmarshalJSON 同时接受小写字符串
// （本版本 MarshalJSON 输出）与 JSON 数字（v1.0 既有形态），保证同版本
// JSON 往返可用且不破坏按数字编码的旧消费方。
func TestStatusUnmarshalJSON(t *testing.T) {
	for in, want := range map[string]Status{
		`"unknown"`:  StatusUnknown,
		`"passing"`:  StatusPassing,
		`"warning"`:  StatusWarning,
		`"critical"`: StatusCritical,
		// v1.0 数字形态：向后兼容。
		`0`: StatusUnknown,
		`1`: StatusPassing,
		`2`: StatusWarning,
		`3`: StatusCritical,
	} {
		var s Status
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", in, err)
		}
		if s != want {
			t.Fatalf("Unmarshal(%s) = %d, want %d", in, s, want)
		}
	}
	// 非法形态报错：未知字符串、大小写敏感、越界数字、非 number/string。
	for _, bad := range []string{`"bogus"`, `"PASSING"`, `42`, `-1`, `1.5`, `[1]`, `true`} {
		var s Status
		if err := json.Unmarshal([]byte(bad), &s); err == nil {
			t.Fatalf("Unmarshal(%s) = nil error, want error", bad)
		}
	}
}

// TestStatusJSONRoundTrip 是 B 的核心断言：marshal 再 unmarshal 必须还原
// 原值（Instance 整体往返，status 字段为字符串形态）。
func TestStatusJSONRoundTrip(t *testing.T) {
	for _, s := range []Status{StatusUnknown, StatusPassing, StatusWarning, StatusCritical} {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back Status
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("round-trip unmarshal %s: %v", data, err)
		}
		if back != s {
			t.Fatalf("round-trip Status(%d) became %d", s, back)
		}
	}
	inst := passing("svc", "i1", Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"})
	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	var back Instance
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Status != StatusPassing {
		t.Fatalf("instance round-trip status = %d, want StatusPassing", back.Status)
	}
}

// TestMatchFilterSemantics 直接锁定导出的 MatchFilter 匹配语义（RC-08）：
// 此前 memory 与 consul 各持一份副本，本测试是两处共用实现的契约。
func TestMatchFilterSemantics(t *testing.T) {
	base := passing("svc", "i1",
		Endpoint{Protocol: ProtocolHTTP, Address: "10.0.0.1:8080"},
		Endpoint{Protocol: ProtocolGRPC, Address: "10.0.0.1:9090"},
	)
	critical := base
	critical.Status = StatusCritical
	tagged := base
	tagged.Tags = []string{"blue", "v2"}

	cases := []struct {
		name string
		f    Filter
		i    Instance
		want bool
	}{
		{"zero filter keeps passing", Filter{}, base, true},
		{"zero filter drops non-passing", Filter{}, critical, false},
		{"include unhealthy keeps critical", Filter{IncludeUnhealthy: true}, critical, true},
		{"protocol match", Filter{Protocol: ProtocolGRPC}, base, true},
		{"protocol mismatch", Filter{Protocol: ProtocolHTTPS}, base, false},
		{"tags all match", Filter{Tags: []string{"blue", "v2"}}, tagged, true},
		{"tags subset match", Filter{Tags: []string{"blue"}}, tagged, true},
		{"tags missing fails", Filter{Tags: []string{"red"}}, tagged, false},
		{"tags on untagged fails", Filter{Tags: []string{"blue"}}, base, false},
		{"combined", Filter{Protocol: ProtocolGRPC, Tags: []string{"blue", "v2"}}, tagged, true},
	}
	for _, c := range cases {
		if got := MatchFilter(c.f, c.i); got != c.want {
			t.Fatalf("%s: MatchFilter = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestErrorMessagesKeepSentinels 确保既有 sentinel 错误消息未被改动。
func TestErrorMessages(t *testing.T) {
	if got := ErrNoInstance.Error(); got != "registry: no healthy instance" {
		t.Fatalf("ErrNoInstance = %q", got)
	}
	if got := ErrBadName.Error(); got != "registry: empty or invalid service name" {
		t.Fatalf("ErrBadName = %q", got)
	}
	if !errors.Is(ErrNotRegistered, ErrNotRegistered) {
		t.Fatal("sanity")
	}
}
