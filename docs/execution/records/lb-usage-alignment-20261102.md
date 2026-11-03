# LB 使用方式对齐（2026-11-02）

本记录保存固定输入与分层证据，当前结论只维护在[执行状态](../status.md#lb-usage-alignment-20261102)。正式行为见[统一规格](../../specs/vpc-connectivity-lb.md)和[延迟激活合同](../../specs/lb-deferred-activation.md)。

## 固定输入

- Resource：`0bce651a84634b09c7df29939d2d5b934d76a67d` 加本次差异；独立工作树 `lb-alignment-resource-20261102`。
- Console：`2e6f278111bc169ed2df7f480304093a11528e0b` 加本次差异；独立工作树 `lb-alignment-console-20261102`。
- Governance：`316ee2efe75377c8d9d3c7edcc7f5fffe89a0d04`；现有 POST/PATCH 合同已经支持整组监听器、空后端和可省略 TLS namespace，无须修改。
- KCN 使用文档：本地 `/home/chabking/workspace/kc-networking-docs/usage/lb`，`c98e966928e853c9270248b6a282af72e1c4b161`；远端刷新不可用，此提交是本次实际核对快照。Envoy Gateway v1.8.3。
- 用户明确取消 LB 对基础 Intranet EIP/SNAT 的前置要求；明确允许跳过 Console 缺失的“ani-console对接文档补充”索引前置，依据最新 Governance 正式合同接线。
- 本地仅编辑、审查、Git 和传输；生成、格式化、测试、API 驱动、数据库和镜像均在 Fedora。原有两个 dirty 工作区保留。按用户要求只跑改动对应的必要测试，未运行完整 `make verify` 或 Console production build。

## 定向验证

| 层次 | 实际范围 | 结果 |
| --- | --- | --- |
| Resource 业务与真实 PG | 基础网络缺失/故障时 Create/Update 与真实 worker→adapter 生命周期；VPC Provider UID、当前代次、Ready、freshness、pending、租户和原子性负例 | PASS |
| Resource 业务与真实 PG，Kubernetes 边界 fixture | 三档私网、公网/双入口、空后端保存/添加/清空/重启/再激活/删除、多监听器、五协议及参数；Proxy/Backend/Policy 创建丢响应、Route 更新丢响应；Proxy 已建/Gateway 未发时产品删除与物理回收 | PASS |
| 能力与构建 | 公共 pair fingerprint、固定镜像首次冷启动及身份拒绝、Worker、sqlc 生成一致性、Go 1.26.10 native server/lb-api 和原生镜像脚本 | PASS |
| Console | scoped Oxlint/格式、typecheck、真实组件/Hook/API 定向 harness（外部 UI shell/HTTP 边界替身）；public/private/both 映射、成员身份、权重 0、whole-listener PATCH、版本/幂等/缓存、清空再加；GitNexus 最终 detect_changes | PASS |

原始失败保留：基础网络解耦测试先 RED 后 GREEN；既有单 VIP fixture 不支持同时创建两个 LB，改为串行完整生命周期后通过，不能据此声称并发容量通过。部分创建时 Update 仍受未结束 operation 屏障，未为不可达的“清空再激活”放宽该屏障。

可复现命令和日志：Fedora `lb-provider-20261102.JzNul23J/{run.sh,run2.sh,run3.sh,run4.sh,*tests.log,pg-iteration*.log}`；基础解耦 `ani-resource-service-runs/lb-base-decoupling-20261102-00f37640/{red,green,green2}`；Console `.codex-runs/lb-alignment-console-20261102/{oxlint.log,format-check.log,typecheck.log,harness.log,gitnexus-detect.json}`。测试只在外部 Kubernetes/控制器和 HTTP 边界使用 fixture，未模拟待实现的领域逻辑、事务或权限。

## 候选产物与现场

Resource 构建 run：`/home/chabking/workspace/ani-resource-service-runs/lb-candidate-20261102.cLPP0HeM`。438 文件来源 manifest SHA-256 `1ce1e1a1d8f6717a1ef259bdf1ca7b4b6431c6073d0dc18952da6b024fa68a80`；镜像专用 snapshot `d4ee3fe68aa9e1ebb20b41703b05ecc91d4c9f3110a4d1af091973d9eabc6f63`。native server SHA-256 `4bff7e8ef8220c193b7a98dd21805478d574a0894a528d2692c04bb7083e9815`，镜像内二进制逐字相同。镜像经受管 Harbor 发布为 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:e91716e3a44e41f527dc2e2ad4083f09c9741bae00a48862656631e098dcd0ac`。5000 registry 为只读（405），原 pull credential 无 push 权限（401），成功发布使用受管 push 身份；凭据均仅保存在远端 private。

现场发现首个候选漏渲染 `Gateway.spec.listeners[].allowedRoutes.kinds[].group`；API Server 自动补入 `gateway.networking.k8s.io`，导致严格 spec hash 永久不匹配、worker 每轮重写 Gateway，后续 Policy/Route 无法执行。只读真实业务 Observe（1.54s）确认依赖/成员/生成物全部正常，唯一失败为 Gateway Matches=false。修复显式输出该合法 group，保留完整 hash/UID 校验；fixture 注入真实默认后先 RED，修复后 private/small 创建→更新→删除 GREEN（2.669s）。Backend 的 Accepted 仍保留：v1.8.3 是 Route 引用后才写入该状态，未以伪造状态解决。

修复后构建 run `lb-gateway-group-20261102.X4Pqyzmf`，来源 manifest `75d117086d56d95ec995b2409ae3c1314ee18847eb55bc832f3a1e5d270fb61b`、生产 snapshot `c12b8fedfa487617d15a90b67a4cd68d847715a0a9c9115264e17743b5754366`；相对首个镜像生产源码只有上述一行改变。最终发布镜像 `172.16.101.10:30003/icn-01a15337-release/ani-resource-service@sha256:5440839323150fbdb3a814916ab8d5f2800e7044c30bee86ad57375f2933a716`，guarded rollout 已成功，无新增迁移。原始诊断、失败/通过与 BuildKit 回执保留在该 Fedora run 和 `live/evidence/`。

现场 run：`/home/chabking/tasks/lb-alignment-20261102/live`。实际 context `kubernetes-admin@ani-lab`，API `https://172.16.101.10:6443`、TLS name `lb.kubesphere.local`，`kube-system` UID `5277649d-e28d-4a0a-ae76-8354365c63bc`；仅 10～12 三节点。ani-03 在验收前已 Unknown/NotReady，本次不修改节点。

公共 EnvoyProxy UID `234df647-8e2b-47e9-9b12-3cdaaadc7efc`，GatewayClass UID `ed09e25e-9977-4188-afc3-1acc5bc6bcaa`；Class Accepted，公共 Proxy 无容量/ServiceType override。指纹 `2c57dd98237f5a3306653f183e0d8b533b1fc8eccc0f45bb226ecd1663f4e983`，由候选 lb-api 显式核对实际 context/cluster UID 得出。已有 legacy pair 和 Higress 保留。服务账号新增共用读取/必要 LB 权限及 tenant-a namespace 专属 Proxy Role；公共 Proxy 无写授权。

三个现场镜像 tag 的 registry manifest HEAD 均 200，digest 与配置一致：`gateway:v1.8.3`、`envoy:distroless-v1.38.3`、`gateway-dev:latest`。此前“没有运行中的 Envoy LB”指无业务 Gateway/数据面 Pod，未证明镜像缺失；本次 registry 检查也不等于所有节点已有缓存。

Network owner 备份 `pg_dump -Fc` 和文件式 `pg_restore --list` 通过，SHA-256 `e5004f888583663a170f44a5c2965e45debfe818ecbbc9e3ef1588756f2b9525`。原生 `server -migrate` 将 schema 25→26，0026 checksum `14534d550badcd716f83391667b1044fcd98a3dab9e07583978391a7ac94a6fe`，前 25 项未变；kind/member/listener CHECK 和唯一 active Proxy index 校验通过。备份与 DSN 仅在远端 private。

首次候选 full 启动被主线既有 VM 003 schema 缺失阻断；尝试回退旧镜像后，旧版明确拒绝 Network schema 26。故恢复候选，通过同一 native server 的既有 `-vm-migrate` 入口补齐 VM schema 2→3，checksum `d506d4ad601cd5319c71b39d29662078b7d6d079500f4444507aea19f914d8b8`。VM schema owner 备份和 TOC 校验通过，SHA-256 `5625b7638b722e9851be392cc8545bf3f600795a682fb38333f3604af600d8bb`；7 台 VM 的 Native/完整配置/初始规格/version/closing 摘要、21 条 operation intent 和 VM config Secret 均未变。随后候选 Pod `ani-resource-service-b5bcc4d55-chlx8` 启动成功，restart 0，1/1 Ready。启动另记录 Sandbox schema 未就绪 warning；本次未迁移或验证 Sandbox。

运行配置保留原 Secret，并切换到克隆的 `resource-configs-lb-20261102`，仅追加 LB installation 输入；Resource Deployment UID `e17f1f9e-e406-452f-838d-85062a6e860a` 保持。专属 Proxy 写 Role 本次应用于实际 tenant-a namespace，其余租户依[安装权限模板](../../../deployments/load-balancer/network-rbac.yaml)配置。

Console 采用任务独占 Fedora Vite 源码预览 `http://172.16.101.31:43927/`，`/api` 指向现场 Governance，未运行 production build。已正常登录并观察纯公网模式隐藏私网 VIP。页面创建 `lb_462b30c2d88b40e1bb690be72e4d0002`、HTTP/8080 空后端监听器成功，同事务 operation succeeded、无运行投影。再由页面 PATCH 加入实例 API 创建的真实 CPU 后端 `10.73.0.5:8080`（Attachment `att_492d471d1afc45e6aa4d1e6313c30a17`），最终达到 available/configured、applied=desired=2；专属 Proxy、Gateway、Backend、Policy、Route 的身份、规格和当前状态全通过，生成两副本 Envoy 的镜像及 small 容量与预期相符。

浏览器长时间停留后成员编辑暴露版本断点：观测会推进 resource.version，desired_version 保持 2。真实旧版本 PATCH 返回 **HTTP 412 / VERSION_CONFLICT**（最初按 409 推断的临时验证不作为现场通过证据）。最终 Hook 在每次用户保存前强制单次 GET；仅有效 int64 desired_version 与编辑基线相同时使用新 resource.version 发一次 PATCH，配置意图变化则停止并提示核对；GET→PATCH 竞争仍按准确 412 刷新，不自动重放。Fedora 七组定向 harness/静态检查及最终 GitNexus 通过。浏览器实际观察到冲突刷新提示、输入保留，再由修复后的单次保存成功将权重 1→2；成员 ID 保持 `ede0fa6c-4b1d-469b-8652-3bf257e402be`，配置实际 applied=desired=3。前端提交 `95e7fde`、`1e032e4` 已通过 SSH 推送。

## 真实数据面边界

私网 VIP `10.73.0.100:8080` 的直接请求 **FAIL**，不能由 configured 或 Pod Ready 追认为通过。同 VPC 客户端直连两个 Envoy Pod 的 8080/19001 也 refused。实际 Envoy admin 为 PRE_INITIALIZING，xds_cluster DNS 更新失败 13 次、成功 0，hosts=0、连接尝试=0：尚未进入 xDS TLS/gRPC。客户端查询平台 DNS `10.96.0.3` 超时；到 EG Service `10.96.230.106:18000` 和 Pod endpoint `10.16.0.240:18000` 均 SYN_SENT 超时。EG Pod 经临时 port-forward 的 TCP/TLS1.3/ALPN h2 通过，相关两个 namespace 无 NetworkPolicy。

现场阻塞已收敛到租户 VPC→平台 DNS UDP/TCP53 和 EG TCP18000 的内网往返路径；只改 DNS 名称不够，正向/回程丢包位置仍需网络层定位。本次没有修改 KCN/EG 代码、共享控制面或网络路由。原始证据 `live/evidence/{lb-first-vip-result.json,xds-admin-summary.json,envoy-resolution-stats.json,backend-dns-route-probe.txt,eg-tcp-state.txt,eg18000-listener-tls.json}`；临时 port-forward 已退出。

公网/双入口、HTTPS/TLS、三档容量的真实流量：NOT_RUN，只有上表受控业务/adapter 证据；平台 Prometheus 未启 OTLP receiver，监控接入与指标健康验收：NOT_RUN。产品 data_plane_state 保留 unknown。

## 清空、再激活与清理

页面清空最后一个成员后保留监听器，desired=4。产品正常删除 Gateway/生成资源；等待默认 Envoy Pod 360 秒优雅终止后，Gateway、专属 Proxy、Backend、Policy、Route、Service、Deployment 和 Pod 均不存在，实际 Subnet 的 VIP `10.73.0.100` 已释放；公共 Class/Proxy UID 保持。未缩短 grace、去除 finalizer 或删除数据库行。证据 `live/evidence/{lb-cleared.json,lb-cleared-physical.json,lb-cleared-address-common.json}`。

页面再次添加同一真实后端，实际达到 configured、applied=desired=5；新成员 `b542e9cb-d28b-4d00-b7ed-afe04c7545af`，新专属 Proxy `lb-envoy-proxy-96ce5471ce6f472eb14f83c49b279621`、UID `d9adedd9-ae93-418e-97a2-f6c54ac330b4`，新 Gateway `lb-4d559854214f444784fabbebaf7abd55`、UID `28887c64-596d-43a0-83ed-b579e8f3171f`。证据 `live/evidence/{lb-reactivated.json,lb-reactivated-objects.json,lb-reactivated-physical-before-delete.json}`。生成资源名称不能假定包含 LB ID，物理核对使用 Gateway 名称、UID 和 owner chain，记录 15 个实际对象 UID。

随后经正常租户 API DELETE（operation `65419a41-434b-44f6-a5bf-7acdc28a23f0`），等待正常优雅终止至 deleted。上述 15 个关联对象及其子对象均已不存在，VIP 再次释放，公共 Class/Proxy 原 UID 仍在。证据 `live/evidence/{lb-delete-receipt.json,lb-deleted.json,lb-deleted-physical.json}`。这是配置生命周期与回收的 PASS，不改变上节真实流量 FAIL。

LB 物理清理完成后，经正常产品 API 删除任务 CPU 实例 `b08324d2-63a7-4d02-98c2-12224ee6e247`，operation `7c1ca05d-676c-4dd5-9b9a-9d66471383e8` completed/done；observed deleted、network released，compute/workspace/networkCleaned 和 quotaReturned 全 true，任务 Pod/StatefulSet/PVC/PV/Notebook/VNic/VNicIP 剩余为空。任务 Image `img_9d5ce126ef8e4e20b91788c062c33b97` 正常注销（version=2），Harbor 唯一任务 tag 删除并读回 remaining_tags=[]；按范围保留产品 tombstone 和无 tag 的任务 manifest/layers，未动基础 BusyBox、共享 cache 或既有 VM，既有 VM 仍 Running/10.73.0.4。未使用 raw Kubernetes 删除或数据库写入。证据 `live/evidence/{backend-cleanup-result.json,backend-deleted-physical.json,backend-image-unregister-response.json,backend-registry-tag-deleted.json}`。

本次独占 Vite 预览进程组 `3873916` 已停止、浏览器临时页已关闭，用户开发服务未修改。Console SSH 推送后的远端 branch SHA 为 `1e032e44c9c6e5058394781a89d1f6af90da20d0`，与本地 HEAD 一致。最后仅收尾文档，未重复已经通过的测试；新增/修改文档的 16 个相对链接目标及 `git diff --check` 通过。

## 用户追加：基础网络 EIP/SNAT 前后对照

2026-11-02 22:39 CST，用户要求“创建一个基础网络，然后给 vpc 分配一个基础网络的 eip 并做 snat，看看能不能通”。全部 API 驱动、Kubernetes 操作和流量探测仍在 Fedora，复核原 ani-lab API、kube-system UID 和 10～12 节点后执行。独立 run：`/home/chabking/tasks/lb-alignment-20261102/base-snat-check`；只测试原 tenant-a/test VPC，不操作既有 VM。平台/租户以正常登录获得本轮会话，凭据仅在 private。

现场产品 Intranet 池为空，原 VPC 无基础 EIP/SNAT；现有 `kcn-config.intranetNetworks` 已覆盖 `172.16.101.0/24`、`10.96.0.0/16` 和 `10.16.0.0/16`。为直接验证网络因果关系，使用任务独立命名的 KCN 资源，不执行跨 VPC 补齐 CLI、不写 Resource 数据库、不伪造分配核验证据。创建前核对池 CIDR 不与现有 VPC/Subnet 重叠。

| 资源 | 身份/结果 |
| --- | --- |
| 基础 Subnet | `kcn-system/lb-base-net-20261102`，Intranet `10.250.250.0/28`，网关 `kcn-system/kcn-cluster` / `10.250.250.1`；UID `f5329d9d-9050-46b7-a0b0-4e76d32221c6`，Ready/当前代次 |
| 基础 EIP | 原 tenant-a namespace 的 `lb-base-eip-20261102`，地址 `10.250.250.2`；UID `553e3b83-6741-4879-8bd2-458c961fc196`，Bound 到本次 Snat/原 VPC |
| 基础 Snat | 同 namespace 的 `lb-base-snat-20261102`，引用原 `vpc-6d7a163a92c24a359dd6536475cb68ad`；UID `9896c3ff-2b56-42e3-b198-e1d7e55c4952`，disable=false、phase=Bound |
| 测试后端/客户端 | `lb-base-snat-check`，UID `7d917252-b113-4c32-9287-8c6c111f0d35`，ani-01、`10.73.0.11:8080`；响应 `lb-base-snat-check-20261102` |
| 测试 LB | `lb-base-snat-gateway`（UID `1aac16f2-730f-4f90-99f2-6bacf2d858aa`）、专属 `lb-base-snat-proxy`、Backend/HTTPRoute；复用本轮已验证的公共 `lb-common` 和相同 small 私网投影，VIP `10.73.0.100:8080` |

原 task 派生 BusyBox digest 仍在 registry，首次测试 Pod 未带既有 pull secret，拉取被正确拒绝；为本次测试 Pod 引用同 tenant namespace 原有 pull secret 后启动成功。未改 registry 权限、共享凭据或基础镜像，原失败回执保留。该 Pod/手工 LB 是本次 Provider 试验资源，不是前轮已删除的产品 Instance/LB，也不冒充新的产品创建验收。

绑定 SNAT **之前**，同一后端直连正常，DNS 查询超时、EG Service `10.96.230.106:18000` 和 Pod `10.16.0.240:18000` 连接失败；Envoy admin 为 PRE_INITIALIZING，VIP connection refused。绑定 SNAT **之后**，同一客户端 DNS 返回正确 EG Service IP，两个 xDS TCP 连接成功，VIP 返回 HTTP 200 和上述业务标记。两个 Envoy 分别为 `10.73.0.13` / UID `b59fa179-3386-4606-95a6-4e9ab236bdc9`、`10.73.0.12` / UID `b7c00f28-50cc-46f3-b035-73bfd74cb902`；二者都从初始化进入 LIVE、UID 未变、restart=0，逐个 Pod 的 8080 也返回 HTTP 200。没有改镜像/业务代码、重启控制器或手改 kcn-config/default VPC。

证据保存在 run 的 `evidence/{baseline-probe.txt,envoy-before-server-info.json,vip-before.txt,after-snat-probe.txt,envoy-live-summary.json,final-subnet.json,final-eip.json,final-snat.json}`；临时 admin port-forward 均在 finally 中停止。读取平台能力时 `load_balancer_ready=true`；`base_connectivity_ready=false` 仍如实保留，因为本次没有登记产品 Intranet 池及其验证证据，不能把底层流量成功冒充产品管理能力就绪。

按本次创建和继续复验的用途，基础 Subnet/EIP/Snat、测试 LB 及后端 Pod 当前保留；原用户 VM、既有默认公网出口和共享控制器保持。Console 预览已按用户前一条要求重新打开，版本仍为 `1e032e4`。本次只追加运行试验与记录，未改 API 准入规则，也未扩大到公网/TLS/监控或全量测试。

## 用户追加：独立纯公网 LB，不分配基础网络 EIP

2026-11-02 23:02～23:12 CST，用户要求“再做一个公网 lb 的实验，不要基础网络 eip”，随后明确“测完暂时不删除这次的数据”。在 Fedora、同一经 UID 核对的 ani-lab/10～12 集群执行；为了不撤销前次私网试验的 SNAT，创建独立 VPC、子网、后端和纯公网 LB。读取 KCN 使用文档同一快照的 `vpc-public.yaml`，采用 `envoyService.type=LoadBalancer`、`lb_vip_address=disable` 和单独 `lb_eips`，复用既有公共 `lb-common`。本次仍是 KCN Provider 试验，不冒充产品 API 或 Console 全链。

run：`/home/chabking/tasks/lb-alignment-20261102/public-no-base-check`，所有清单在 private、原始回执在 evidence。资源均在原 tenant-a namespace `tenant-46936e6b-3a3b-4868-8d88-6833665abb70`：

| 资源 | 实际身份 |
| --- | --- |
| VPC / Subnet | `lb-public-no-base-vpc` / `lb-public-no-base-subnet`；`10.74.0.0/16` / `10.74.0.0/24`，网关 `10.74.0.1`；VPC UID `9df6e706-5c86-4297-8189-293a11142604` |
| 公网 EIP | `lb-public-no-base-eip`，UID `0fa7fcae-985d-4f96-86dd-5b360fa61eba`，`172.16.102.194`，原 Public pool；phase=Bound，resourceType=Service，绑定本次 Gateway Service 和新 VPC |
| LB | `lb-public-no-base-proxy` / `lb-public-no-base-gateway` / `lb-public-no-base-endpoint` / `lb-public-no-base-route`；Service UID `aaf6c590-56f3-47d8-b5f7-be42893e7a69`，外部入口 `172.16.102.194:8080`，不分配 VPC 私网 VIP |
| 后端/客户端 | `lb-public-no-base-backend`，UID `329d4685-dbea-45d0-aa8c-1d412f37b69b`，`10.74.0.2:8080`，业务标记 `lb-public-no-base-check-20261102` |
| 实际 Envoy | `lb-public-no-base-gateway-55977c574f-gsfvr`，UID `ccdea7c7-8b58-4f98-81c2-39ef9d3a88c2`，`10.74.0.3`，restart=0；EndpointSlice target UID 相同、ready/serving=true |

VPC/Subnet Ready，Public EIP 正确绑定，Route/Backend Accepted。全量只读核对该 VPC 的 SNAT 引用为空、Intranet EIP 绑定为空，唯一 EIP 来自 `type=Public` 的池并绑定 Service；既有 Intranet EIP/SNAT 仍只指向前次 test VPC，未给新 VPC创建基础或公网 SNAT。证据 `final-no-base-proof.json` 和 `final-{vpcs,subnets,eips,snats,services,endpointslices}.json`。

初始沿用两副本 small 的 1 CPU/1Gi 申请，但节点事件为 Insufficient CPU/memory，两个 Envoy 未调度。为排除容量因素，只将本次试验专属 Proxy 改成单副本，申请 50m/128Mi、限制 1 CPU/512Mi；不修改产品规格或前次私网 Proxy。新 Envoy 随后正常 Running、Gateway Programmed，但这些状态不作为流量 PASS。原调度失败和实际调整保存在 `scheduling-{pods,events}.json`、`gateway-wait-after-probe-capacity.txt` 和实际清单。

**结果 FAIL：** 后端自身 HTTP 200；从同 VPC 查询平台 DNS 超时，EG Service/Pod 的 TCP18000 连接失败；Envoy Pod 自身 8080 connection refused。对已运行的 Envoy 做一次有界 55.2 秒初始化观察，最终 uptime=203s 仍 PRE_INITIALIZING：xDS update_attempt=2、update_failure=1、update_success=0、membership_total=0、upstream_cx_total=0，尚未加载转发监听器。

Fedora 的初次 HTTP 经透明 Meta 路由返回空响应，不能据此归因到 LB；因此另从 ani-01 已有 hostNetwork CNI Pod 执行只读 curl，实际源 `100.64.128.2`、经 `ovn0` 到公网 EIP，初始化观察后再次请求仍 connection refused、curl exit=7、HTTP000。这是实验环境 Public 池地址的外部 VPC 访问，不声称全球互联网可达。证据 `no-base-dns-xds-backend-probe.txt`、`envoy-direct-http-from-vpc.txt`、`envoy-initialization-observation.json`、`envoy-final-server-info.json`、`envoy-final-xds-stats.txt`、`final-public-http-from-ani01.txt` 和 `conclusion.json`。

本试验表明该现场只绑定 LB 公网入口，不能替代 Envoy 到平台 DNS/xDS 的内网可达性。未添加基础 EIP/SNAT 来改变本次条件，未改共享网络、镜像、业务代码或重启控制器。临时 admin port-forward 均已停止；按用户明确要求，所有本次 VPC/Subnet/Public EIP/LB/Envoy/后端及相关数据暂不删除，前次私网试验和用户既有资源也保留。

## 用户追加：同组公网 LB 补基础 EIP/SNAT

2026-11-02 23:14～23:18 CST，用户进一步要求“在这组测试，再加上基础网络的 eip，看看公网 eip 能不能通”。在上述同一 VPC/LB 上执行，仅新增 Intranet EIP `lb-public-base-eip-20261102`（UID `20aa773f-56ef-4076-b130-f0378106487b`、地址 `10.250.250.3`、复用 `kcn-system/lb-base-net-20261102`），以及 Snat `lb-public-base-snat-20261102`（UID `87656f23-ec0e-4557-865d-b234b302a0aa`、vpc=`lb-public-no-base-vpc`、disable=false、phase=Bound）。新增资源及原所有试验数据继续保留。

**结果 PASS：** 同一后端/客户端查询平台 DNS 成功，EG Service/Pod TCP18000 连接成功。原 Envoy 自动进入 LIVE，uptime=557s；xDS membership_total=1、update_success=19、upstream_cx_total=1，历史失败计数仍保留。前后逐一核对 Envoy Pod UID `ccdea7c7-8b58-4f98-81c2-39ef9d3a88c2`、两个容器的 containerID 和 restart count 完全相同，未重建或重启 Envoy；公网 EIP/VPC/Gateway/Service/后端 UID 及公网地址也保持不变，本阶段没有再次调整容量。

23:16 CST 从 ani-01、ani-02 两个既有 hostNetwork CNI Pod 以 `curl --noproxy '*'` 访问同一个 `172.16.102.194:8080`，两者 curl exit=0、HTTP 200，响应均为原后端 `10.74.0.2:8080` 的 `lb-public-no-base-check-20261102`。这是该实验环境 Public 池入口的真实 VPC 外流量；不扩大为互联网路由、容量、TLS 或 Console/Resource 产品全链证明。

证据在同一 public run 的 `evidence/{with-base-dns-xds-probe.txt,with-base-snat-bound.json,with-base-before-envoy-pod.json,with-base-after-envoy-pod.json,with-base-envoy-server-info.json,with-base-envoy-xds-stats.txt,with-base-public-http-ani-01.txt,with-base-public-http-ani-02.txt,with-base-public-http-summary.json,with-base-conclusion.json}` 及 `with-base-final-*.json`。原无基础条件的 `conclusion.json` 和全部 FAIL 回执未覆盖。临时 admin port-forward 已停止；没有清理任何原试验或新增资源，没有修改代码、镜像、公共配置或重启控制器。
