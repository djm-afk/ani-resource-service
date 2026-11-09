# Intranet 地址池配置与运行边界

业务规则见[基础连接合同](../../docs/specs/vpc-connectivity-lb.md)和[网络状态消费](../../docs/specs/cr-observation.md)，实际发布与验收只见[执行状态](../../docs/execution/status.md)。

## 管理配置

BOSS 显式通过 GetIntranetPoolDefaults 读取 kcn-system 默认 VPC 的公开身份；intranet_networks 是可选管理员记录，不扫描 ConfigMap、ServiceCIDR、控制器启动参数或节点来推导完整拓扑。管理员准备真实 DNS/xDS/Service 目标与路由；记录本身不成为 runtime 的完整性证明。

CreateIntranetAddressPool 保存 scope=intranet、CIDR、网关、保留地址及产品引用，在平台 Namespace 声明自己的 Subnet(type=Intranet)。不创建 Public EIPGateway 假冒内网出口，不修改默认 VPC、共享 kcn-config 或集群路由。原声明身份、用户配置和 CIDR/引用冲突由 Res 保留。

## 原生结果与分配开关

池与网关的运行结果使用自己对象的 Ready，基础 EIP/SNAT 分别消费原生结果；不合取 Valid/Initialized、observedGeneration、四种组件角色/镜像、Node/CNI 或路由覆盖来再判可用。读取错误保留最后成功状态和时间，observed_at 不用于 TTL。

人工 RecordIntranetPoolVerification 只归档真实材料，不触发 provider 操作、替换当前 operation 或成为开放分配/创建 VPC 的门槛。SetIntranetPoolAutoAllocationEnabled 按 expected_version/幂等原子保存默认池、分配及新 VPC 开关；关闭只影响后续首次受理，既有引用与冻结意图继续。

## 权限、删除与诊断

运行权限以当前 [Network RBAC](network-rbac.yaml) 所需的自己声明、精确读取和管理配置写入为准，不因旧手册要求而恢复 ConfigMap/生成工作负载等审计权限。池删除仍受 Res 自己未释放的 EIP/产品引用保护；引用收尾后只删除原声明，不等物理地址或组件内部清场。

平台 DNS/xDS、真实内网/Public/LB 流量属于显式部署/诊断验收，各自保存实际版本与结果。历史 NET-U02 没有发布或曾失败的记录保留原时间；此说明不提供环境入口或新部署授权。
