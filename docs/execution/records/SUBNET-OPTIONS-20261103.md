# 租户子网创建参数实施记录

用户指定在租户创建 VPC 子网时开放 gatewayIP、excludeIPs、enableDHCP、dnsServers。网关复用现有公开 `gateway` 字段；其余公开字段为 `exclude_ips`、`enable_dhcp`、`dns_servers`。[正式规则](../../specs/vpc-subnet.md#31-身份与输入规范化)；当前状态只维护于[执行状态](../status.md#subnet-creation-options-20261103)。

## 固定来源与执行范围

- Resource：SDK `076c267e6e16b9527d8222b65902f5c7ec89f769`、业务 `d54aafadb51dace5d75da1a3e6c919275f84ce47`；Gov `0a2855843f1dcb2825bd087673c206796e041740`；Console `2b56757efdc723cd848e10b4e1b285a550690467`。均在独占工作树实现并 SSH443 推送，没有修改用户原工作区。
- Gov 正式 Resource SDK `v0.0.0-20261103151539-076c267e6e16`，标准模块 zip/go get，无 replace。Proto/sqlc 在 Fedora 使用固定 Buf1.60.0/sqlc1.31.1 和 Go1.26.10 生成并回传；本地仅编辑、Git和传输。
- 参数随创建事务和幂等快照持久化，在 Get/List 回显，KCN 真正收到字段并按接受配置观察。默认空列表/false 保留旧指纹；排除范围规范化合并，DNS保留首选顺序，关闭DHCP仍可预先保存DNS。未增加已有子网编辑、路由、跨Namespace共享或基础网络聚合变更。

## 必要检查

- PASS：Subnet 业务参数规范化、非法地址、原网关行为与旧默认指纹；Provider `TestKCSubnetOptions*` 的创建/默认/配置比对与身份保护。
- 初次真实PG批次在旧 fixture 的 tenant namespace reader 前置失败，包括新增测试；没有把它写成业务通过。改用现有维护中的 `newAttachmentFixture` 后，`TestSubnetPGCreationOptionsPersistenceReplayAndWorker` PASS，覆盖真实PG持久化、Get/List、重放、修改参数冲突及真实worker/adapter；仅外部KCN控制器由HTTP fixture提供。未修复或反复重跑两个无关旧fixture。
- PASS：Gov `make api` 与两个字段/可信RPC契约；Console五文件定向lint/format/typecheck，最新图差异检测及真实表单非法格式/范围阻止提交。未生产build Console，未全仓fmt/lint、未追加全量make verify、数据库备份或额外发布门禁。
- 原始日志：Fedora `/home/chabking/tasks/subnet-options-20261103/{biz-check,provider-check,pg-check,pg-check-current-fixture}.log`；Gov run `/home/chabking/workspace/ani-governance-runs/kcn-vnic-20261103/subnet-options-{check,image,rollout}-01.log`。

## 部署与真实创建

- 固定集群 kube-system UID `5277649d-e28d-4a0a-ae76-8354365c63bc`，经 Fedora/原 cluster10 SSH 会话执行。迁移30已应用，无新增RBAC；保留原Env/节点放置。Resource镜像 `sha256:85534215aa1475db60476ff587fb54f4c9eb6fa15cae537e948cc94cfd018092`，generation89/observed89/Ready1；Gov镜像 `sha256:b8fc2e4be6b44092217db44e1fdb9c686fdba67d0c9315ac6435df06c001078d` rollout成功。Console仍为Fedora任务预览 `http://172.16.101.31:43928`。
- 唯一UI创建：`subnet-options-1103` / `subnet_2e5a7520556f469f8320ad3a051ef14e`，VPC `vpc_bd5d4a4e965f465ba26f001ab11f16e9`，operation `2bd14a4d-0e9a-4289-a800-9041ea55b950`，key `5a025544-3a51-4bcc-a699-e0f919d64f56`。POST200先provisioning、随后GET available/version4，非API ACK替代完成。
- 四参数实际值：CIDR `10.75.5.0/24`；gateway `10.75.5.254`；exclude `10.75.5.10..10.75.5.20`、`10.75.5.30`；DHCP=true；DNS `10.75.5.253`、`1.1.1.1`。页面与持久查询一致。
- 原生Subnet UID `edc2da70-9f7f-4f99-9c6c-23a5e755f01f`，Ready=True；实际可分配范围 `10.75.5.1-10.75.5.9,10.75.5.21-10.75.5.29,10.75.5.31-10.75.5.253`，所选范围、单IP及网关确实被排除。
- 实际 DHCP_Options UUID `be96dad5-7eb5-433b-87e1-e1a2cb89d538`；现成 `kcn-system/kcn-ovn-central-85dc666849-5dpwf` 容器通过 `ovn-nbctl --db=tcp:172.16.101.11:6641 --no-leader-only` 只读查询，cidr为上述网段，router/server_id均 `.254`，dns_server为 `{10.75.5.253,1.1.1.1}`。仅证明配置落位，没有向这些DNS发解析请求，也没有新增VM/Pod来冒充guest验收。
- 最小证据：Fedora任务 `private/{native-subnet,ovn-dhcp-options,native-proof}.json`；Console任务 `/home/chabking/tasks/vnic-console-20261103/ui-subnet-options-create.json` 与 `ui-subnet-options-detail.png`。不含Authorization。成功测试子网保留，原Subnet354、A/VM1/SG等排查场景未执行变更。

## 既有详情页限制

关联实例旧调用为 `/instances?limit=10&subnet_id=...`，正式公开实例列表没有subnet_id过滤；改分页仍无法满足该过滤，去掉过滤会返回全租户实例，不能冒充关联结果。已限定核对并停止，没有扩展后端或隐藏成空列表；四项创建/配置/回显验收不依赖该查询。

<a id="pr53-merge-20261104"></a>

## 2026-11-04：PR #53 合并收尾（未部署）

- 分支来源 `35152639` 正常 merge 主干 `9aa96edf`，保留主干已发布的 `0028_attachment_consumer_type.sql` 原名与原字节；本任务三条 SQL 只顺移编号为29–31。显式 owner `Migrate` 在原事务/advisory lock内识别旧任务28–30的精确连续校验和，补执行主干28并倒序顺移账本，保留原checksum/applied_at；运行时仍严格校验canonical31，不放行旧历史。没有现场数据库升级或新镜像部署，实际运行仍为上文 `d54aafa`/schema30。
- protobuf/sqlc通过 Fedora 固定 Go1.26.10/Buf1.60.0/sqlc1.31.1重新生成。合并交点中，主干新增关联实例查询的容器当前地址投影补上已接受手动VNIC的精确ProviderName/UID匹配，保留Pod/current/IP owner边界。
- PASS：Network关联查询/consumer type、VNIC申请地址、Subnet参数业务定向；Provider申请地址/子网参数及当前地址投影定向；真实PG的main28升级、旧task30升级与canonical replay，验证原迁移checksum/applied_at、VNIC申请IP/MAC及Subnet选项保留、consumer_type回填；未知checksum/混合/空洞历史拒绝且不改schema/ledger；既有申请地址/子网选项PG定向；`go build ./cmd/ani-resource-service`。未运行全量verify或现场升级验收。
- 初次PG fixture先因参数类型推断42P08、随后因缺租户Namespace外键前置失败；补齐fixture后仅重跑受影响迁移用例，未将前置失败记为升级成功。原始日志位于 Fedora `/home/chabking/tasks/vnic-pr53-merge-20261104/{merge-targeted,migration-targeted,released-migration-targeted}.log`，源码快照同目录 `source`。测试由既有 `scripts/integration` 创建并清理独占PostgreSQL容器。
