# Network 运行契约

本文接替初始化时的通用运行骨架说明，描述 Network 与 NET-05A 持续观察的实际装配。业务规则以 [VPC/Subnet 规格](specs/vpc-subnet.md) 为准，实际检查结果以 [执行状态](execution/status.md) 为准。

## 启动和迁移

`cmd/ani-resource-service` 是唯一组合根。正常启动装配受限 PostgreSQL、实际 kc adapter、共享观察器、Network 用例、持久 worker、gRPC 和 admin HTTP；不存在 fake、内存数据库或旧 ANI 后端的运行配置开关。

先为专用的空 Network 数据库准备不同的 migration owner 和 runtime login role。owner 拥有数据库与 schema；runtime 不拥有表，不具有 superuser、BYPASSRLS、CREATEDB、CREATEROLE、持久或临时 DDL 权限，也不能 SET ROLE 为 owner/管理角色。迁移会撤销 PUBLIC 的数据库 CREATE/TEMP 和 public schema CREATE，给 runtime 授予具体表权限；不会创建 IAM Tenant 表、启用 RLS 或访问其他服务的数据库。

构建后显式迁移：

```bash
# 以下变量由本次环境的凭据管理方式注入，不把连接串写进源码或日志。
# ANI_NETWORK_MIGRATION_DSN：migration owner 的连接串
# ANI_NETWORK_RUNTIME_ROLE：事先创建的受限 login role 名
./bin/ani-resource-service -migrate
```

迁移入口拒绝包含其他 public 业务表的数据库及未经版本管理的 Network 表，核对已应用 migration checksum，支持相同 migration 的重放。正常启动只检查权限、schema 版本和 checksum，不读取 owner 变量或隐式执行迁移。

正常启动需要以下环境输入：

| 变量 | 含义 |
|---|---|
| `ANI_NETWORK_DATABASE_DSN` | runtime 角色的 PostgreSQL 连接串，必填 |
| `ANI_NETWORK_CURSOR_SIGNING_KEY` | 32–128 字节 secret 的标准 Base64，必填；同一服务的副本共享，重启保持稳定 |
| `ANI_NETWORK_KUBECONFIG` | 管理员提供的 kc 集群 kubeconfig 路径；空值使用 Kubernetes in-cluster 凭据 |
| `ANI_NETWORK_CLUSTER_ID` | 固定的内部集群标识，默认 `primary`；须与已保存映射一致 |
| `ANI_NETWORK_INSTANCE_CONSUMER_ENDPOINT` | ANI 实例 owner 的 InstanceNetworkConsumer gRPC 地址；未配置/不可用时保留 Attachment 占用，不猜测封闭 |
| 原 Namespace prefix 配置 | 已移除；非空旧字段或环境变量启动失败，名称固定为 `tenant-<canonical resource UUID>` |

```bash
./bin/ani-resource-service -conf ./configs
```

Namespace 统一合同与维护窗口回填流程见[正式合同](contracts/tenant-namespace.md)。标准 Namespace 必须预先存在；Resource 各 owner 只精确 GET 并核对 network 两项归属标签与原 UID，不创建、补标签或收养。产品 ID 与 Provider 位置、对象名、UID 的映射在受理时持久化；正常删除继续保留 provider UID/resourceVersion 护栏，不移除 kc finalizer。kc 权限仅需 Namespace GET，以及原 VPC/Subnet/Pod/VNic/VNicIP/EIP 所需的精确操作和观察权限。

## 类型化配置

配置由 [conf.proto](../internal/conf/v1/conf.proto) 生成，示例在 [config.yaml](../configs/config.yaml)。Kratos 按 `ANI` 前缀加载环境变量。Duration 使用 Protobuf JSON 秒格式，例如 `0.1s`、`20s`，不能写 `100ms` 或 `1m`。

监听器默认 `127.0.0.1:19090`（gRPC）和 `127.0.0.1:19091`（admin），允许用已有 `ANI_SERVER_*` 变量设置 loopback 或 unspecified IP；两个端口必须不同且非零。服务间调用验证按 [ADR-0003](adr/0003-defer-workload-authentication.md) 延期，当前 tenant 输入不代表认证；环境应限制直接访问范围。

worker 默认：lease 20s、单次 Provider 调用 5s、观察 10s、观测有效期 60s、重试 1–60s、空闲轮询 0.1s。可通过 `ANI_WORKER_LEASE`、`REQUEST_TIMEOUT`、`OBSERVE_EVERY`、`STALE_AFTER`、`RETRY_MIN`、`RETRY_MAX`、`POLL_INTERVAL`（均加 `ANI_WORKER_` 前缀）设置。验证要求 lease 至少为单次调用的三倍、有效期大于观察间隔、退避上下界有序；每次 Step 的总期限也受 lease 限制。具体上下界由 [配置验证](../internal/conf/v1/validate.go) 实现。

观察器默认共享六类资源来源：VPC、Subnet、Pod、VNic、VNicIP、EIP；使用独立完整分页审计，不用 informer 同步标记续鲜。配置环境变量统一加 `ANI_OBSERVATION_` 前缀：`AUDIT_INTERVAL=30s`、`AUDIT_JITTER=2s`、`AUDIT_TIMEOUT=10s`、`FLUSH_INTERVAL=0.1s`、`QUEUE_CAPACITY=4096`、`WORKERS_PER_KIND=2`、`REQUEST_QPS=5`、`REQUEST_BURST=10`。最后两项限制短请求；Watch 连接配置不继承短请求 timeout。容量实验对旧/新版本都显式使用每副本 QPS=40、burst=80，不能将该容量结论套用到默认预算。

10 秒 worker deadline 继续承担持久恢复和 owner 查询；稳定事实来自共享审计，普通已知资源缺少可用证明时直接 GET。事件提示进入有界合并队列，再按已登记 binding/关系唤醒 PostgreSQL。内存溢出、持久化失败和来源重连使旧证明失效，启动清点及持久 deadline 补偿丢失提示。事实顺序、代次与释放约束的权威定义见 [观察规格](specs/cr-observation.md)，实现说明见 [NET-05A 记录](execution/records/NET-05A-implementation.md)。

关键删除/释放加入已有完整采集时，可能需要等它结束后再采集一次，才能覆盖本次调用的时间下限；等待过程保持这个下限，不在每次竞争后重新 Claim 抬高它。每次采集独立保存结果和错误，调用取消及审计超时仍生效，旧采集不能因稍晚返回而变新。时效预算须计入连续两次完整采集。

schema 4 仅新增通知代次、退避下限、证据及索引。升级须停止旧执行者并用 owner 显式迁移；正常启动不迁移。固定 NET-05 二进制仅接受旧 schema，升级后无法直接回退启动；不要降级 schema、清空意图或绕过版本检查。

## 进程生命周期与健康

Kratos 同时启动 gRPC、admin、共享观察与 worker。请求方退出不终止持久任务；worker 的内存状态仅用于运行，不是待执行队列。Subnet 沿用 VPC 的持久 T1–T4 worker；Attachment 使用自己的 next_check/lease/epoch 和历史，由同一 WorkerServer 的独立执行通道调度。资源通道保留 VPC/Subnet 最早到期领取，Attachment 按父 Subnet 和到期顺序领取；两通道各有 1–4 个 worker，默认各 2 个，慢 owner RPC 不占满资源通道。Attachment worker 对四种状态持续核验，实例 owner 查询不可用和外部结果未知都保留可恢复占用。停止时取消外部调用、等待 worker 和监听器退出，未完成记录留在数据库，之后由新的租约持有者恢复。

| 接口 | 实际含义 |
|---|---|
| `GET /healthz` | admin 进程可以响应 |
| `GET /readyz` | 应用已启动、worker 正在运行、数据库/schema/角色检查通过且探测记录未过期 |
| 标准 gRPC health Check/Watch | 与 readiness 相同；服务名支持空串和 `network.v1.NetworkService` |
| `GET /metrics` | 本地 Prometheus registry；不代表实际 scrape、告警或远端 tracing 导出已配置 |

数据库每秒独立检查，单次探测最多 0.5s，超过 3s 的健康缓存失效。Provider 暂时失联不会单独关闭可持久受理和查询的服务；资源/operation reason 和 `ani_network_provider_reachable` 呈现观测结果。worker 异常返回或 panic 会使 readiness 失败并导致应用退出，不会继续假报健康。GET/LIST/GetOperation 只读数据库，`observation_stale` 在查询时纯计算。

日志保留 Kratos JSON、service 身份、source 和 trace/span；worker 日志增加资源、operation、epoch、状态和稳定 reason。tenant/resource ID 只用于日志，不作为 metrics label。新增 `ani_network_worker_attempts` 按封闭结果分类，Provider gauge 表示最近一次观测；尚未观测时没有成功证据。指标不能代替数据库历史。`ani_network_observation_*` 另行报告来源同步/错误/重连边界、内存提示和持久队列年龄、完整审计时长/失败/证据年龄、各封闭 verb/GVR 请求及字节、已应用证据年龄与 PG 事务/UPDATE/WAL。Watch 与 audit 索引字节是 JSON payload 估计，不是进程 RSS。观察连接或审计健康不能替代 worker 应用进度、数据库健康或产品可用性；这些指标不作为 API readiness 的替身。

保留原中间件顺序：recovery → metadata → tracing → logging → metrics → validation。gRPC reflection 关闭。一个进程只装配一个 Kratos app，应用拥有全局 OTel provider 的初始化和关闭。

## 验证边界

运行入口和命令见 [运行验证](runtime-verification.md)。受控 HTTP/API server 验证实际适配器请求，真实 PostgreSQL 验证持久事务，独立进程验证恢复；它们都不能证明真实 kc、OVN、kind、Pod/VM 或 IAM 已验收。

## Governance VPC 只读入口

所有服务运行模式的内部业务 gRPC 均要求 [Governance → Resource 调用登记](specs/governance-calls.md) 中的 mTLS 配置；默认/full 不再提供明文入口。`ANI_NETWORK_MODE=vpc-read` 使用真实 PostgreSQL 查询与相同安全构造器，装配边界、环境变量及只允许 GetVPC 的合同见 [只读接入规格](specs/governance-vpc-read.md)。它不运行 KC、worker 或云网络创建。结构仍由 `-migrate` 使用独立 owner 显式执行；运行角色只能读取本片数据，不因服务启动迁移或播种。
