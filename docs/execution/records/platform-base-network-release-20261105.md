# 平台基础网络：发布与真实主链验收

执行日期：2026-11-04～05（Asia/Shanghai）。承接 Res #62、Gov #93、用户 fork Console #1。
按用户最新指令，本地只编辑、查看和转运；构建、PG、打包、推送在
`ubuntu@100.64.128.15`，集群操作在 `kubercloud@100.64.128.12`。
用户要求必要的定向验证及真实 Console 验收，不要求全量测试或额外审计。
正式规则仍见 [平台基础网络规格](../../specs/vpc-subnet.md#平台基础网络租户接入)。

## 发布物

| 服务 | 实际运行源码 | Harbor digest |
| --- | --- | --- |
| Resource | `585c2d9f87d41543dac54fc0ba5af6191eafc848` | `sha256:a335dc68aeebdfc3671575941d8b3ae163fc64bada8b26cf71aa3fd09118e466` |
| Governance | `e334d81f02f2c8a6cb7ba270463e4be8e9e5bdd7` | `sha256:35cd33d48a1ba162a9b98842c4de8588461b1829129157a9c282bb7289e86147` |
| Console | `a86dcba450d4c8434a8affc8c84245dd625f3c17` | `sha256:22a838e22c27838bf3e69483e33b376f6f69cb21cf63f3178ff1713345de1668` |

仓库路径前缀均为 `10.10.1.66:30003/ani-gpu-platform/`，服务名分别为
`ani-resource-service`、`ani-governance`、`ani-console`。
Gov 运维 CLI 镜像为 `ani-governance-admin@sha256:f657a3bd3991225855eb3e60989f40a0f186cb63576f35edb2e90f22f84d45cf`。
三仓均已普通推送到原 `codex/platform-base-network-20261104` 分支，未合并 PR。
Gov 保留后续主干的 current-ledger 改动；Console 保留线上 `80002ba` 的 GPU 页面修正。
Gov SDK 保持正式 `v0.0.0-20261104150849-76dc2a3218d2`，没有 local replace；本次 Res 后续补丁未改变公开协议。

Go 为 1.26.10；生产构建使用 `GOMAXPROCS=4 GOFLAGS=-p=4 CGO_ENABLED=0 go build -trimpath`。
复用已有 Go、pnpm、distroless 和 Console 基础镜像缓存。PG 使用内网缓存的 17.11
`10.10.1.66:5001/library/postgres@sha256:b66d2b738032cfc48764b8b14dc1e338368183903b5ede6bb6e4c0793fd4a9c2`。
初次直连 SSH/依赖下载不稳定，后续使用经集群机中转的 SSH、7897 代理及正式模块文件缓存。
最终 Res 在 Ubuntu 拉取 GitHub 提交，`api/cmd/internal/pkg/migrations/go.mod/go.sum` 等构建输入与该提交差异为零。

## 配置、迁移与权限

- Network 32→33 通过正式 `server -migrate` 完成；新增表 owner 为 `resource_migrate`，运行角色保持 `resource_runtime`。
- 迁移前独占备份位于集群机任务目录，Network dump SHA256 为 `22ad0a1c2ffa0246143f0fd05815bca622156ea49cb988e1a585485d72b35316`。原 Deployment、Secret、RBAC 与 Gov API/权限表备份同目录受限保存。
- 增补原 Resource ClusterRole 的 BNI get/create/patch/update。Gov 官方 CLI 先 dry-run 后同步，新增唯一关联路由并更新四项网络读取合同；执行正式 `register-network-instance-permission.sql`。
- 验收租户 2 的原管理员角色 5 增补关联读取及缺失的网卡候选、VM 设置、容器读取/设置共五项权限；记录明确 tenant_id=2、status=ON、effect=ALLOW。未改套餐。第二租户使用原有效 NETWORK 套餐和临时的九项正式路由授权，验收结束撤销本任务新增行。
- 现场 VM Secret 的 `Service.ImageUpload` 不在选定 main/PR 合同中。已切换到保留的 `ani-vm-resource-pr57-20261104`；与原 `ani-vm-resource-r3-20261104` 除该项外完全相同，两个原 Secret 均保留。本次版本不交付该额外 ImageUpload 能力。
- 初次制包漏旧入口，已补齐 `/ani-resource-service` 与 `/app/bin/ani-resource-service`；失败镜像不作为最终发布物。历史迁移 Secret 已清理，改用本次临时维护身份，没有重置已有账号密码。

用户明确选定的目录只包含以下原生对象，不开放 `kcn-default`，不修改平台 CR：

| 对象 | 产品 ID | 原生引用及 UID |
| --- | --- | --- |
| VPC | `vpc_03eaeb5776614d3ab114a7b7fb5b3de0` | `kcn-system/kcn-cluster`；`55c8c219-0acc-4088-a5a5-4608a81d80e5` |
| Subnet | `subnet_5dadd7ee2895480285d65493dc291109` | `kcn-system/pool-8836e1121d3c40a4ab73859afef5d6d8`；`2d392b1a-2e6e-476c-a7ef-c93a6299cb95` |

CIDR 为 `10.20.0.0/24`，网关 `10.20.0.1`；产品父子关系来自显式配置。

## 实际结果

| 层面 | 结果 |
| --- | --- |
| 定向 PG/业务 | 平台可见性、关闭准入/重放、跨租显式 MAC 约束、Attachment 重放优先、VM 两轮准备、旧 VM/Instance binding revision 兼容通过。VM/Instance 的独立故障恢复 PG 全组未运行。 |
| Gov | 平台 scope/capability、optional bool、关联 unknown/tenant 映射、错误码和空 NAD UID 冻结/分发定向检查通过。 |
| Console | pnpm 8.6.11 frozen install、lint、fmt、fmt:check、typecheck、production build 通过。 |
| 两租户 VNIC | A 获得 `10.20.0.21`，B 获得 `10.20.0.27`；B 没有私有 VPC/Subnet，看到同一平台产品 ID。跨租同 key 返回不同资源；B 读取 A VNIC 为 404，B 关联 total=0。未通过改 KCN 状态制造成功。 |
| VM | 正式 HTTP 创建并运行，自动地址 `.22`，普通重启后 `.24`；guest SSH 确认实际接口。Console 保存手动 VNIC、显示待重启并执行正常重启，实际切换为 `.21`；同租户容器连接 VM SSH 端口取得真实 SSH banner。 |
| 容器 | 使用已有租户 CPU 工作镜像创建、运行、重启后再次 running，地址 `.26`，networkState=attached。首次误选需要训练配置的镜像启动失败，已通过正常 API 删除，未把 Pod Running 当产品成功。 |
| 关联与页面 | 正式关联接口 pageSize=1 的两页 total=2，分别为本租户 VM/容器；浏览器实际显示平台标签、平台托管、原 ID 名称、子网/网关、关联行和禁用删除。无权限身份实测 403。 |
| 关闭准入 | 实际关闭 accept_new 后，固定 ID 仍可读；原 VNIC key 重放200，新申请412/NETWORK_NEW_ADMISSION_CLOSED，平台 VPC/Subnet 删除403。已有消费者保留运行，后续删除/释放使用原引用。 |
| 现场补修 | 手动 VM 已生效但地址投影快路径漏 VNIC UID，导致 UI 显示状态待确认。对应 adapter 回归先 red，再修正为只对当前手动 VNIC读取实际 UID；自动完整 Pod 地址保持原单次读取。相关地址投影/VM 展示定向 green，最终真实 effective_now=true、Console“已生效”。 |

测试标识：VM `fbef215a-7f2c-4f4e-9137-21fb2e2f342d`；A/B VNIC
`vnic_9fb29a3c2ba84d0e88698b24e211bb31` / `vnic_98bdea86ce544641ab7cc0875842f43d`；
有效 CPU 容器 `21444081-e4eb-465f-967a-8116753396c9`；首次训练镜像尝试 `dd0f2f3a-a9d5-4d59-a6f7-5e56aa830501`。

清理与最终开放状态：**通过。** 关闭新接入期间，VM 删除 operation succeeded，手动 VNIC
仍 available/free 并保留 `.21`，随后显式删除成功；两张手动 VNIC、VM、两个容器尝试
对应的原生 VM/VMI/Pod/PVC/VNIC 均无遗留。两个容器 compute/workspace/network 清理及
quotaReturned 均为 true，关联 total=0。原五台 Running VM 保持原版本和运行状态。
平台原 UID、两个租户 enabled BNI 保留。目录已恢复 published/accept_new=true；三服务均 Ready1。
第二租户的九行临时授权、维护数据库角色、两个临时 Secret、迁移/同步 Job、guest key
均已清理，Ubuntu 测试 PG 容器为零。精简原始回执见[验收摘要](platform-base-network-release-20261105.json)。

## 检查边界与证据

`make verify` 首次在旧 observer 插桩锚点失败；修正后生成、边界与故障覆盖构建已通过，
执行到全量测试阶段时按用户新指令停止，不能称完整 verify 通过。
一次较宽的 Platform 选择器额外命中 EIP fixture 和取消收敛失败，保留日志，归属未核验，
不扩展为本轮整仓整改；随后按明确平台主链用例定向验证。未执行全套 race、安全扫描、
全部历史集成、Windows 或 KCN/OVN 内部隔离证明；不将其记为已验收。

原始日志与缓存构建材料：`.15:/home/ubuntu/platform-base-release-20261104/`。
迁移、私有备份、正常 HTTP 驱动与精简无凭据摘要：
`.12:/home/kubercloud/platform-base-release-20261104/`。
证据只复用上述单一目录和本记录，不复制凭据、完整业务 dump 或运行源码快照入 Git。
