package registry

import (
	"errors"
	"maps"
	"slices"
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

// frozenKeyedInstances / frozenKeyedVector 是 RendezvousPicker 的冻结测试
// 向量：期望值由独立的 FNV-1a 64 参考实现离线计算，不是由本实现生成。
// 向量即契约——任何改动 hash 编码（拼接顺序、分隔符、hash 函数、
// tie-break）都会在此变红。消费方（如 messageloop 的按场景定向路由）
// 双持同一份向量，两侧同步钉死。
var frozenKeyedInstances = []Instance{
	{ID: "inst-1"}, {ID: "inst-2"}, {ID: "inst-3"}, {ID: "inst-4"}, {ID: "inst-5"},
}

var frozenKeyedVector = map[string]string{
	"acme:battle.scene-42": "inst-4",
	"acme:battle.scene-43": "inst-3",
	"acme:battle.scene-44": "inst-2",
	"im.chat.room1":        "inst-1",
	"game.tick.001":        "inst-3",
	"":                     "inst-5",
	"游戏.房间.1":              "inst-4",
}

func TestRendezvousPickerEmpty(t *testing.T) {
	_, err := RendezvousPicker().Pick("any", nil)
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("want ErrNoInstance, got %v", err)
	}
}

// TestRendezvousPickerFrozenVector：逐 key 断言冻结属主。
func TestRendezvousPickerFrozenVector(t *testing.T) {
	p := RendezvousPicker()
	for _, key := range slices.Sorted(maps.Keys(frozenKeyedVector)) {
		inst, err := p.Pick(key, frozenKeyedInstances)
		if err != nil {
			t.Fatal(err)
		}
		if want := frozenKeyedVector[key]; inst.ID != want {
			t.Fatalf("key %q: want %s, got %s", key, want, inst.ID)
		}
	}
}

// TestRendezvousPickerStable：重复调用与独立构造的 picker 结果一致。
func TestRendezvousPickerStable(t *testing.T) {
	a, b := RendezvousPicker(), RendezvousPicker()
	for key := range frozenKeyedVector {
		first, err := a.Pick(key, frozenKeyedInstances)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			again, err := b.Pick(key, frozenKeyedInstances)
			if err != nil {
				t.Fatal(err)
			}
			if again.ID != first.ID {
				t.Fatalf("key %q: %s then %s", key, first.ID, again.ID)
			}
		}
	}
}

// frozenRemovalVector：摘除某实例后，原属主是该实例的 key 的冻结新属主
// （同样由参考实现离线计算）。Rendezvous 最小扰动：其余 key 属主不变。
var frozenRemovalVector = map[string]map[string]string{
	"inst-1": {"im.chat.room1": "inst-3"},
	"inst-2": {"acme:battle.scene-44": "inst-3"},
	"inst-3": {"acme:battle.scene-43": "inst-2", "game.tick.001": "inst-2"},
	"inst-4": {"acme:battle.scene-42": "inst-5", "游戏.房间.1": "inst-5"},
	"inst-5": {"": "inst-4"},
}

// TestRendezvousPickerMinimalDisruption：摘除实例只迁移其名下 key，
// 且新属主按冻结向量落位。
func TestRendezvousPickerMinimalDisruption(t *testing.T) {
	p := RendezvousPicker()
	for removed, moved := range frozenRemovalVector {
		survivors := make([]Instance, 0, len(frozenKeyedInstances)-1)
		for _, inst := range frozenKeyedInstances {
			if inst.ID != removed {
				survivors = append(survivors, inst)
			}
		}
		for key, wantBefore := range frozenKeyedVector {
			inst, err := p.Pick(key, survivors)
			if err != nil {
				t.Fatal(err)
			}
			if wantBefore == removed {
				if want := moved[key]; inst.ID != want {
					t.Fatalf("remove %s, key %q: want migrated to %s, got %s", removed, key, want, inst.ID)
				}
				continue
			}
			if inst.ID != wantBefore {
				t.Fatalf("remove %s, key %q: owner must stay %s, got %s", removed, key, wantBefore, inst.ID)
			}
		}
	}
}

// TestRendezvousPickerIgnoresWeight：Weight 悬殊不影响属主。
func TestRendezvousPickerIgnoresWeight(t *testing.T) {
	weighted := []Instance{
		{ID: "inst-1", Weight: 1},
		{ID: "inst-2", Weight: 10000},
		{ID: "inst-3", Weight: 1},
		{ID: "inst-4", Weight: 10000},
		{ID: "inst-5", Weight: 1},
	}
	p := RendezvousPicker()
	for key, want := range frozenKeyedVector {
		inst, err := p.Pick(key, weighted)
		if err != nil {
			t.Fatal(err)
		}
		if inst.ID != want {
			t.Fatalf("key %q: weight not ignored, want %s, got %s", key, want, inst.ID)
		}
	}
}

// TestRendezvousPickerSingleInstance：单实例恒返回它。
func TestRendezvousPickerSingleInstance(t *testing.T) {
	only := []Instance{{ID: "solo"}}
	for _, key := range []string{"", "any-key", "游戏"} {
		inst, err := RendezvousPicker().Pick(key, only)
		if err != nil {
			t.Fatal(err)
		}
		if inst.ID != "solo" {
			t.Fatalf("key %q: want solo, got %s", key, inst.ID)
		}
	}
}
