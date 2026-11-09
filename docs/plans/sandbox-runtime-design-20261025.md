# Sandbox 实施计划与历史来源

> 原文件的正式业务合同已迁移至[Sandbox 规格](../specs/sandbox.md)。本计划只保留阶段顺序、设计来源与后续目标，不维护第二份状态机、身份或完成合同。当前任务范围由用户指令决定，历史环境和授权不自动沿用。

## 设计输入

以下为原设计在 2026-10 的固定输入，不代表最新远端源码或当前部署，历史“尚未验证”不得解释成后来版本仍未实现。

### 1.1 依据

| 来源 | 作用 | 使用边界 |
|---|---|---|
| 旧 ANI `138887aaa096cf38512b4c68a7bab58d8ec1ac72` | 生命周期、工作盘、文件安全、code-run、端口、token、checkpoint、过期和 egress 行为 | 只作为兼容性和 live gate 输入，不作为新运行时实现或生产就绪证明 |
| Resource `docs/specs/compute-presets.md` | `preset_id`、`ResolveComputePreset`、`ComputeSelection`、`cpu_millis`、`memory_mib`、`selection_digest` | 作为 Sandbox 算力解析和冻结的唯一来源；用途绑定以该合同登记的 `SANDBOX` 为准 |
| Resource `docs/specs/instance-mvp.md` | 资源单位、CPU/内存单位、requests/limits 相等、冻结执行快照和清理语义 | 只复用通用资源语义，不复制 Notebook 专用生命周期和表结构 |
| Resource `AGENTS.md`、`CONTEXT.md` 和模块化 ADR | `cmd` 组合根、`internal/biz`、`internal/service`、`internal/data`、`internal/server` 的依赖方向，显式租户条件、持久事实和 Provider 观察 | 作为本组件唯一实现约束；Gov 只作为入站适配，不创建跨服务运行时模块 |
| Resource `docs/specs/instance-configuration-network.md`、[ADR-0002](../adr/0002-use-tenant-owned-data-without-rls.md)、[ADR-0004](../adr/0004-observe-cr-with-durable-reconciliation.md)、[ADR-0006](../adr/0006-evolve-resource-service-preserving-network.md) | 显式租户条件、PostgreSQL 事实源、Provider 观察和模块化边界 | 只复用架构原则，不把 Network 或 Instance 的私有字段、调用或数据表接入 Sandbox |
| `ani-inference-service` 的 `inference/v1.ResourceSpec` 与 managed contract | CPU/内存 Quantity、requests/limits 校验、GPU 分离原则 | 仅作历史语义参考；不作为依赖、不导入 protobuf、不访问该服务 |


## 合同接入

直接对象、权威来源、动作依赖、冻结输入、API/数据面边界和删除责任见[正式规格](../specs/sandbox.md)；执行 agent 不能据旧计划或旧测试保留越权依赖。规格与现有代码的偏差须在执行状态明确，本次文档整理不代表代码已整改。

### 2.2.2 共享租户空间开通目标

开通/关闭栅栏与标准 Namespace 的业务合同已迁至[共享开通条款](../specs/sandbox.md#222-共享租户空间开通目标)，并受[统一 Namespace 合同](../contracts/tenant-namespace.md)约束。此跳转保留旧引用，不复制合同，也不授权部署策略变更。

## 实施顺序与后续目标

按相依的纵向动作推进，复用现有 operation、journal、worker 和端口，不新增统一 observer、证明库或审批框架。每批只验证受影响的实际风险，状态始终分清设计、实现、针对性检查和真实部署。

## 8. 分阶段验收门禁

P0～P2 对应一期核心 Sandbox；P3～P5 对应二期数据面能力。每个二期 action 都可以独立发布和回滚，不要求一次性开放全部数据面。

### P0：契约审核

- 审核并冻结本文、Sandbox API、Sandbox -> Kubernetes Provider 合同和 snapshot digest；
- 决定 `SANDBOX` usage 或显式复用 `CONTAINER_INSTANCE`，同步 res 内部 compute 合同、transport identity 和测试；
- 在 `docs/specs/governance-calls.md` 和 Resource 精确 allowlist 登记 Sandbox RPC；使用 Gov 公网路径 `/api/v1/sandbox/...`，验证 tenant/actor/request_id、Sandbox ID 和 deadline；
- 明确 image、workspace、Kata、StorageClass、CSI、NetworkPolicy 和本地 data-plane adapter 的 capability matrix；
- 规定旧 ANI 每条 evidence 的 L1/L2/L3 等级和新 live gate 格式。

Proto 生成、Resource 精确 allowlist、SANDBOX compute usage、租户边界、幂等摘要和错误映射，以及 Gov -> Resource 真实 mTLS/metadata 联调和目标集群 capability matrix 均属于退出条件；执行结果按唯一状态记录。

### P1：Sandbox 事实和恢复骨架

- 新建 `internal/biz/sandbox`、`internal/data/sandbox`、`internal/service/sandbox`，保持分层边界；
- 实现 DB intent/snapshot/operation/idempotency、workspace binding、lease 和 cleanup receipt；
- 接 Runtime fake，覆盖重复 create、跨租户、unknown、重启、删除重试和过期条件更新；
- P1 只建立生命周期和恢复骨架；端口、code-run、egress、checkpoint 暂不开放，调用时返回明确的 `UNSUPPORTED` 或 `FAILED_PRECONDITION`。

状态模型、幂等、lease、unknown 和恢复代码需要对应测试；真实 PostgreSQL/Kubernetes 重启恢复、删除回收 receipt 和多副本门禁须分别记录实际结果，不能由代码存在推断通过。

### P2：Kata CPU 生命周期

真实 gate：

```text
create -> Kata Pod Ready -> local data-plane target validation
-> write /workspace -> pause -> resume -> file retained
-> delete -> 自己管理的运行对象/PVC 退出 -> 管理完成回执
```

还必须验证租户隔离、重复请求不产生第二个 Deployment、Provider 超时可恢复、旧目标在 pause/delete 后失效。

退出条件：Kata Pod Ready、PVC Bound、RuntimeClass、workspace 写入保留、pause/resume 和自身管理责任完成均有 L3 证据；实际验收需记录 image digest、cluster、namespace、UID、RBAC、网络和清理结果，并链接到唯一执行状态。

### P3：文件、Exec、code-run 和 token

通过安全测试和真实 Pod gate 后，才开放 Sandbox 自己的数据面 adapter。验证路径穿越、symlink/hard-link、输出上限、语言/超时、token scope、过期、版本变化、跨租户和密钥轮换。

文件操作只接受 `/workspace` 内相对路径；列表沿用递归、`limit/cursor`，单文件读写上限 1 MiB。写入和删除采用持久幂等意图及与生命周期相同的行锁顺序，派发后没有可信回执时保留 unknown，不自动重复；文件内容不写入操作表。实际 adapter 要求容器中 Python 3、Linux openat2/renameat2，以及独立挂载的 `/workspace`，缺少时不开放成功结果。路径解析禁止跨挂载和符号链接，覆盖使用原子替换；操作读取和写删分别授权。与旧接口的不同之处是 DELETE 通过 JSON `data` 携带路径和幂等键，并新增文件内容读取接口；上传会话暂不支持。

Token 沿用默认 15 分钟、最长一小时和 `connect`/`exec`/`files`/`ports` 的旧范围命名；当前只开放已落地的 `files` 和 `exec` 数据面，`ports` 暂不签发，默认 `connect` 当前没有可消费入口。Resource 用稳定密钥签发并保存不含明文 Token 的幂等记录；Gov 逐请求向 Resource 验证签名、撤销状态、Sandbox 版本和范围，使用独立 Token 主体而非伪造用户主体，并继续核对租户状态及签发者当前动作授权。签发前须确认签发者拥有整个所选范围的动作权限。Token 管理和生命周期接口不能由 Token 身份调用。密钥、轮换及中断边界见[部署说明](../../deployments/sandbox/README.md#sandbox-token-签名密钥)。

code-run 沿用旧业务的 Python/JavaScript、默认 60 秒和最大 300 秒；代码限 64 KiB、stdin 限 1 MiB，stdout/stderr 各最多保存 64 KiB，并标记截断。代码与 stdin 在 exec 流中分别传输，不拼接到 shell 脚本或 Kubernetes 请求 URL。超时或连接丢失不证明容器内进程已经停止：结果必须保留未知，不得自动重复执行或作为回收凭证。

新建 Pod 使用 downward API 将自身 UID 注入受控环境；准备数据面动作时先核验租户、版本、命名空间、Pod UID、Pod 规格及 Ready，执行命令再在容器内对照目标 UID。已有无该标识的 Pod 继续按旧合同观察及生命周期管理，但不提供此数据面执行能力；恢复创建的新 Pod 才可申请数据面动作。目标检查须与下述持久受理栅栏配套；两次读取或执行流关闭均不能证明与 pause/delete 原子排序。未经目标集群实测和独立 action 鉴权验收前不得发布执行接口。

code-run 受理和生命周期动作以相同数据库行锁确定顺序：暂停或删除先受理时，拒绝新的执行；执行先受理时冻结当时的 Pod UID，若在派发前状态已变化则取消，已派发的请求不得改投恢复后的 Pod。受理只持久化请求摘要，不保存代码或 stdin；进程在派发前退出后，调用方需以相同幂等键和相同内容重新提交。派发意图一经持久化不得自动重发；超过最长等待仍无回执时查询转换为 unknown，不能由此推断进程已经停止。POST 返回运行 ID 和状态，输出只通过单独授权的 GET 获取。

退出条件：文件边界测试覆盖父目录替换和 rename-out；code-run 在 Ready Pod 中成功并能处理暂停、超时、输出超限和缺少语言运行时；token 不接受跨租户、跨 Sandbox、过期或旧 generation。

### P4：端口、egress 和生产过期

端口由 Sandbox 自己创建和观察受控 Service/endpoint；NodePort 只能实验开关。NetworkPolicy 由同一 Kubernetes adapter Prepare/Observe/Release。重启 worker/Resource 后仍能完成 session/idle expiry，且重复扫描不重复操作。

退出条件：端口创建、访问、撤销、TTL 到期和清理有独立证据；egress deny/allowlist/internet 分别有 Provider 观察结果；到期动作在服务重启和 Provider unknown 后仍能恢复。

### P5：filesystem checkpoint/restore/clone

前置条件是 CSI VolumeSnapshot CRD/controller、SnapshotClass、RBAC、Workspace StorageClass 和回收策略已通过预检，且 P1 durable operation、P2 pause/resume、P4 cleanup receipt 已通过。

验证顺序为：写入 `version-1` -> snapshot ready -> 写入 `version-2` -> restore -> 读回 `version-1` -> clone -> 新 Sandbox 读回 `version-1` -> 删除并确认受管对象归零。只承诺 CSI filesystem crash-consistent；记录 PVC/Snapshot UID、RBAC、等待预算、失败和回收 receipt。没有 memory/CRIU 证据时，接口返回 `UNSUPPORTED`，不做隐含承诺。

### 8.1 所有阶段通用门禁

1. **契约门禁：** pinned protobuf 生成、API compatibility、错误映射、分层依赖和 `make verify`。
2. **状态门禁：** desired/observed、operation、lease、unknown recovery、重启后查询和幂等 replay。
3. **事实源门禁：** 清空内存、删除临时文件并重启服务后，业务状态从 PostgreSQL 恢复，PVC/Pod/Service/Snapshot 状态从 Kubernetes Provider 恢复；任何 local map/file 变化都不得改变结果。
4. **安全门禁：** 跨租户拒绝、错误 kind/state/provider 的明确错误、token scope/version、目标 UID、路径和端口边界。
5. **Provider 门禁：** Kata RuntimeClass、Pod Ready、PVC Bound、Service/Network 观察、UID ownership、删除和回收 receipt。
6. **故障门禁：** 请求超时、Provider unknown、worker 重启、CSI 未 ready、RBAC 不足和迟到对象；任何未知写入不得自动重复 create。
7. **证据门禁：** 每项记录区分 L2 local/logic 与 L3 live；旧 ANI evidence 只能作为输入，不替代 Sandbox/Provider gate。


未来 P5 等能力必须另行明确目标，不由下面历史材料推导已支持。当前文档修改仅检查链接与格式，不运行阶段中的业务验收。

## 历史设计复核与来源

| 项目 | 设计约束与来源 |
|---|---|
| 方案方向 | 独立 Sandbox 组件、冻结输入与 Provider 事实分离；按 P0～P5 门禁逐项验收 |
| 新版 `ani-resource-service` | 实现、部署和验收状态仅见[执行状态](../execution/status.md)，本文不维护第二份进度 |
| 旧 ANI | `138887aaa096cf38512b4c68a7bab58d8ec1ac72`；仅作为行为和验收输入 |
| 新 Resource 历史 Sandbox 设计 | `198f6afe536e46be5acbf5acbcbfc17ce17787e6`，仅在 `backup/main-before-sync-20261018` |
| 生产开放条件 | 对应阶段 L3 live gate、租户/幂等/恢复/删除回收证据齐全；旧 `LOCAL_VERIFIED` 不可替代 |

新版 Resource 正式依据：

- `docs/specs/compute-presets.md`：算力 preset、`ResolveComputePreset`、`ComputeSelection`、CPU/内存单位和 selection digest；
- `docs/specs/instance-mvp.md`：只复用冻结执行资源、requests/limits 和清理语义；
- `docs/specs/governance-calls.md`：仅用于确认 Sandbox 不继承 Gov 的跨服务调用边界，不作为 Sandbox 运行时依赖；
- `docs/specs/instance-configuration-network.md`：只复用 workspace、配置和 Provider 观察原则，不接入 Instance/Network 服务；
- `docs/adr/0002-use-tenant-owned-data-without-rls.md`：显式 tenant predicate；
- `docs/adr/0004-observe-cr-with-durable-reconciliation.md`：PostgreSQL 事实、Provider Observe、unknown 和恢复；
- `docs/adr/0006-evolve-resource-service-preserving-network.md`：按业务域模块化，不做共享运行时模型。

推理资源语义参考（外部仓库，不作为 Resource 运行时依赖）：

- `/root/kubercon/ani-inference-service/api/inference/v1/inference.proto` 的 `ResourceSpec`；
- `/root/kubercon/ani-inference-service/api/inference/contract/v1/managed_contract.go`；
- `/root/kubercon/ani-inference-service/docs/contracts/compute-spec-scheduling-handoff.md`。

旧 ANI 行为证据来源：

- `development-records/instance-sandbox-adapter-a.md`、`instance-sandbox-stateless-a.md`、`instance-sandbox-kata-storage-a.md`；
- `development-records/instance-sandbox-file-safety-a.md`、`instance-sandbox-file-safety-live-gate-a.md`；
- `development-records/instance-sandbox-coderun-a.md`、`instance-sandbox-ports-a.md`、`instance-sandbox-token-a.md`；
- `development-records/instance-sandbox-checkpoint-a.md`、`sandbox-expiration-egress-a.md`；
- `development-records/live-evidence/instance-sandbox-*.json`；
- `pkg/adapters/runtime/kubernetes_sandbox_runtime.go`、`sandbox_expiration_controller.go`、`pkg/security/sandboxtoken/token.go` 和旧 Gateway Sandbox 路由。

本文不授权任何远端提交、推送或 PR 操作；后续远端动作须单独获得授权。
