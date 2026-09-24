package eventbus

import "testing"

// TestRedeliveryLimiter 单元验证有界计数器：计数累加、成功清除、环形
// 淘汰最老条目；计数键按 handler 隔离（同一消息投给多个 handler 时，
// 成功侧不清零失败侧的累计）。
func TestRedeliveryLimiter(t *testing.T) {
	l := NewRedeliveryLimiter(3)
	if got := l.Failure("h", "a"); got != 1 {
		t.Fatalf("a first failure = %d, want 1", got)
	}
	if got := l.Failure("h", "a"); got != 2 {
		t.Fatalf("a second failure = %d, want 2", got)
	}
	l.Success("h", "a")
	if got := l.Failure("h", "a"); got != 1 {
		t.Fatalf("counter must reset after success, got %d", got)
	}

	if got := l.Failure("h1", "same"); got != 1 {
		t.Fatalf("h1 first failure = %d, want 1", got)
	}
	if got := l.Failure("h1", "same"); got != 2 {
		t.Fatalf("h1 second failure = %d, want 2", got)
	}
	l.Success("h2", "same") // 另一 handler 处理成功
	if got := l.Failure("h1", "same"); got != 3 {
		t.Fatalf("h1 count must survive h2's success, got %d, want 3", got)
	}
	l.Success("h1", "same")
	if got := l.Failure("h1", "same"); got != 1 {
		t.Fatalf("own success must clear own counter, got %d", got)
	}

	// 环形淘汰：容量 3，第 4 个键顶掉最老的 b（计数归 1），
	// 未被顶掉的 d 保持原计数。
	l2 := NewRedeliveryLimiter(3)
	l2.Failure("x", "b")
	l2.Failure("x", "c")
	l2.Failure("x", "d")
	l2.Failure("x", "e")
	if got := l2.Failure("x", "b"); got != 1 {
		t.Fatalf("b should restart at 1 after ring eviction, got %d", got)
	}
	if got := l2.Failure("x", "d"); got != 2 {
		t.Fatalf("d count = %d, want 2 (not evicted)", got)
	}
	if l2.Failure("x", "") == 0 {
		t.Fatal("empty ID must be treated as immediately terminal")
	}
}

// TestNewRedeliveryLimiterDefaults：容量 <=0 回退 4096（不 panic）。
func TestNewRedeliveryLimiterDefaults(t *testing.T) {
	l := NewRedeliveryLimiter(0)
	if got := l.Failure("h", "id"); got != 1 {
		t.Fatalf("Failure = %d, want 1", got)
	}
}
