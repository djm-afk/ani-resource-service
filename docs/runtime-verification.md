# Resource 运行验证

文档修改只核对 Markdown 链接、锚点及 Git 空白，不运行编译、业务单元或数据库测试。实现变更按实际风险选择以下入口，执行位置先按[当前环境规则](remote-execution.md)确认；不能将历史批次的 SSH 入口或授权套用于新任务。

## 源码与生成

    make tools
    make verify

固定工具版本以 [Makefile](../Makefile) 为准：Buf 1.60.0、sqlc 1.31.1；Proto Go/gRPC 插件按生成配置固定。make verify 检查再生成一致性、分层、格式、module tidy、普通测试、vet、build、module checksum 与 Git 空白。不要手改生成物或为修旧测试恢复已废止接口。

## 数据库、并发与隔离

    make integration
    make race
    make tenant-mutations
    make image-integration
    make image-tenant-mutations

数据库入口使用脚本创建的独占 PostgreSQL、独立 owner/runtime 角色和私有凭据；镜像、超时、分片及清理方式以 [integration](../scripts/integration)、[image-integration](../scripts/image-integration) 和 CI 配置为准。普通 go test 未配置对应 DSN 时会 skip 数据库用例，不能据此声称真实 PG 通过。各领域的 build tag 与定向入口按实际测试文件选择。

对 Network，验证原生结果映射、读错保留、通知/Finish 并发、未知 CREATE、原 UID 删除、管理引用及终态退休；不要求恢复 proof/Covers、TTL、生成物健康或物理清场。对其他领域，按自己的合法身份、业务引用、并发、输出交接和完成合同验证，不能机械套用 Network 的字段或执行器。

进程恢复与 lease 测试保留原数据库、在途意图、身份、幂等和未知写，不能重建空库冒充恢复。历史 a57b676/schema13 回退和旧客户端组合只说明对应版本范围，不能作为当前迁移后的可运行回退候选。每次数据库变更须核对实际版本/checksum 和可支持的恢复路径。

## 供应链与镜像

    make supply-chain-tools
    make audit
    make test-image

供应链脚本检查实际源码、依赖、秘密、notice、许可证据与 SBOM；工具版本以 Makefile 为准。SBOM 要求相应干净源码输入，synthetic source commit 只属于该扫描，不作为业务提交或发布；不能为文档链接修正触发与任务无关的供应链重跑。镜像构建与部署分别取得授权和记录。

## 结果记录

STATIC、普通测试、真实 PostgreSQL、受控 Provider、真实集群、来宾及浏览器分别记录 pass/fail/not_verified。只写实际执行命令和对应源码；计划、fixture、构建或 Ready 都不替代真实业务验收。最新结果只在[执行状态](execution/status.md)维护。

以下保存旧工作包的复现入口和结果范围。原批次的关系索引、完整审计、Covers、TTL 或生成物验收要求已由[网络 v3](specs/cr-observation.md)替代；这些历史命令不自动成为本次必跑门禁或环境授权。

## 历史 NET-02–04 双仓库接口验证

`scripts/remote-pair -- <argv>` 从固定 ANI 专用 worktree 与 Network 工作树建立各自源清单和归档，在 ubuntu 新建目录；不包含 ANI 主 checkout 改动或兄弟运行模块。`scripts/integration-pair` 创建专属 PG 容器，ANI 与 Network 使用不同数据库/owner/runtime。环境变量 `NET0204_TEST_ADMIN_DSN` 只由该入口设置，专项测试没有 live DSN fallback。

```bash
scripts/remote-pair -- bash -c '"$NETWORK_SOURCE/scripts/integration-pair" "$NETWORK_SOURCE/scripts/test-net0204-wire"'
```

该入口构建独立 Network 服务、实际 Gateway 路由测试进程与 ANI 实例 owner 测试进程。HTTP fixture 只提供固定 kc/Kubernetes 契约和可控故障事实；使用真实 client、renderer、持久实例受理/封闭、gRPC consumer 查询与数据库恢复，不构成真实 kc 控制器、OVN 或 IAM 认证证明。具体故障场景、执行结果与源码身份见 [组合记录](execution/records/NET-02-04-implementation.md)。

## 历史 NET-05A 持续观察与真实复验

单仓重任务入口为 `scripts/net05a-remote -- scripts/net05a-gates`，从专用 dirty worktree 建立逐文件校验快照。`scripts/net05a-stage` 提供开发期间定向生成与真实 PG 测试；不能替代完整门禁。观察受控用例位于 [kc_observation_integration_test.go](../internal/data/network/kc_observation_integration_test.go)，通知/公平性位于 [observation_integration_test.go](../internal/data/network/observation_integration_test.go)。协议 fixture 使用真实 dynamic client 的 HTTP List/Watch，仅证明受控层。

固定容量入口为 `scripts/net05a-remote -- scripts/net05a-capacity --matrix --verify-first`。同一进程串行执行旧/新版本、100/1000/2000 Attachment、1/2 副本和五个固定阶段；每组独立真实 PG。条件与随机种子见 [容量合同](execution/records/NET-05A/capacity-contract.json)。导出后通过 `scripts/net05a-capacity-report <run-directory>` 生成判定，保留基线失败、丢失/截尾样本和未完成组；Go 测量程序 exit=0 不等于性能通过。

本轮用户允许资源限制下的大负载延期后，补验使用 `scripts/net05a-remote -- scripts/net05a-capacity --small-matrix --variants net05a`，只重测固定的 100 Attachment / 1、2 副本。报告的 `--baseline-from <prior-run-directory>` 仅引用旧版已完成组，保留各组源目录和摘要；不能据此将全矩阵写为通过。

真实链路使用 `scripts/net05a-pair -- python3 -B network/scripts/net05a-live.py build` 建立精确双仓隔离构建。后续对同一 `NET05A_RUN_DIR` 依次运行 `positive`、`setup-faults`、`queries`、`transactions`、`lease`、`unknown`、`provider`、`owner`、`watch`、`rollback`、`product-cleanup`、`gates`、`export`、`fixture-cleanup`。每阶段先检查已保存身份与结果，不盲目重放失败写入；新源码快照沿用旧 run 时仍须保留实际构建/运行身份映射。NET-05A wrapper 核对并适配固定 NET-05 runner，原脚本保留为历史输入。

本包重要修复后的精确新构建可用 `upgrade` 保留原失败意图并验证恢复，再用 `NET05A_WAVE=<unique-wave>` 的 `recheck-products` 经产品 API 清理并创建新普通容器波次。两阶段有已启动检查点，失败后先检查记录；不会自动重放。`NET05A_WAVE` 同时隔离后续故障用例的永久幂等键，测试暂停点必须使用同一源码构建。

原批次 V-09 曾检查后台暂停、查询副作用及旧证据过期拒绝；其中 proof/TTL 准入要求已由网络 v3 废止，不再作为当前验收条件。正常 Network 使用新镜像；明确标记的测试构建只暂停本包进程。真实 Watch、真实 PG、普通容器数据面、受控 403/429/410 和容量证据各自分列。ANI 固定源码不修改；既有八条 compatibility 与全历史 Atlas 失败不能由 schema fixture 改写。详细源身份、实际命令和结果见 [NET-05A 记录](execution/records/NET-05A-implementation.md)。
