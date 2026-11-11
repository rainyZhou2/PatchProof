# Verdict 与证据等级

[English](../../concepts/verdicts.md) | **简体中文**

PatchProof 在两个层级报告：每个 claim 有独立结果，Run 再聚合这些结果形成 verdict。
只有当所有策略要求的 claim 都达到声明的最低证据等级时，Run 才可能通过。

## Claim 结果

| 结果 | 含义 |
|---|---|
| `VIOLATED` | 有效反例或确定性失败与 claim 冲突。 |
| `PROVEN_WITHIN_MODEL` | 在声明模型和前提下存在机器可核验的证明。 |
| `TESTED_WITHIN_SCOPE` | 在记录的输入域和预算内，有界测试或 fuzz 通过。 |
| `UNKNOWN` | 引擎无法判定、缺少 oracle 或资源耗尽。 |
| `NOT_RUN` | 被策略或预算跳过。 |
| `INVALID_EVIDENCE` | 证据校验失败，不能支撑通过结论。 |

## Run verdict

| Verdict | 含义 |
|---|---|
| `COUNTEREXAMPLE_FOUND` | 至少一个必需 claim 被推翻。 |
| `VERIFIED_WITHIN_SCOPE` | 所有必需 claim 达到最低证据等级。 |
| `INCONCLUSIVE` | 没有确认反例，但必需证据缺失或未知。 |
| `POLICY_BLOCKED` | 补丁或环境违反可信策略不变量。 |
| `INFRA_FAILED` | 基础设施失败导致验证无法完成。 |
| `CANCELLED` | 完成前被取消。 |

## `VERIFIED_WITHIN_SCOPE` 的含义

它只针对报告中冻结的 base/head、策略摘要、输入域、前提和预算成立，不等于对任意输入
的程序正确性证明。请阅读报告中的 `assumptions` 和 `limitations`，并区分
`TESTED_WITHIN_SCOPE` 与 `PROVEN_WITHIN_MODEL`。

有效签名只证明报告由谁生成、引用字节未被修改，不证明被验证程序正确。

完整保证见 [证据与离线验签](../../api/README.md#offline-verification) 和
[威胁模型](../../security/threat-model.md)。
