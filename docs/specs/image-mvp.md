# Image MVP 架构规格 v1.0

状态：待实施。已有代码依据见 [来源](../plans/image-mvp-sources.md)；API、数据和方法分别以对应规格/契约为准。本规格不记录“已完成”。

## 1. 边界与目标

交付“租户自动开通—自行推送—登记版本—一种普通容器业务使用”。只面向 Docker/OCI 容器镜像，不处理 VM 磁盘、模型权重、Helm 包。Image 与 Network 平级；同 repo/module/process 不意味着共享数据所有权。

单 Harbor、单集群、每租户一个已绑定的容器 Namespace。平台可登记镜像，租户可登记自己的镜像。用途支持 container/development/inference/finetuning/training 多选；加速器适配是维护者声明的独立字段，不作硬件或托管业务兼容性认证。

**不做**完整仓库/Tag 管理、后台镜像发现/同步、Webhook/NATS、本地全量制品索引、在线构建、扫描界面、审计中心、跨租户分享、GC、自动保留策略、通用调度、通用凭证分发平台。普通容器与前端接入方不得凭空假设存在。

## 2. 组件与流量

```text
浏览器 / 平台 API 用户
  → ani-governance（JWT/HMAC、成员与权限、uint32 tenant→资源 UUID）
  → gRPC+mTLS → ani-resource-service / TenantImageService
                    service/image：协议和错误转换
                    biz/image：租户规则、空间、凭证、登记用例
                    data/image：sqlc/pgx、Harbor API、Secret 加密
                      ├─ PostgreSQL image schema
                      └─ Harbor 管理 API（固定内部上游）

Docker/Podman/CI → Registry 入口及 Token 端点 → Harbor（直接 Push）

普通容器创建方 → Image 内部窄端口（解析与受限拉取材料）
              → 自己负责的租户 Namespace/Secret/Pod 模板
节点 kubelet/容器运行时 → Registry 入口及 Token 端点 → Harbor（直接 Pull）
```

Image 不代理镜像层，不为注册下载全部镜像。Registry 管理入口仅后端/受控运维访问；客户端可访问 Push/Pull 及其必要认证端点，但不能访问管理 UI/API。路由配置在授权测试环境验证，不随意重配共享网关。

## 3. 身份与授权

| 身份 | Harbor 范围 | 交付边界 |
|---|---|---|
| 平台管理身份 | 创建 Project/Robot、查询/停用所需管理操作 | 仅 Image adapter，Secret 文件注入，不下发前端/工作负载 |
| 租户发布 Robot | 当前租户 Project 的 repository push+pull；platform pull | 仅持 image:credential:issue/reset 的租户管理员或 CI |
| 租户运行 Robot | 当前租户 Project + platform 的 repository pull | Image 加密保存，受信工作负载创建方写入 Namespace Secret |
| 平台发布 Robot | platform 的 push+pull | 受控平台发布流程，不与租户凭证共用 |

租户 Robot 不带 system permissions，不使用 Cover all projects，不授予 artifact delete、project update、robot CRUD。收到的完整用户名以 Harbor 返回值为准，不硬编码 `robot$` 前缀。

**管理身份现实约束**：上游 H4 对 Robot 创建子身份有权限子集校验，不能承诺“只给 CreateProject/CreateRobot 就能替任意未来租户授权”。MVP 使用已授权的后端管理身份；可先用仅后端持有的 Harbor 管理账号配置完成闭环。改用更窄的管理 Robot 必须用实际版本证明授权覆盖，不在租户/节点发 admin，也不擅自更换现有密码或扩大部署授权。

Gov 决定当前用户/AK 的角色、模块与权限，Image 根据可信上下文限制 tenant。Image 不建立第二份 IAM、成员表或跨服务外键。平台操作走本机受控管理模式，不能用 tenant_id 空值/全零 UUID/前端 scope 参数获得平台写权限。

对租户资源 Get/Update/Unregister：他租户 ID 与不存在 ID 都返回 NotFound。对提供了明确不允许的仓库路径的 Register：返回 PermissionDenied，不能去查询并泄露对方制品信息。

## 4. 空间与友好名称

内部资源 tenant UUID 沿用现有映射；用户启用时选择稳定 `slug`，建议 3～40 字符，小写字母开头、字母数字结尾、中间只含小写字母数字和 `-`。Project=`t-`+slug。`platform` 为平台空间，默认配置不允许重名。

展示名称可更改，Project 地址首期不可更名。全局唯一性由 Image DB 和 Harbor 同时校验，不能靠前端“检查可用”代替最终并发约束。Project 名不是安全身份，数据库中的映射才是领域关联。

Enable 只创建空间与运行身份；发布身份首次点击签发时创建。新增 ANI 用户不触发 Harbor 用户/Project 创建。Get/List 平台目录可以在租户空间未启用时使用；真正启动工作负载需要租户运行环境已准备。

## 5. 自动开通与有限重试

1. 校验可信租户、slug、幂等键。在短事务中预留空间与一条本领域 command，唯一约束解决同租户/同 slug 并发。
2. 对同一空间串行推进当前外部写操作；使用专用 pgx 连接上的 session advisory lock（有超时），**不得让 HTTP 调用处在长数据库事务中**。锁只是串行化辅助，不声称能给 Harbor 提供事务或 fencing。
3. 校验平台空间已存在且私有；创建租户私有 Project；拿到 Project ID 后立即持久化并校验后续 GET 的 name/id/私有属性。
4. 建立运行 Robot 候选、设置预持久化的 Secret、验证其权限精确等于预期，保存加密拉取材料。
5. 空间标记 available 并返回。仅 Project 已创建但凭证尚未就绪时不得返回 available。

没有后台 worker：同一请求在上游总时限内有限推进，遇到依赖暂不可用返回可重试错误；页面以**同幂等键**重试。状态可由 GetImageSpace 观察，不能新造 Network operation/worker。不得默认无限重试。

### 5.1 不确定创建结果

已有可信 Project ID 的重试可以自动校准。对于“创建请求已发送、响应及 ID 均未保存、同名 Project 现在存在”的情形，不能仅凭同名自动认领；Harbor ProjectReq 没有本方案可以假设的自定义 ownership 原子字段。

此时标记 `SPACE_OWNERSHIP_UNCONFIRMED`，保留请求/时间/受控日志证据；受控 `image-admin recover-project` 用显式 space_id、project_id 和核验记录完成绑定，不通过 Harbor 页面。正常租户初始化无需人工；这个罕见的不确定副作用需要核验，不能为自动化牺牲归属正确性。执行 Agent 不得自动确认不属于本 run 的 Project。

## 6. 发布凭证与 Secret 生命周期

仅保持一份**逻辑有效发布身份**。Issue 用于首次/已停用签发；Reset 用于替换当前身份；Disable 用于停止后续新认证。每次替换使用新 Robot generation，不对所有重置请求反复刷新同一个 Robot，避免迟到重试覆盖后来的密钥。

```text
短事务：command+候选代次+随机 Secret 密文
  → 创建候选 Robot（name/description 含 installation/space/purpose/generation/command）
  → RefreshSec 设置上述已保存 Secret
  → GET 验证限定权限、ID、用户名、有效期
  → 重置时停用上一代 Robot并确认
  → 短事务：切换有效身份、完成 command
  → 返回当前代次 Secret（仅本次签发响应/受限幂等重放）
```

H4 的 CreateRobot 由 Harbor 生成 Secret；本方案不虚构“创建接口支持指定 Secret”。客户端生成的稳定 Secret 用于后续 RefreshSec；该 API 使用指定 Secret 时响应可能不再回显，不能依赖响应 Secret 非空作为成功条件。密码满足实际 Harbor 强度规则，至少 32 个随机字节的编码并确保字符类，使用 crypto/rand，不用时间戳/UUID 当密码。

候选名称冲突必须校验完整 ownership description、预期 generation 和权限；未知身份不采用、不刷新、不停用。终态 command 不再发外部写；一个未解决的副作用 command 不允许被新 command 直接跳过。Robot generation 隔离迟到请求影响，仍不声称数据库/Harbor 跨系统 exactly-once。

Secret 存储由 `SecretCipher` 端口实现，adapter 使用标准库 AES-256-GCM，随机 nonce、外部 Secret 文件密钥及 key_id；AAD 绑定 installation、scope、tenant、space、purpose、generation/command。不得只 base64 保存，也不得将整条含 Secret 对象放入日志。

发布 Secret **普通 GET 永不返回**。同 actor、同方法、同幂等键、同指纹、仍是当前有效代次时，可在完成后 10 分钟内重放加密保存的签发结果；窗口外返回 `CREDENTIAL_DELIVERY_EXPIRED`，需要 Reset。UI 只在签发成功弹窗展示；这是可重试交付，不宣称严格网络“只传输一次”。重置/停用后旧签发结果不可再重放。到期密文机会清理，并提供受控清理命令；不保证从数据库备份中物理抹除。

运行 Secret 加密持久保存。有效期通过配置明确传入，示例发布 30 天、运行凭证 `-1` 永不过期（须同时开启 `allow_never_expires`，不默认修改 Harbor 全局期限）；必须返回/记录 expires_at，永不过期时以 `never_expires` 标记而非伪造远期时间。运维把运行凭证改成有限天数即进入“到期须换”的约束：自动轮换后置；受控运行凭证更换必须先准备新材料、更新唯一已绑定 Namespace 的 Secret，再停用旧身份，不得只改 Image 数据库。已有 Registry Token/进行中的传输可能持续到其授权到期，Disable 不承诺即时杀死现有 Token/上传。

## 7. 镜像登记

输入固定 Registry 的完整镜像引用；不接受任意 URL、userinfo、查询参数、fragment、外部 host、路径穿越或隐式 Docker Hub。首期只支持 Project 下单层 Repository，命名允许范围见 API；其他路径明确拒绝，不能错误截断。后续支持嵌套仓库只扩 parser/adapter，不改变登记对象。

登记步骤：可信 tenant→空间→先查同 command→验证引用所属→按 Tag/Digest 查询制品→取得根 Digest→再次按 Digest 查询确定元数据→验证为受支持容器清单→事务保存登记及幂等结果。Tag 在“查询与落库”间移动也不改变已解析内容；已完成的同键请求先重放，不重新解 Tag。

支持 OCI/Docker v2 image manifest，以及标准 image index/manifest list。保存根 Digest 和可运行平台列表；读取索引限深 2、子描述符上限 32、响应大小上限 4 MiB，忽略明确的附件/证明描述符，不把 unknown/unknown 声称成有效平台。禁止下载镜像层。不能确认有可运行平台时拒绝登记；非容器 OCI artifact 拒绝。实现方应以实际 Harbor artifact/child API 字段做定向解析，不造一套通用 OCI 制品平台。

目的、显示名称、说明、加速器声明可更新；owner、repository、Digest、平台事实不可通过 Update 改写。每个有效 `(space,repository,digest)` 唯一，重复登记返回 AlreadyExists。修改 Tag 后需登记新版本；取消登记不删除 Harbor 内容，不影响已创建任务保存的 Digest，但不能再用于新建任务。

平台目录和租户目录分开分页，无 ALL 跨租户模式。所有筛选在 SQL LIMIT 前应用；不能只过滤 Harbor 某一页。列表是“已登记资产”，不暗示完整仓库事实；Resolve 时重查根 Digest 存在且支持目标平台。Harbor 503 不等于镜像被删除，不自动清空记录。

## 8. 工作负载使用

创建方负责当前用户/租户授权和原业务意图，Image 不接管工作负载，也不读取 Network 私表。创建时解析已登记镜像并冻结获授权的 digest 引用，重试沿用原意图，不重新追随可变 Tag。

Instance 已通过进程内 EnsureTenantPullSecret 接入 Image 统一 Secret writer；独立服务的专用 mTLS 入口也提供该方法。统一 writer 根据经验证的 tenant、cluster、namespace 及原 Namespace UID 创建或更新共享拉取 Secret，这些调用路径仅返回 Secret 引用、代次及非敏感回执，不交付明文 Registry 凭据。标准 Namespace 与调用方范围按[租户合同](../contracts/tenant-namespace.md)校验，同名异主 Secret 拒绝覆盖。

独立运行身份仅经专用 mTLS listener 调用 ResolveImageForWorkload 与 EnsureTenantPullSecret，GetTenantPullMaterial 不在该入口允许的方法中；字段与身份以[Image API](image-api.md)为唯一合同。此限制不能推导为所有进程内 owner 已使用统一 writer。

Sandbox 当前由[组合根](../../cmd/ani-resource-service/sandbox.go)注入 sandboxPullMaterial，接口仍提供 GetTenantPullMaterial。[受理 adapter](../../internal/data/sandbox/image.go)使用材料检查凭据可用性，[Provider](../../internal/data/sandbox/provider.go)在进程内取得材料后，自行创建或更新 `ani-sandbox-pull-<tenant>` Secret。Sandbox 尚未迁到统一 writer；该进程内取材不表示独立服务已获明文 RPC 权限。统一 writer 的后续目标尚未覆盖此路径，本次文档修正不改变 Secret 命名、现有工作负载、接口、装配或权限，也不代表代码迁移完成。

工作负载模板只采用获授权的 digest、Secret 引用和既定拉取策略，保留自己的调度、网络与配额。Secret 代次变化按既有同步机制办理，不在每个 Pod 创建新 Robot。用户可直接建 Pod 时，部署方核实 AlwaysPullImages 或等价隔离前提；缺项按原合同报告，不擅改共享准入策略。

## 9. 工程配置与运行边界

`Bootstrap` 追加 `Image image = 3`（执行时检查实际 tag）；Image disabled 默认不改变 Network 启动、表、迁移或接口。新增配置：enabled、database_dsn_secret_file、harbor_management_url、registry_authority、harbor_ca_file、management_credential_file、encryption_key_file、cursor_key_file、installation_id、platform_project、request_timeout、publisher_duration_days、pull_duration_days、target_cluster_id。运行身份 listener 配置仅在独立创建方真实需要时增加，默认不监听。

管理 URL 固定 HTTPS，拒绝跨主机重定向；响应设大小限制、超时、禁止记录认证头/响应体。Registry 公网 authority 与管理 URL 可以不同，但必须显式映射为同一仓库实例。新 Image 写调用总限时建议 45s、单依赖调用 5s，查询 5s；不把现有 Network 超时一并修改。

Image 只在 Governance 受信入口注册租户 API；现有 full/vpc-read 未受信入口不得注册它。平台 CLI 与 image-migrate 互斥，保留现有 -migrate 的 Network 行为。Image 依赖故障返回域级 Unavailable；Harbor 探测不能让整个 Network 服务因启动网络波动退出。Image 开启但配置/数据库 schema 不合法应报告明确配置错误，不静默降级为无鉴权。

## 10. 可交付边界

后端技术验收用两个测试租户和一个平台制品验证 Push/Register/Resolve/Pull 与越权；产品验收再使用实际普通容器创建 API/页面。所有重任务 Fedora，清理只针对本 run。具体见计划、测试矩阵及执行手册。
