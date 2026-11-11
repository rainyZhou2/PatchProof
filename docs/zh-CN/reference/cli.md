# CLI 参考

[English](../../reference/cli.md) | **简体中文**

使用 `make build` 构建，二进制为 `bin/patchproof`。本地与远程验证共用同一客户端。

## 命令

| 命令 | 用途 |
|---|---|
| `patchproof init` | 在当前目录生成策略模板 `patchproof.yaml`。 |
| `patchproof verify` | 运行本地流程，或通过 `--api` 提交到控制面。 |
| `patchproof run submit` | 提交 Run。 |
| `patchproof run status` | 查看 Run 与步骤状态。 |
| `patchproof run list` | 游标分页列出 Run。 |
| `patchproof report` | 输出 JSON、Markdown 或 SARIF。 |
| `patchproof case reproduce` | 创建并可选等待反例重放。 |
| `patchproof proof verify` | 离线验证报告。 |
| `patchproof cluster doctor` | 检查已注册执行集群。 |

## 本地验证

```sh
bin/patchproof init
bin/patchproof verify --repo . --base HEAD~1 --head HEAD \
  --policy patchproof.yaml --format markdown
```

本地模式仅支持 `trusted-local`，不得处理不可信 PR。

## 远程验证

```sh
export PATCHPROOF_API=http://localhost:8080
export PATCHPROOF_TOKEN=...

bin/patchproof run submit --project "$PROJECT_ID" \
  --repo . --base HEAD~1 --head HEAD --policy patchproof.yaml
bin/patchproof run status "$RUN_ID" --steps
bin/patchproof report "$RUN_ID" --format sarif
bin/patchproof case reproduce --run "$RUN_ID" --case "$CASE_ID" --wait
```

`run submit` 会发送 `Idempotency-Key`；同一 key 配不同冻结输入返回 `409`。

## 离线验签

```sh
bin/patchproof proof verify --report report.json \
  --cas-root .patchproof/cas \
  --key local=.patchproof/keys/signing.pub \
  --policy patchproof.yaml
```

退出码：

| 码 | 状态 | 含义 |
|---|---|---|
| `0` | `VALID_INTEGRITY` | schema、签名、manifest、证据和策略全部有效。 |
| `1` | `INVALID` | 完整性或策略检查失败。 |
| `2` | `UNVERIFIABLE` | 缺少 CAS 对象、密钥或其他必要输入。 |
| `3` | 用法错误 | 参数或标志非法。 |

参见 [快速开始](../getting-started.md) 与 [API 指南](../../api/README.md)。
