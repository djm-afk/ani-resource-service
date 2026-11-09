# Resource 运行契约

本文说明单进程装配、配置与运行边界；业务行为按[全域原则](specs/res-responsibility-and-simplification.md)及各领域规格，实际实现、验证和部署只见[执行状态](execution/status.md)。

## 装配与迁移

cmd/ani-resource-service 是唯一组合根。Network、Compute、Image、Storage、Instance、Sandbox、VM 和平台集群运维 ClusterOps 按各自启用条件装配，保留各自的持久化与执行机制，不引入统一资源状态机。默认/full、governance、vpc-read 使用同一业务 gRPC 安全构造，受信身份、方法清单与委托见[调用合同](specs/governance-calls.md)。vpc-read 只装配规定的只读入口，不运行网络创建 worker。

Network 使用独立 migration owner 与受限 runtime role；runtime 不拥有表，不具有 superuser、BYPASSRLS、CREATEDB、CREATEROLE 或 DDL 权限，不可切换为 owner。迁移使用明确的 owner 入口，正常启动仅检查目标 schema/checksum/角色，不隐式迁移。各领域的迁移入口与版本分别由源码定义；历史 schema4 或 schema13 不能当作当前启动/回退依据。

构建后在获准的环境显式迁移和启动，连接串和角色由凭据管理方式注入：

    ./bin/ani-resource-service -migrate
    ./bin/ani-resource-service -conf ./configs

## 配置与身份

类型化配置的字段定义为[conf.proto](../internal/conf/v1/conf.proto)，示例见[config.yaml](../configs/config.yaml)，校验规则见[validate.go](../internal/conf/v1/validate.go)。Kratos 使用既有 ANI 环境变量映射；时长采用 Protobuf JSON 秒格式。私有 DSN、证书、签名及加密材料只记录其受管文件位置。

| Network 输入 | 责任 |
| --- | --- |
| ANI_NETWORK_DATABASE_DSN | runtime PostgreSQL 连接 |
| ANI_NETWORK_CURSOR_SIGNING_KEY | 稳定共享的游标签名材料 |
| ANI_NETWORK_KUBECONFIG | 显式 kubeconfig 路径；空值使用 in-cluster 凭据 |
| ANI_NETWORK_CLUSTER_ID | 本服务配置的稳定集群范围，与已登记映射一致 |
| ANI_NETWORK_INSTANCE_CONSUMER_ENDPOINT | 已配置的消费者查询入口；读取错误保留管理占用 |
| ANI_NETWORK_CLIENT_CA/TLS_CERT/TLS_KEY | 业务 mTLS 必需材料，缺少或身份不符时拒绝启动/调用 |
| 旧 namespace prefix | 已移除；旧非空配置拒绝启动，标准名称由[租户 Namespace 合同](contracts/tenant-namespace.md)定义 |

Sandbox 的租户空间开通在原生名称不存在时持久受理创建并核验原 UID；其他 owner 复用该标准 Namespace，不创建、补标签或收养。执行与清理使用已登记的原名称/UID，不能因同名替换取得权限。Namespace、Secret、Pod 与各领域数据身份分别管理。

Image 的 Registry、Secret writer 与可信运行身份见[Image API](specs/image-api.md)；Compute 受管配置见[算力合同](specs/compute-presets.md)；Storage、Instance、Sandbox 和 VM 的额外装配输入从[文档索引](START-HERE.md)进入对应合同，不从 Network 环境变量推导。

## Network 观察、调度与声明

当前行为唯一见[网络状态消费规格](specs/cr-observation.md)：

- 普通 client-go informer 只提供精确 namespace/name 通知；现有任务行保存通知代次与 lease/epoch，执行者 GET 自己处理的原生目标。
- Ready、phase、Initialized 或对应原生配置回执按对象合同使用，不组合父网络健康、节点在线、镜像或生成工作负载再裁决状态。
- 读取失败保留最后成功的状态、原因、地址、applied 和 observed_at；记录读取错误并按既有策略退避。observed_at 不作 TTL 准入，observation_stale 表达读错或尚无成功观察。
- 稳定任务停放；未完成操作与读错重试仍有持久排程。通知在 Finish 校验后消费，Claim 后新通知不丢失；已结束 Deleted/Released 任务退休，旧事件不复活。
- Attachment 采用消费者当前管理事实，地址展示独立；owner 完成自己的关闭责任后 closed，Network 释放管理关系，不重新证明 KCN 子资源清场。
- 对自己声明的对象保留 UID/resourceVersion 写入前提。删除只结束原身份，不删除同名替换对象；未知 CREATE 沿同一次意图与名称恢复。

保留 request QPS/burst、workers_per_kind、worker 并发、lease/request timeout、重试与未完成工作排程。旧 audit_interval/audit_jitter/audit_timeout/flush_interval/queue_capacity、Worker.stale_after 及 LB installation_fingerprint/镜像期望字段已 deprecated，可解析但运行时忽略；不再提供这些字段作为有效配置示例。显式安装诊断不成为运行准入。

## 生命周期、健康与遥测

应用启动实际监听器、已启用领域的执行器与数据库健康检查；请求方退出不取消已持久受理的业务责任。停止时取消外部调用，等待组件退出，未完成任务按原持久状态恢复。故障、租约和观察方式按领域合同处理，不强制所有模块拥有 worker。

| 接口 | 含义 |
| --- | --- |
| GET /healthz | admin 进程可响应 |
| GET /readyz | 当前装配的应用、执行器和数据库检查通过 |
| 标准 gRPC health | 沿用服务自身健康含义；业务调用允许清单另行约束 |
| GET /metrics | 提供本地指标，不等于 scrape、告警或流量已验收 |

Provider 读错不等同于服务无法持久受理；资源状态、operation 结果、观察错误及服务健康分别表达。遥测记录请求、错误、通知重试、排程与数据库运行情况，不产生 provider proof、审计续鲜或物理回收证书。日志保持脱敏和 trace 关联；租户/资源 ID 不作为无界 metrics label。

## 检查与发布

文档修改只检查链接和格式。涉及实现时按实际改动运行[运行验证](runtime-verification.md)中的必要检查，在当前任务确认的环境记录源码、命令与结果；不从历史远程入口取得执行授权。生成镜像、推送、部署及业务验收分别记录，不能以 Ready 或构建成功替代真实业务通过。
