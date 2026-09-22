// testing 示例展示应用的"组装可复用"测试约定：
// main 只留组装入口，Setup 生产与测试共用，环境差异全部经配置注入。
package main

import (
	"net/http"

	"github.com/lynx-go/lynx"
	lynxhttp "github.com/lynx-go/lynx/server/http"
)

// httpServer 暴露给测试捕获引用（获取实际监听地址）；生产 main 不使用。
// 包级变量只适用于串行用例——并行子测试请改用闭包工厂捕获
// （见 lynxtest 包自测的 newHTTPSetup 模式）。
var httpServer *lynxhttp.Server

// Setup 是应用的组装函数：生产 main 与测试共用同一函数，环境相关参数
// （地址等）一律从 a.Config() 读取——测试通过 lynxtest.WithConfigMap
// 注入覆盖值（如 http.addr=":0"），组装代码不写测试分支。
func Setup(a lynx.App) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello " + r.URL.Query().Get("name")))
	})
	httpServer = lynxhttp.NewServer(mux,
		lynxhttp.WithAddr(a.Config().GetString("http.addr")),
		lynxhttp.WithHealthCheckers(a.HealthCheckers),
	)
	a.Register(httpServer)
	return nil
}

func main() {
	lynx.NewRunner(Setup, lynx.WithName("testing-example")).Run()
}
