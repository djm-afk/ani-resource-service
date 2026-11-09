# 执行环境、源码隔离与历史约定

当前环境选择以用户本次指令和个人环境清单为准，工程规则见 [AGENTS](../AGENTS.md)。本文不提供当前 SSH 配置、用户名、集群或回退授权；下方历史任务参数仅供追溯，不能自动沿用。

## 当前任务的执行边界

- 本地可进行源码读取、编辑、Git 差异及轻量文档检查。文档修改不运行编译、业务测试或数据库。
- 实现验证前确认工具执行主机、源码主机、构建/测试主机与部署目标；连接入口缺项或冲突时先说明具体缺项，不猜别名、用户名或 context。
- 远程操作先用只读命令核对身份。单个入口失败不证明整个目标不可用，也不自动允许换环境；回退依据当前任务授权。
- 环境记录只提供连接信息，不授权共享设施变更、发布或破坏性操作。

## 源码、工具和产物

使用任务独占目录及明确源码清单，不覆盖其他工作树。快照包含本次实际未提交输入，排除 .git、凭据、kubeconfig、私有配置、缓存和无关产物；记录 HEAD、dirty 差异、文件清单及 SHA-256，单独 HEAD 不能标识验证输入。

工具版本以当前 Makefile 和生成配置为准。需要 Git 上下文的独立验证仓库只用于快照检查，临时提交不发布。并发、CPU/内存和共享设施限额按实际执行环境约定，不从历史机器配置推导。

集成测试只使用本任务独占数据库、角色和容器，显式区分 migration owner 与 runtime；不复用业务数据库。仅清理本任务创建的设施，保留共享缓存和其他任务资源。源码与结果回传先到临时位置，核对期间漂移后再应用，不能覆盖本地新增修改。

在现有执行记录中分别登记来源、实际主机、命令、退出码、验证层次与清理结果。构建、镜像推送、部署和业务验收是不同事实。个人连接清单不复制到共享仓库。

## 历史任务约定

2026-09-09 的 ubuntu 入口、工具环境与准备结果见[原远程记录](execution/records/2026-09-09-remote-readiness.md)。后续 NET-05A、VPC SNAT、Resource R0–R5 使用各自当时确认的入口和限额；以下原文中的“本轮”仅指其历史工作包。

## NET-05A 隔离流水线

NET-05A 使用 `scripts/net05a-remote`（单仓）和 `scripts/net05a-pair`（固定 Network/ANI 输入），远端目录为 `net05a-<run-id>`。两者共享本包重任务 flock；实际资源预算与停止记录见 [NET-05A 实施记录](execution/records/NET-05A-implementation.md)。当前采用 GOMAXPROCS=1、GOFLAGS=-p=1、CPUQuota=100%、MemoryMax=2300MiB、MemorySwapMax=0，PG 单独 768MiB/1CPU；一次一条重流水线。本地不运行容量压力。

ANI 完整固定 manifest 用于检查身份，传输时排除 `.claude/settings.local.json`；Git 对象包也不能包含其私有 blob。各快照排除凭据、kubeconfig、私有配置、缓存与本包原始验收输出，排除项随 manifest 记录。生成物由 `scripts/net05a-return-generated` 回传临时位置，逐项审查后 `--apply`，并检查本地期间是否漂移。

固定 kind、kc、节点和已有工作负载只读核验；故障只施加到本 run 的进程、数据库、对象或受限代理。真实业务先经产品 API 释放清理，再撤销本 run 的数据库/角色、进程、RBAC、配置与空 namespace。工具、共享镜像缓存、kind/CNI 和其他任务资源保留；完整恢复与清理证据不能以删除数据库代替。

## VPC SNAT 本轮覆盖约定

本轮 Goal 要求重任务必须在 ubuntu 串行执行，远端不可用时仅继续本地编辑/静态检查，不允许自动回退本地重任务。`scripts/snat-remote` 创建任务独占快照目录，沿用共享重任务 flock，GOMAXPROCS=2、GOFLAGS=-p=2、CPUQuota=200%、MemoryMax=2300M，日志及精确清单保留在 `.work/snat-runs/` 并在正式记录归档。临时 Git 索引仅用于静态门禁，不创建提交。生成物由 `scripts/snat-return-generated` 先回传审查，再检查期间本地哈希后应用。

2026-09-11 本仓分支提交推送另获用户授权。发布时 `scripts/snat-remote --publication` 传递完整显式暂存树，包含本次发布的历史/新增证据，核对 index blob 与文件模式；独占远端目录内建立仅供 SBOM/供应链验证的临时提交。普通实施模式保持原有排除项及不创建验证提交的行为。真实分支与 exact-SHA CI 见 [发布记录](execution/records/VPC-SNAT-PUBLICATION-20260911/README.md)。

## Resource 改名任务：Fedora 强制执行

2026-09-22 用户为本次 R0—R5 指定 `ssh fedora`。本地仅编辑、静态检查、Git 和转运；
依赖下载、生成、构建、测试、PG、镜像和 API 驱动全部在 Fedora 执行，不自动回退本地。
任务根目录为 `/home/chabking/workspace/ani-resource-service-runs/rsmod-20260921T1640Z`；
baseline/candidate 分离，共用本 run 独立缓存、相同工具链与 UTC 环境。
继续使用原 `/home/chabking/workspace/ani-network-service-runs/net05a-heavy.lock`，不能因
改名创建第二把锁。systemd CPUQuota=200%、MemoryMax=2300M、MemorySwapMax=0；
GOMAXPROCS=2、GOFLAGS=-p=2、GOMEMLIMIT=1536MiB、GOTOOLCHAIN=local、GOWORK=off。
PG 单实例 768 MiB/1 CPU、随机 loopback 端口；保留 INTEGRATION_TIMEOUT=20m。

源码以完整文件清单及 SHA-256 核验，保留全历史 Git bundle 用于供应链扫描；凭据、kubeconfig
和私有配置不进入源码归档或公开证据。集群写入前重新冻结当前身份/权限/fingerprint，
对照保护清单；只操作本 run 资源。历史 Ubuntu 运行器仍保留原环境路径，不能将旧记录
改成 Fedora 已执行。实际命令/产物/退出码见[本次记录](execution/records/RESOURCE-MOD-20260922/README.md)。
