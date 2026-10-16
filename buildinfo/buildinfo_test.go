package buildinfo

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestBuildInfoDisplay(t *testing.T) {
	cases := []struct {
		name string
		info BuildInfo
		want string
	}{
		{"zero falls back to dev", BuildInfo{}, "dev"},
		{"version only", BuildInfo{Version: "v1.2.3"}, "v1.2.3"},
		{"version and commit", BuildInfo{Version: "v1.2.3", Commit: "abc1234"}, "v1.2.3 (commit abc1234)"},
		{"full triple", BuildInfo{Version: "v1.2.3", Commit: "abc1234", Date: "2026-10-17T00:00:00Z"}, "v1.2.3 (commit abc1234, built 2026-10-17T00:00:00Z)"},
		{"empty version with commit", BuildInfo{Commit: "abc1234", Date: "2026-10-17T00:00:00Z"}, "dev (commit abc1234, built 2026-10-17T00:00:00Z)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildInfoIsZero(t *testing.T) {
	if !(BuildInfo{}).IsZero() {
		t.Error("zero BuildInfo should be IsZero")
	}
	for _, info := range []BuildInfo{
		{Version: "v1"},
		{Commit: "abc"},
		{Date: "2026-10-17T00:00:00Z"},
	} {
		if info.IsZero() {
			t.Errorf("%+v should not be IsZero", info)
		}
	}
}

// ldflags 探针的注入值：子进程断言同一组常量（父子同包编译，常量共享）。
const (
	probeVersion = "v9.8.7+probe"
	probeCommit  = "deadbeefprobe"
	probeDate    = "1985-10-26T01:20:00Z"
)

// buildinfoLdflagsTarget 是三个注入目标的共享前缀（文档注释的机器可执行
// 形态——注释拼错探针不会红，这里拼错用户构建期静默 no-op，两者都钉）。
const buildinfoLdflagsTarget = "github.com/lynx-go/lynx/buildinfo"

// TestBuildInfoLdflagsInjection 是注入通道的活体探针：以子 go test 进程
// 携带 -ldflags -X 重跑本包，断言文档承诺的三个目标精确生效。go test
// 自身即注入载体，无需 testdata 夹具。
func TestBuildInfoLdflagsInjection(t *testing.T) {
	if testing.Short() {
		t.Skip("ldflags probe spawns a child go test (slow)")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not in PATH: %v", err)
	}
	ldflags := fmt.Sprintf(
		"-X %s.Version=%s -X %s.Commit=%s -X %s.Date=%s",
		buildinfoLdflagsTarget, probeVersion,
		buildinfoLdflagsTarget, probeCommit,
		buildinfoLdflagsTarget, probeDate,
	)
	cmd := exec.Command("go", "test", ".", "-run", "^TestBuildInfoLdflagsProbeChild$", "-count=1", "-ldflags", ldflags)
	cmd.Env = append(os.Environ(), "LYNX_BUILDINFO_PROBE_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe child go test failed: %v\n%s", err, out)
	}
}

// TestBuildInfoLdflagsProbeChild 只被 TestBuildInfoLdflagsInjection 以
// -ldflags 注入形态拉起，直接运行时跳过。
func TestBuildInfoLdflagsProbeChild(t *testing.T) {
	if os.Getenv("LYNX_BUILDINFO_PROBE_CHILD") != "1" {
		t.Skip("child-only: spawned by TestBuildInfoLdflagsInjection with -ldflags")
	}
	if Version != probeVersion || Commit != probeCommit || Date != probeDate {
		t.Fatalf("ldflags injection missed: version=%q commit=%q date=%q, want %q/%q/%q",
			Version, Commit, Date, probeVersion, probeCommit, probeDate)
	}
}
