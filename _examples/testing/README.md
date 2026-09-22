# testing 示例：组装可复用的应用测试

展示 Lynx 应用的测试约定：

- `main.go` 只留组装入口，`Setup` 生产与测试共用；
- 环境差异（监听地址等）全部从 `a.Config()` 读取；
- `main_test.go` 用 `lynxtest.Run` + `WithConfigMap` 注入 `http.addr=":0"`，
  在测试进程内拉起完整应用并断言业务端点与健康端点。

```bash
go test -v ./testing/
```

详见 `docs/08-testing.md` 与 `docs/design-testkit.md`。
