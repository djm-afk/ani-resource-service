# Storage 部署权限与租户开通清单

本目录交付 Storage 模块访问 Kubernetes 所需的 RBAC，以及命名空间级绑定的手工开通流程。
它只覆盖 Res 对 Kubernetes 的部署权限，**不涉及** Gov 对用户的 action 权限与套餐模块。

## 交付顺序（不可跳步）

1. 应用角色规则与集群级绑定：
   ```bash
   kubectl apply -f deployments/storage/rbac.yaml
   ```
2. 按下方开通清单，为每个租户命名空间逐条创建 RoleBinding。
3. 确认运行中的 Res Pod 使用 `ani-system/ani-resource-service` 身份：
   ```bash
   kubectl get deploy ani-resource-service -n ani-system \
     -o jsonpath='{.spec.template.spec.serviceAccountName}{"\n"}'
   ```
4. 用该运行身份逐项核对权限（管理员身份不能替代）：
   ```bash
   SA=system:serviceaccount:ani-system:ani-resource-service
   kubectl auth can-i get namespaces --as=$SA
   kubectl auth can-i get storageclasses.storage.k8s.io --as=$SA
   kubectl auth can-i list storageclasses.storage.k8s.io --as=$SA
   kubectl auth can-i get persistentvolumes --as=$SA
   kubectl auth can-i get persistentvolumeclaims -n <租户命名空间> --as=$SA
   kubectl auth can-i list persistentvolumeclaims -n <租户命名空间> --as=$SA
   kubectl auth can-i create persistentvolumeclaims -n <租户命名空间> --as=$SA
   kubectl auth can-i update persistentvolumeclaims -n <租户命名空间> --as=$SA
   kubectl auth can-i delete persistentvolumeclaims -n <租户命名空间> --as=$SA
   kubectl auth can-i list pods -n <租户命名空间> --as=$SA
   kubectl auth can-i list events -n <租户命名空间> --as=$SA
   kubectl auth can-i list notebooks.kubeflow.org -n <租户命名空间> --as=$SA
   # 反向确认：必须为 no
   kubectl auth can-i create rolebindings -n <租户命名空间> --as=$SA
   ```
   Notebook 只有 `get`、没有 `list` 不算通过。

## 开通清单模板

租户命名空间必须预先符合[统一合同](../../docs/contracts/tenant-namespace.md)（`tenant-<canonical resource UUID>`），Storage 只精确读取与校验，**不创建、不改标签**。
每个租户命名空间必须有一条 RoleBinding：

| Gov 租户 ID | Res 租户 UUID | 命名空间 | RoleBinding 已创建 | 核对时间 | 操作人 |
|---|---|---|---|---|---|
| | | | | | |

创建命令（运维/管理员身份执行，不经过 Res）：

```bash
kubectl create rolebinding ani-storage-tenant \
  --clusterrole=ani-storage-tenant \
  --serviceaccount=ani-system:ani-resource-service \
  -n <租户命名空间>
```

核对命令：

```bash
kubectl get rolebinding ani-storage-tenant -n <租户命名空间> \
  -o jsonpath='{.roleRef.kind}/{.roleRef.name} {.subjects[0].kind}/{.subjects[0].namespace}:{.subjects[0].name}{"\n"}'
# 期望：ClusterRole/ani-storage-tenant ServiceAccount/ani-system:ani-resource-service
```

存量租户上线前逐项核对；新增租户在首次使用存储前完成一次手工绑定。
清单与执行结果作为部署记录保留，不能只验证新租户。

## 未开通时的页面表现

命名空间已存在但没有 RoleBinding 时，Res 的 Kubernetes 调用返回 Forbidden，接口返回
`STORAGE_ACCESS_DENIED`。页面必须显示"该租户存储权限未开通，请联系管理员"，
**不能显示"稍后重试"**——本期不存在自动补绑，等待不会恢复。

## 后续归属

租户管理的"开通租户空间"功能将统一承担命名空间创建、归属标签与租户级权限（含本
RoleBinding），届时取消手工步骤。本期不实现该功能，也不给 Res 授予 `rolebindings` 权限。

## 对象存储配置

`object_storage.managed_buckets` 是管理员预置桶：新增/删除仍需修改 Res 配置并**重启 Res**，不支持热更新。

`object_storage.tenant_bucket_prefix` 非空时开启租户建桶：物理桶名为
`<prefix><sha256(cluster_id|tenant_uuid)[:20]>-<逻辑名>`，建桶不需要改配置或重启，
逻辑名仅接受 S3 合法小写 ASCII（1–32 字符）。为空时只展示配置桶并关闭租户建桶，不关闭配置桶的对象写入。同一 S3
账号不应被多个 Res 集群共用而未配置稳定 `cluster_id`，否则租户桶指纹会变化。

S3 服务账号至少需要：`s3:ListAllMyBuckets`、`s3:CreateBucket`、
`s3:ListBucket`、`s3:GetObject`、`s3:PutObject`、`s3:DeleteObject`；HeadBucket 使用 ListBucket 权限。上传/下载的预签名 URL 使用
`browser_endpoint`，必须由浏览器可达且证书受终端信任；浏览器上传还需要桶级 CORS
（允许控制台 Origin 的 `PUT/GET/HEAD` 与 `content-type` 头，暴露 `ETag`）。
`browser_endpoint` 为空时元数据仍可浏览/删除，上传与下载返回 503。
