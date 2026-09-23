# 贡献指南

欢迎为 Lynx 贡献代码。本文面向人类贡献者；面向 AI 助手的开发指引见
`CLAUDE.md`，两者不互相替代。

## 开发环境

- Go 1.26.5、golangci-lint 2.12.2，版本以 `mise.toml` 为唯一来源：
  安装 [mise](https://mise.jdx.dev/) 后执行 `mise install`
- Windows 下任务脚本需要 Git Bash 的 `sh` 在 PATH 中

## 仓库结构

Go workspace（`go.work`），共 11 个模块：根模块（核心框架）、`_examples`
与 9 个 `contrib/` 扩展模块。每个 contrib 有独立 `go.mod`，经 replace
指向根模块。**注意：workspace 根下的 `./...` 只匹配根模块，构建/测试
须逐模块遍历**——直接用 `mise run test` 即可。

## 提交前检查

```bash
mise run test    # 全模块 -race -shuffle=on（与 CI 同参）
mise run vuln    # 全模块 govulncheck 依赖漏洞扫描
# lint 在各模块目录下执行：golangci-lint run
```

要求：

- **每个 bug 修复尽量配一个回归测试**（仓库原则，见 ROADMAP）
- CI 强制覆盖率门槛：根与 contrib 模块 70%（`_examples` 除外）
- 新增导出符号需要 GoDoc 注释；docs/ 教程与示例涉及行为变化的同步更新

## Commit 规范

沿用仓库既有风格：`<type>: <中文主题>`，type 取
feat / fix / docs / refactor / test / build / ci / chore。复杂改动
另起正文，写"为什么"与关键取舍，不复述 diff。

## 破坏性变更

核心生命周期 API 保持稳定（v1.0 冻结）。确需破坏性更名时：CHANGELOG
明示（不带兼容别名）、示例与 docs 同步更新、集中在 minor 版本发布并
经评审。提 PR 前先开 issue 讨论。

## PR 流程

1. 大的功能或行为变化先开 issue 讨论，达成一致再动手
2. fork / 分支开发，一个 PR 聚焦一件事
3. 确保提交前检查全部通过
4. PR 描述写清动机与方案，关联对应 issue

## 发布

发布流程（多模块打 tag）由维护者执行，见 `RELEASE.md`。
