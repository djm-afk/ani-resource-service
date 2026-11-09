# LB 配置与操作边界

产品行为以[LB 合同](../../docs/specs/vpc-connectivity-lb.md)、[延迟激活](../../docs/specs/lb-deferred-activation.md)和[网络状态消费](../../docs/specs/cr-observation.md)为准。本文说明部署与产品操作入口，不维护第二套运行验收合同。当前环境和共享变更按用户本次任务确认，不沿用历史 ubuntu/fedora、10–12 或旧 context。

## 部署支持条件

首次部署准备所需 Gateway/Backend/Policy API、公共 lb-common GatewayClass/Proxy、可信证书引用与最小 RBAC。公共对象由平台负责，单 LB worker 不安装或覆盖共享组件；自己专属的 Proxy、Gateway、Backend、Route、Policy 使用租户 Namespace 中的精确授权。

安装诊断可通过[lb-api 工具](../../scripts/lb-api)显式只读运行，使用本次核实的 kubeconfig、context 和集群身份保存实际结果。诊断里的 CRD、镜像或安装摘要是来源材料，不作为每次激活、更新和删除的 runtime fingerprint。不能把历史安装结果当作当前支持证据。

## 运行配置

当前配置只开放既有能力开关，示例须加入本服务完整有效配置：

    network:
      load_balancer:
        enable_isolated_api: true

旧 installation_fingerprint、controller_image_id、envoy_image_id、shutdown_image_id 和 kc_image_id 字段可解析但已废弃、运行时忽略；不再要求填写四角色 imageID 或通过共享审计续签。实际配置字段以[conf.proto](../../internal/conf/v1/conf.proto)为准。

[Network RBAC](network-rbac.yaml)只按当前自己声明及必要管理读取配置；旧手册不授权恢复生成 Service/Deployment/EndpointSlice、Node 或镜像链路的证明权限。具体安装/共享权限变更须独立执行，不因编写配置就应用到集群。

## 产品操作顺序

1. 经正常 Gov 鉴权创建 LB，校验 VPC/Subnet 的产品归属、管理状态与地址/EIP 独占，不追加 Provider Ready、观测年龄或基础 Intranet 前置。
2. 空后端保存配置与预留；添加后端后下发本服务自己的组件，按原生公开配置回执推进。
3. 使用 expected_version、稳定幂等键和完整可写监听配置更新，不把部署状态或 reason 回传为输入。
4. 按实际运行投影区分后端减少、单监听器清空和最后运行监听器清空，依下表处理，不把局部更新扩大成整个 LB 下线。
5. 显式产品删除完成自己声明的原 UID、pending 和引用责任后释放管理占用，不等生成工作负载、EIP/VIP 物理回收。

| 后端变更 | 本服务操作范围 |
| --- | --- |
| 仅减少部分 Backend，相关监听器仍有合法运行投影 | 更新受影响配置，保留仍需要的 Route、Gateway 和专属 Proxy；不再引用的 Backend 在相应配置回执与引用责任完成后清理 |
| 清空某个监听器后端，其他监听器仍可运行 | 仅撤不再需要的相关 Route/Policy，更新并保留共享 Gateway，清理不再引用的 Backend；其他监听器及专属 Proxy 保留 |
| 最后一个可下发监听器被清空 | 才按 Route/Policy → Gateway → Backend/专属 Proxy 的既有顺序撤除整套运行声明；自己的声明、pending 和引用责任完成后进入待配置，LB 产品预留继续保留 |

范围与顺序见[延迟激活 §5.1](../../docs/specs/lb-deferred-activation.md#51-清空部分监听器)、[§5.2](../../docs/specs/lb-deferred-activation.md#52-清空最后一个下发监听器)及[现有 worker](../../internal/biz/network/load_balancer_worker.go)。单个原生声明被 API 接受不等于原对象退出；未知 CREATE/DELETE 继续原意图和已登记原 UID，版本、租约与必要清理责任保留，不能强拆 finalizer、改库成功、清 Namespace 或追删第三方对象。不增加生成工作负载或 VIP/EIP 物理回收等待。

## 实际流量与历史复现

私网、公网、双入口、TLS 和后端故障分别通过显式真实请求验证，记录实际版本、来源和结果。Gateway Programmed 或 operation 成功只说明对应配置结果，不承诺流量健康或 HA。

NET-VPC-LB-02 的旧安装指纹、fixture、环境和命令只作为[历史复现记录](../../docs/execution/records/NET-VPC-LB-02/README.md)使用；历史参数不再作为当前配置范例。产品创建、真实连通和测试资源清理按当次范围记录，执行结果只见[唯一状态](../../docs/execution/status.md)。
