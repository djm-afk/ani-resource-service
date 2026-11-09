# ani-resource-service

Module: `github.com/zhangzhe-ctrl/ani-resource-service`

This repository was generated from ANI's pinned Kratos layout. It is an
independent source snapshot: builds and runtime do not require the layout.

本仓库在原 Network 服务完整历史上演进为 Resource 服务，一个 Go module、一个进程。
现有能力包括 Network、Compute、Image、Storage、Instance、Sandbox、VM，
以及平台集群运维 ClusterOps，
按 internal/{biz,data,service} 分层，由 cmd/ani-resource-service 装配。
各领域的职责边界见[全域原则](docs/specs/res-responsibility-and-simplification.md)，
服务演进的历史决定见 [ADR-0006](docs/adr/0006-evolve-resource-service-preserving-network.md)。

从 [文档导航](docs/START-HERE.md) 开始阅读；其中链接当前规格、领域词汇、
设计决定、实施计划和唯一执行状态。设计目标与实际实现/验证结果分别记录。

`THIRD_PARTY_NOTICES.go-kratos-layout.txt` preserves the upstream template's
MIT notice. This generated repository intentionally has no project `LICENSE`;
its owner must make that choice before publication.

## 开发与运行

```bash
make tools
make verify
make integration
make race
make tenant-mutations
```

生产镜像使用与 ACC、ModelDev 相同的命令接口，在当前任务确认的构建环境执行：

```bash
make image IMAGE=registry/project/ani-resource-service:tag VERSION=version
make test-image
```

`PLATFORM` 默认 `linux/amd64`，`TARGET=service`；可显式覆盖 `BUILDER_IMAGE`、
`RUNTIME_IMAGE` 和 `GOPROXY`。构建只编译 `cmd/ani-resource-service`，带齐各领域嵌入的
SQL migration，不在构建或启动时执行迁移。Docker 使用独立白名单源码快照，排除配置、
凭据、测试、Git 和缓存。`.work/images/image-*/result.json` 记录源码 commit、dirty 状态、
实际输入摘要及本地镜像 digest；命令仅构建并加载本地镜像。

镜像统一入口 `/app/bin/server`，默认参数 `-conf /app/configs`，用户 `10001:10001`，
带系统 CA；兼容现有 `/ani-resource-service` 和 `/app/bin/ani-resource-service` 命令路径。
运行配置和 secret 由环境挂载，线上自定义 command/args 可继续使用。
ani-deploy 页面中的 `make verify` 可独立勾选，开启后执行本仓原始质量检查，失败即停止发布。

Image 的真实数据库、生命周期和租户隔离另用 `make image-integration`、
`make image-tenant-mutations`；需设置本次检查独占的短路径 `TMPDIR`。
完整入口和 CI 检查范围见 [运行验证](docs/runtime-verification.md)。

执行环境按当前任务和个人环境清单确认；[远程约定](docs/remote-execution.md)中的历史任务入口不自动适用于新任务。
正常启动需要专用 PostgreSQL 的 runtime 连接串、游标签名 secret 和 kc 集群凭据；
迁移使用独立 owner，通过显式 `-migrate` 入口执行。
具体变量、权限、启动命令和健康含义见 [运行说明](docs/runtime.md)。

`network.v1` 提供网络资源管理和消费者 Attachment 协议。
Network 沿已有持久任务推进，普通 informer 提供精确通知，执行者读取自己的原生目标；
状态、管理关系与地址分别处理，合同见[网络状态消费](docs/specs/cr-observation.md)。
Governance 使用受信 mTLS 委托，实例和 VM owner 管理自己的运行对象并发布当前消费者事实。
当前实施结果和证据只在 [执行状态](docs/execution/status.md) 维护。

已有 Kratos 日志、中间件、trace、metrics 和优雅停机门禁继续保留。
readiness 现在包含 worker 与真实数据库状态；实际 kc adapter 是正常运行的唯一 Provider。

`make audit` 保留漏洞、Git secret、notice、许可证据和 SBOM 门禁。
生成 SBOM 需要干净的已提交源码；具体范围和输入要求见 [运行验证](docs/runtime-verification.md)。
没有执行真实 kc 部署、数据面验收或发布，就不能据这些检查宣称网络可用。
