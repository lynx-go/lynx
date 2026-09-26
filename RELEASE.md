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
mise run release-all -- vX.Y.Z "release vX.Y.Z"
```

命令行为（`mise run -n release-all -- vX.Y.Z "release vX.Y.Z"` 可预演）：

1. 按固定顺序逐个执行 `git tag -a` + `git push origin`，共 10 次（根 + 9 个 contrib）；
2. `version` / `comment` 为必填参数（也可用 `VERSION` / `COMMENT` 环境变量），无默认值，无需改文件；
3. 各 tag 均带注释（annotated tag）。

只打单个模块的 tag 也可用 `release-tag`：

```bash
mise run release-tag -- vX.Y.Z "release vX.Y.Z"                          # 根模块
mise run release-tag -- contrib/watermill-kafka/vX.Y.Z "release vX.Y.Z"  # 单个 contrib
```

## 打 tag 顺序（依赖约束）

Git 只记录 tag，模块代理在解析 `require` 时按版本号取 tag，**contrib 模块
之间的 require 交叉引用要求被依赖方先发布**，否则在无 replace 时解析不到
（unknown revision）。当前依赖关系：

```
lynx（根） ──────────┬──> contrib/zap
                      ├──> contrib/telemetry
                      ├──> contrib/cluster ─┬─> contrib/schedule
                      │                     ├─> contrib/consul
                      │                     └─> contrib/cluster-redis
                      ├──> contrib/watermill ──> contrib/watermill-kafka
                      └──> contrib/registry ──> contrib/consul
```

推荐的显式发布顺序（贡献者单模块发版时务必遵守）：

1. **根模块**：`v1.0.0`（所有 contrib 都 require 它，必须先发）；
2. **contrib/watermill**：`contrib/watermill/v1.0.0`（仅依赖根）；
3. **contrib/watermill-kafka**：`contrib/watermill-kafka/v1.0.0`（依赖根与
   contrib/watermill，须在 watermill 之后）；
4. **contrib/cluster**：`contrib/cluster/v1.0.0`（schedule / consul /
   cluster-redis 依赖它，必须先于这三者发布）；
5. **contrib/telemetry / contrib/zap / contrib/registry**：无交叉依赖，可并行
   （`contrib/{telemetry,zap,registry}/v1.0.0`）；
6. **contrib/schedule / contrib/consul / contrib/cluster-redis**：依赖 cluster（consul 另依赖 registry），须在 cluster（及 consul 所需的 registry）之后。

> 依赖关系以各 `contrib/*/go.mod` 的 require 为准；后续若新增 contrib 间
> 依赖，须在发布前更新本清单。
>
> `mise run release-all` 的循环顺序已与上表对齐（根 → watermill →
> watermill-kafka → cluster → zap / telemetry / registry → schedule →
> consul → cluster-redis）。新增 contrib 间依赖时须同步更新该顺序。

## 发版前检查清单

- [ ] **CI 全绿**：`.github/workflows/ci.yml` 中 11 个模块（根、`_examples`、9 个 contrib）的 vet、`go test -race` 与 golangci-lint 矩阵全部通过
- [ ] **本地回归**：11 个模块逐个执行 `go build ./... && go vet ./... && go test -race ./... && golangci-lint run`
- [ ] **ROADMAP 同步**：本次发版覆盖的路线图条目已勾选
- [ ] **模块间版本引用**：各 `contrib/*/go.mod` 的 `require github.com/lynx-go/lynx` 指向已发布的根模块版本；跨 contrib 依赖（consul→registry/cluster、schedule→cluster、watermill-kafka→watermill）指向对应模块的已发布版本。`replace ../` 仅 workspace 内生效、发布后对消费方无影响（消费方忽略依赖模块的 replace），无需删除；发版前确认各 require 版本已随批 bump

## 发版后

- [ ] 无需修改版本默认值：`mise run release-*` 的版本经参数 / `VERSION` 环境变量传入
- [ ] 如发布的是里程碑版本（如 v1.0.0），同步更新 `ROADMAP.md` 状态
