# Storage PVC 管理合同

本合同说明 Storage 直接管理的独立 PVC、使用方和删除结果，遵循[全域原则](res-responsibility-and-simplification.md)。业务 API/受信身份见[调用登记](governance-calls.md#存储)，部署权限见[Storage 开通说明](../../deployments/storage/README.md)；实际实现与验收只见[执行状态](../execution/status.md)。

## 对象、作用域与权威事实

Storage 直接创建、查询、扩容和删除受管 PVC；Kubernetes 的 spec/status/UID/resourceVersion 是原生事实，Res 保留产品输入、归属标签、原名称/UID、请求摘要和引用保护。PV 回收策略可在详情只读展示，未知即未知，不作 PVC 删除、扩容或业务终结的下游证明；不写 PV/CSI 或第三方 finalizer。

租户从受信委托取得，标准 Namespace、原 UID 和归属核验按[共享合同](../contracts/tenant-namespace.md)。Storage 不创建、补标签、收养或删除 Namespace，不接管 Instance/VM/Sandbox 的工作盘生命周期，也不因物理名称相同越过其他 owner 的引用。

## 创建与重放

CreateVolume 接受规范非零 UUID create_token、显示名称、正整数 capacity_gb、显式 StorageClass、volume_mode 和 access_modes。1 GB 为 1,000,000,000 bytes，不能把 GiB 混作 GB。默认 Filesystem/ReadWriteOnce；其他受支持模式按当前校验处理，零/负/溢出和非法组合拒绝。

原生名称为 `ani-vol-<去连字符的 create_token>`，显示名独立保存，不用用户显示名定位对象。创建前核验 Namespace 和 StorageClass；保存受管/租户/token 标签与规范请求摘要。已存在或并发 AlreadyExists 时只接受同一归属和请求摘要；异意图冲突，不能 PATCH 接管外部对象。正在删除的原对象返回其实际状态，不重置为新创建。

创建返回对象声明结果及当前状态，不承诺 PVC 已 Bound、底层存储已健康或工作负载可用；调用方依自己的消费合同处理后续事实。当前创建幂等依赖稳定名称和原对象内容，不声称存在跨对象永久操作账本。

## 查询和详情

ListVolumes 只读取当前可信租户的受管 PVC，先按名称/ID/phase 筛选、稳定排序后分页；total 与本次读取集合对应。名称按显示名不区分大小写包含，ID 精确匹配 PVC name 或 UID，state 对应原生 phase。读取失败返回错误，不伪造空集合。

GetVolume 返回原生申请量/已观察容量、conditions、事件和直接引用该 PVC 的 Pod/Notebook 使用方。容量未知与零不同。PV/StorageClass reclaim 策略的可选读取失败时标为未知，不把旧值伪装成新事实；不从事件或监控推导对象不存在。具体字段见生成 API，不复制下游控制器状态机。

## 扩容

UpdateVolume 只扩容，以 spec 当前申请量比较，不缩盘。检查当前授权、原 PVC UID、当前 resourceVersion 及 StorageClass 扩容能力，使用原生条件更新；同名替换、版本冲突和读取错误分别表达。spec 更新成功不表示文件系统或底层扩容已完成，观察容量按原生状态返回，不执行 guest、CSI 或节点补偿。

## 删除、引用与恢复

DeleteVolume 使用当前可信 tenant、保存位置及 expected_uid，先取得既有 BorrowedPVCGuard 的持久名称删除屏障，再调用 Kubernetes。名称键包含 cluster/namespace/PVC name，防止 Attach/Create 与旧名字删除竞争；不能以新 UID 绕过活动 borrowed 引用。

发送前检查自己可观察的直接 Pod/Notebook 使用方及管理引用；仍在使用时拒绝，查询失败不当作无人使用。不扫描历史全集、PV/CSI 或组件内部对象补作清场证明。

| 成功结果 State | 含义 |
| --- | --- |
| SUBMITTED | 原生 DELETE 已接受，原对象仍需继续观察 |
| TERMINATING | 原目标仍存在/正在退出，名称屏障与恢复责任保留 |
| GONE | 读取明确确认原 PVC absent（NotFound）时，按现有删除流程返回的成功结果 |

同名位置已经换成新 UID 时，前台 DeleteVolume 返回 ErrIdentityChanged，服务层映射为 AlreadyExists / VOLUME_IDENTITY_CHANGED，不返回成功 State。恢复流程可结束原 UID 的删除 ticket，但仍返回同一身份冲突；ticket 完成只结束旧目标的恢复责任，不代表公开调用删除成功，不接管、追删替换对象或改写原 target UID。依据见[deleteVolumeGuarded / advanceDelete / completeDelete](../../internal/biz/storage/delete_recovery.go)与[DeleteVolume / rpcError](../../internal/service/storage/service.go)。

未知出站写先持久标记，租约和恢复 ticket 不能因请求超时或进程退出自行清空。恢复沿原名称、UID、尝试/租约状态推进；明确读取错误继续等待，原对象退出后完成自己的删除责任，不等 PV/CSI handle 或介质擦除。

该流程复用现有删除 guard/recovery，不增加通用 operation/observer，也不把该直接 PVC 管理合同机械套给其他领域。正常原生 GC/finalizer 由组件处理，Res 不强拆、不修改 reclaimPolicy、不删除业务行伪造结果。

## 代码与文档边界

请求、类型和错误以[领域类型](../../internal/biz/storage/types.go)、[用例](../../internal/biz/storage/usecase.go)、[删除恢复](../../internal/biz/storage/delete_recovery.go)及[Kubernetes adapter](../../internal/data/storage/k8s.go)为实现定位。本合同不把源码存在当作部署或边界测试通过；文档更新不运行这些业务验证。
