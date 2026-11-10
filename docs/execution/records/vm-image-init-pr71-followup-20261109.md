# PR #71 剩余修复：镜像创建重放与声明制脚本

日期：2026-11-09（Asia/Shanghai）

源码基线：`28ef401355390f0bd867bf8f593c529b7dad2bd1`。本记录只覆盖该基线之后的两项剩余修复；原 PR 的 006/007、部署与验收记录保持原有时点。

## 变更及边界

1. 在现有显式 `Migrate` 中追加版本 8。只处理 `vm.image_requests` 中 action 为 CreateImage 且原创建快照没有 `InitializationCapability` 的旧记录。
2. 从请求自身的 `record` 重建创建时的名称、描述、类型、格式、容量和 OS，复用现有 `ImageRequestDigest` 校验旧摘要，再生成同一请求加缺省 `NONE` 的摘要。不得从可能已改名或改描述的当前镜像行恢复原意图。
3. 只更新 `request_digest`；tenant、action、幂等键、原 image ID、创建快照和时间戳保持不变。已有 NONE/NoCloud 声明和其他 action 不改。旧摘要不匹配时整个事务回滚。
4. 008 SQL 标记与对应 Go 迁移源码共同参与 checksum。迁移只在版本 8 尚未登记时执行；普通请求重放没有双摘要或旧版本兼容分支。
5. 删除验收脚本 `catalog()` 对已删除的 `initializationVerified` 的断言，保留 Linux、DISK_IMAGE、CLOUD_INIT_NOCLOUD 条件及现有来源身份检查。
6. 已应用的 001–007、VM `Spec.Digest`、原生对象身份标签、凭据密文和 CI 配置均不修改。旧 VM 的摘要还用于对象身份和凭据 AAD，不为本次镜像请求迁移重算。

## 一次性切换

- 使用现有维护窗口停止旧版本镜像创建写入，处理尚未结束的旧请求投递；不要在旧写入者仍可能生成旧摘要时完成最终切换。
- 静态 media 配置删除已经退出接口的 `initialization_verified` 字段；平台目录保留明确登记的 capability。配置加载继续严格拒绝未知字段。
- 用 schema owner 身份运行现有 VM 显式迁移入口，runtime role 与 owner 保持分离。008 与原有迁移共用事务和 advisory lock；出现坏快照/摘要不匹配应停止并处理具体原记录，不能清空请求表后让重试创建第二个镜像。
- 确认版本 8 已登记后切换业务进程，检查一次原创建请求重放返回同一 image ID。重复运行迁移应无新增数据变化。
- Gov 配套仍按原 PR 约定接入正式 Res 依赖。涉及旧 VM 创建命令的未完成投递需要在切换窗口按原命令状态处理；本次不改 Gov，不增加运行时兼容，也不批量重算旧 VM 摘要。
- 本次只交付代码，没有连接测试集群、执行生产/测试数据库迁移或部署。PR 中 r4/007 的既有部署记录不代表 008 已应用。

## 实际验证

执行环境：ChatGPT 隔离 Linux 工作区，源文件经 Git blob SHA 与上述基线逐项核对。仓库约定的 `/home/chabking/.ssh/config` 和 `/home/ubuntu/.ssh/config` 均不可用；环境中名为 `go` 的程序返回 `Go: Unknown option: version`，不是可用的 Go 编译工具链，且没有 gofmt、PostgreSQL 或容器运行工具。未降低 go.mod 的 Go 1.26.10 要求。

| 检查 | 结果 | 范围 |
|---|---|---|
| 脚本 AST 解析与离线 catalog 行为 | pass | 只编译 require/catalog 定义并注入 fake API；无旧字段的 Linux disk NoCloud 通过；NONE、Windows、ISO 各自拒绝。未导入脚本顶层或访问凭据、网络、集群。 |
| 三份固定完整请求载荷摘要 | pass | 独立用 Python 重建旧版、NONE、NoCloud 的有序 JSON，SHA-256 与回归测试固定值一致。 |
| 实现与测试独立静态复核 | pass | 原快照来源、字段顺序、身份绑定、已有声明不变、失败回滚和重复执行边界。 |
| `git diff --check` | pass | 本次差异的空白检查。 |
| Go 单元测试、编译、gofmt、`make verify` | pass | GitHub CI 在 `7e126f38` 上执行成功；见下方补验记录。本地工作区未执行。 |
| VM 镜像 PostgreSQL 集成测试 | not_verified | 本地没有隔离 PostgreSQL；现有 CI 未启用 `instanceintegration` 和 `VM_IMAGE_TEST_ADMIN_DSN`，没有把 skip 或 Image 领域测试记作此项通过。 |
| 集群部署、真实 API/前端全链路 | not_verified | 本次未执行。 |

## 发布与 GitHub CI 补验

原 PR #71 的 `java559` fork 对当前 GitHub 连接没有写权限；Git tree 创建返回 403，带预期旧 SHA 的分支更新也被拒绝。完整修复因此发布为主仓库 [草稿 PR #73](https://github.com/zhangzhe-ctrl/ani-resource-service/pull/73)，原 #71 未被改写。修复提交为 `d685a79373611a096bb6a9b4aef623b3ca5da532`。

已通过合并提交 `7e126f38fbf31b836ac8d2a11fe63a5bfad8014b` 同步 main `b7f58d4`，保留 #70 全部文档修订，并合并执行状态顶部的两边新增记录。远端合并树与“main 完整文件树加本 PR 的 24 个目标文件”逐项核对一致；与 main 无冲突。

[CI run 42601788722](https://github.com/zhangzhe-ctrl/ani-resource-service/actions/runs/42601788722) 在 GitHub 的 ubuntu-24.04 runner 上检出本 PR 的合并结果、按 go.mod 安装固定工具链。已观察到以下实际结果：

- [source job](https://github.com/zhangzhe-ctrl/ani-resource-service/actions/runs/42601788722/job/129107792918) 为 success。生成一致性、gofmt、普通 Go 单元测试、vet、build、mod verify 和 diff check 随 `make verify` 完成；日志包含 `internal/data/vm` 测试通过。此次业务源码为 `7e126f38`。
- Image job 为 success，但运行的是 `internal/data/image` 的 `imageintegration`，不覆盖本次 VM 镜像的 008 PostgreSQL 用例。
- Network LB job 为 failure；与原 #71 [run 42598505594](https://github.com/zhangzhe-ctrl/ani-resource-service/actions/runs/42598505594) 的 LB 日志逐项比较，五个顶层失败测试相同，包括旧 `target_generation` 列、attachment 约束和 egress 收敛用例。此处没有恢复旧字段、放宽约束或修改 Network 实现。
- supply-chain job 为 failure，两次运行均在 `Makefile:133: secrets` 失败，均报告 1017 个历史扫描命中；不是全仓门禁通过的结果。本次没有跳过扫描或修改扫描规则。
- Network core/recovery 在上述取证时仍在运行；本次不能记为完整 CI 通过。

本节及状态入口的后续提交只补充验证证据，业务源码与上述已验证提交一致。008 真实 PostgreSQL 迁移、原 key 重放及失败回滚仍为 not_verified，部署也未执行；PR 保持 draft。

## 已加入的回归覆盖

- 完整旧创建快照转换到 NONE 摘要；显式重复转换无变化。
- 新 NONE/NoCloud 声明不变；身份错误、损坏快照、错误旧摘要拒绝。
- 当前镜像已经改名/改描述时，旧请求仍由原创建快照迁移，重放返回原 image ID。
- 修改 capability 后仍产生幂等冲突，不生成第二个镜像。
- 非 CreateImage 请求及原记录除摘要外的所有字段不变。
- 隔离数据库中的中途失败整事务回滚；版本 8 不登记；修正测试夹具后可以重试。
- 现有迁移版本断言与实际集合 `1,2,3,4,6,7,8` 对齐；没有把未在本分支的 5 伪造为已执行。

在具备固定工具链与任务独占 PostgreSQL 的开发环境运行：

```bash
go test ./internal/data/vm -run '^TestImageCreateDigestMigration' -count=1
: "${VM_IMAGE_TEST_ADMIN_DSN:?provide an isolated PostgreSQL owned by this task}"
go test -tags=instanceintegration ./internal/data/vm \
  -run '^(TestVMImageCreateDigestMigration|TestVMImageMigrateIsRepeatableAndGrantsRuntimeAccess)' -count=1
make verify
```

不能省略 PostgreSQL 环境后将 skip 记作通过。此记录中的 not_verified 在实际执行之前保留。
