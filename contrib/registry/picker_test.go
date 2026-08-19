package registry

import (
	"errors"
	"testing"
)

func TestRandomPickerEmpty(t *testing.T) {
	_, err := RandomPicker().Pick(nil)
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("want ErrNoInstance, got %v", err)
	}
}

func TestRoundRobinPickerEmpty(t *testing.T) {
	_, err := RoundRobinPicker().Pick(nil)
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("want ErrNoInstance, got %v", err)
	}
}

// TestRandomPickerIgnoresWeight：Weight 悬殊（1 vs 10000）的两实例，
// 分布仍须近似均匀。
func TestRandomPickerIgnoresWeight(t *testing.T) {
	instances := []Instance{
		{ID: "light", Weight: 1},
		{ID: "heavy", Weight: 10000},
	}
	p := RandomPicker()
	counts := map[string]int{}
	const total = 2000
	for i := 0; i < total; i++ {
		inst, err := p.Pick(instances)
		if err != nil {
			t.Fatal(err)
		}
		counts[inst.ID]++
	}
	for id, n := range counts {
		if n < 700 || n > 1300 {
			t.Fatalf("weight not ignored: %s picked %d/%d times", id, n, total)
		}
	}
	if len(counts) != 2 {
		t.Fatalf("both instances must be picked: %v", counts)
	}
}

// TestRoundRobinPickerIgnoresWeight：严格轮转，与 Weight 无关。
func TestRoundRobinPickerIgnoresWeight(t *testing.T) {
	instances := []Instance{
		{ID: "light", Weight: 1},
		{ID: "heavy", Weight: 10000},
	}
	p := RoundRobinPicker()
	for i := 0; i < 10; i++ {
		inst, err := p.Pick(instances)
		if err != nil {
			t.Fatal(err)
		}
		want := instances[i%2].ID
		if inst.ID != want {
			t.Fatalf("pick %d: want %s, got %s", i, want, inst.ID)
		}
	}
}

// TestRoundRobinPickerWrapsAround：切片长度变化时不越界。
func TestRoundRobinPickerWrapsAround(t *testing.T) {
	p := RoundRobinPicker()
	three := []Instance{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	for i := 0; i < 3; i++ {
		if _, err := p.Pick(three); err != nil {
			t.Fatal(err)
		}
	}
	one := []Instance{{ID: "x"}}
	inst, err := p.Pick(one) // 计数 3 % 1 = 0
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != "x" {
		t.Fatalf("want x, got %s", inst.ID)
	}
}
