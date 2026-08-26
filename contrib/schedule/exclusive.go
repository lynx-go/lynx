package schedule

// Exclusive 把 t 标成集群单例：同一次 cron 格子最多一个节点执行。
// 未包装的 Task 仍每节点各自触发。出现 Exclusive 时必须 WithStore。
func Exclusive(t Task) Task {
	if t == nil {
		return nil
	}
	if _, ok := t.(*exclusiveTask); ok {
		return t
	}
	return &exclusiveTask{Task: t}
}

type exclusiveTask struct {
	Task
}

func isExclusive(t Task) bool {
	_, ok := t.(*exclusiveTask)
	return ok
}
