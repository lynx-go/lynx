# Lynx 发布流程

本仓库是多模块 Go workspace：根模块 `github.com/lynx-go/lynx` 与 9 个 contrib 子模块共存于同一仓库。发布时**必须为每个模块单独打 tag**，否则 Go 模块代理无法解析子模块版本。

## 多模块 tag 约定

| 模块 | Tag 示例 |
| --- | --- |
| 根模块 `github.com/lynx-go/lynx` | `v1.0.0` |
| `github.com/lynx-go/lynx/contrib/zap` | `contrib/zap/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/watermill` | `contrib/watermill/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/watermill-kafka` | `contrib/watermill-kafka/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/telemetry` | `contrib/telemetry/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/schedule` | `contrib/schedule/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/registry` | `contrib/registry/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/consul` | `contrib/consul/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/cluster` | `contrib/cluster/v1.0.0` |
| `github.com/lynx-go/lynx/contrib/cluster-redis` | `contrib/cluster-redis/v1.0.0` |

> Go 规定位于子目录的模块，其 tag 必须以模块路径相对仓库根的目录作为前缀（`contrib/<name>/vX.Y.Z`），这是模块代理正确识别子模块版本的必要条件。

## 发版流程

一次打出全部 10 个 tag 并推送：

```bash
task release-all Version=vX.Y.Z Comment="release vX.Y.Z"
```

命令行为（已通过 `task --dry` 验证）：

1. 按 `Version` 逐个执行 `git tag -a` + `git push origin`，共 10 次（根 + 9 个 contrib）；
2. CLI 传入的 `Version` / `Comment` 会**覆盖** `Taskfile.yml` 中 `vars` 的默认值，无需改文件；
3. 各 tag 均带注释（annotated tag）。

只打单个模块的 tag 也可用 `release-tag`：

```bash
task release-tag Version=vX.Y.Z Comment="release vX.Y.Z"                       # 根模块
task release-tag Version=contrib/watermill-kafka/vX.Y.Z Comment="release vX.Y.Z" # 单个 contrib
```

## 打 tag 顺序（依赖约束）

`task release-all` 内部按固定顺序执行，但由于 Git 只记录 tag 而模块代理在
解析 `require` 时按版本号取 tag，**contrib 模块之间的 require 交叉引用要求
被依赖方先发布**，否则代理在无 replace 时解析不到（unknown revision）。
当前依赖关系：

```
lynx（根） ──────────┬──> contrib/zap
                      ├──> contrib/telemetry
                      ├──> contrib/cluster ─┬─> contrib/schedule
                      │                     ├─> contrib/consul
                      │                     └─> contrib/cluster-redis
                      ├──> contrib/registry ──> contrib/consul
                      ├──> contrib/watermill
                      └──> contrib/watermill-kafka
```

推荐的显式发布顺序（贡献者单模块发版时务必遵守）：

1. **根模块**：`v1.0.0`（所有 contrib 都 require 它，必须先发）；
2. **contrib/watermill / contrib/watermill-kafka**：`contrib/watermill/v1.0.0`、`contrib/watermill-kafka/v1.0.0`（仅依赖根；可并行）；
3. **contrib/cluster**：`contrib/cluster/v1.0.0`（schedule / consul / cluster-redis 依赖它，必须先于这三者发布）；
4. **contrib/telemetry / contrib/zap / contrib/registry**：无交叉依赖，可并行
   （`contrib/{telemetry,zap,registry}/v1.0.0`）；
5. **contrib/schedule / contrib/consul / contrib/cluster-redis**：依赖 cluster（consul 另依赖 registry），须在 cluster（及 consul 所需的 registry）之后。

> 依赖关系以各 `contrib/*/go.mod` 的 require 为准；后续若新增 contrib 间
> 依赖，须在发布前更新本清单。

## 发版前检查清单

- [ ] **CI 全绿**：`.github/workflows/ci.yml` 中 11 个模块（根、`_examples`、9 个 contrib）的 vet、`go test -race` 与 golangci-lint 矩阵全部通过
- [ ] **本地回归**：11 个模块逐个执行 `go build ./... && go vet ./... && go test -race ./... && golangci-lint run`
- [ ] **ROADMAP 同步**：本次发版覆盖的路线图条目已勾选
- [ ] **contrib 对根模块的版本引用**：各 `contrib/*/go.mod` 中 `require github.com/lynx-go/lynx` 指向已发布的根模块版本；当前为 `replace` 本地路径 + 旧版本号（如 `v0.4.0`），发版前需确认并修正，发布 contrib 时替换掉 replace（或用伪版本验证解析）

## 发版后

- [ ] 更新 `Taskfile.yml` 顶部 `vars` 的 `Version` / `Comment` 默认值为本次发布版本
- [ ] 如发布的是里程碑版本（如 v1.0.0），同步更新 `ROADMAP.md` 状态
