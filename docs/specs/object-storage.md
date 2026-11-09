# 对象存储管理合同

本合同说明 Storage 的 S3 直接对象管理能力，遵循[全域原则](res-responsibility-and-simplification.md)。传输和受信租户见[调用登记](governance-calls.md#存储)，服务配置/权限见[部署说明](../../deployments/storage/README.md#对象存储配置)，实际实现与验收只见[执行状态](../execution/status.md)。

## 对象、身份与授权

Res 管理允许范围内的桶映射、租户建桶、单个 key 的查询/删除及短期预签名请求。S3 是对象存在、ETag、大小和直接请求结果的权威；Res 不证明后端副本、磁盘擦除或分布式存储内部回收。这里不强加 Kubernetes UID/Namespace、operation、lease 或后台工作框架。

受信 tenant 必须为规范非零 UUID；用户提交逻辑桶 ID/key，不能自由指定其他租户物理桶、服务 endpoint 或管理凭据。受管桶由配置显式映射 tenant/逻辑 ID/物理名；租户桶由稳定 cluster_id、tenant UUID 指纹和合法逻辑名确定。重复物理映射或同一逻辑 ID 的歧义按业务冲突处理，不借同名自动扩大范围。

managed_buckets 配置变更按部署合同重启生效；tenant_bucket_prefix 非空才开放租户建桶。关闭建桶不关闭已有配置桶的对象能力；不以新默认或显示名猜测接管旧物理桶。

## 桶目录、创建与统计

ListBuckets 仅返回当前租户配置桶和合法租户前缀内的桶，不公开其他租户列表。启用租户桶时正常查询 S3 桶目录来解析该业务集合，不属于下游清场扫描。目录失败返回错误，不清空映射或伪造空集合。

CreateBucket 校验逻辑名、租户范围、配置保留名与既有租户桶数量，使用确定物理名创建；同一租户同名的合法已存在桶按相同目标返回。该能力不提供删除桶、递归清桶或跨租户收养。并发数量限制按实际实现与底层行为核实，不声称一次无锁计数提供跨请求强原子限额。

BucketStats 按需累加对象数量和字节，当前有界扫描上限由实现定义；Truncated=true 表示仅观察到下限，Scanned 是实际采样数量，不能当成精确全量、配额账本或管理完成依据。无数据、错误、部分结果与真实零值分别处理。

## 单 key 查询与分页

ListObjects 先按当前受信 tenant 解析允许的物理桶，再按 prefix、limit/cursor 返回结果；现有 cursor 封装绑定逻辑 bucket_id 与 prefix，并携带 S3 continuation token，不是授权凭据。不匹配时拒绝，不能靠 cursor 选择其他租户桶；当前编码不包含 tenant 或签名，不声称游标自身提供租户认证。HeadObject 返回该 key 当前公开元数据，详情/下载不推导其他版本或历史副本。过滤、排序、分页与字段以生成 API 为准；列表错误不当作 key 不存在。

## 上传和下载交接

GetUploadURL 校验桶归属、key、内容类型、声明大小及覆盖选项，签发绑定单个 key 的短期 PUT URL 和必需 headers；文件由客户端直传 S3。成功交付 URL 只代表一次上传能力已交接，不表示对象已经上传、大小/内容已验证或业务导入完成，Res 不记录虚假上传成功。

不覆盖时使用当前对象查询及原生条件写入保护；不能只凭预查询认定后续无并发。覆盖和并发结果按实际 S3 条件语义返回。URL 过期是该能力自身的必要时限，不是 provider proof TTL。

GetDownloadURL 校验当前授权桶/key 并使用实际 browser_endpoint，短期签名只授权该对象；服务端 endpoint 与浏览器可达地址分开。缺 browser_endpoint 时仍可进行合同允许的元数据/删除操作，上传/下载明确不可用。临时 URL、访问材料和内容不进入公开日志或历史证据。

## 删除与错误

DeleteObject 仅删除一个 key，允许 expected_etag 保护原目标。ETag 是原生对象/并发条件，不是底层正确性证明；发生不匹配或 412 时报告身份/前提冲突，不能删另一个已替换内容。明确 key 不存在返回 GONE，直接 DELETE 成功返回 DELETED；含义限于当前 S3 API 的对象管理结果，不承诺所有历史版本或物理字节已擦除。

HeadBucket、HeadObject 和 DELETE 的超时、403、连接错误或其他失败不当作 NotFound；不根据失败清空桶、递归删除或再建同名对象猜结果。错误分类由[adapter](../../internal/data/storage/s3.go)和[用例](../../internal/biz/storage/usecase.go)约束；404、访问拒绝、ETag 前提失败、不可用及请求不合法保持区别。

没有全域统一的上传历史证明库、配额项目、owner 审批或常驻扫描。各消费业务如需持久化上传结果、引用或导入责任，应在自己的已授权合同内完成交接，不以短期 URL 签发代替真实业务完成。
