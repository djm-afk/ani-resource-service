# Sandbox 全新租户真实集群验收（2026-10-30）

66 集群 Resource `a778c1f25da0923272c6df9e7fbfb5ddce3abcf1`，镜像 `sha256:a88fdc6cedc4320a2220e88fc84a874313f0732635955d711db69b5549d0d53d`，gen102/Ready 1；Gov `6a8f766baf22f79a774797a0bee393b2fc6bd4ee`，镜像 `sha256:1a8560d698fdd87f6e4776beb154a9daa71ab68a257e30d2dd5f24f8ddecf7fe`，gen192/Ready 1。当时两仓分支均已推送主仓库。该记录只描述当时的候选版本，不代表当前 main 已现场验收。

新租户 51 自动准备共享 namespace `tenant-dcd7b3ef-46a3-4d42-a57c-dc87f5c7679c` 和 Sandbox 权限，Gov 记录 READY。未先创建 Instance；返回的 instance_id 为空。测试账号最初因脚本覆盖了后台登录权限而返回 403，给仅该租户角色补 `sys:access_backend` 后登录和 Image space 启用成功。

## 实际请求与响应

### create

`POST /api/v1/sandbox`

请求：

```json
{
  "data": {
    "idempotency_key": "sandbox-live-0370b8dd2a78-create",
    "name": "sandbox-live-0370b8dd2a78",
    "image_reference": "10.10.1.66:30003/ani-resource-factor10-20261022/python-runtime@sha256:ebac1666cc6e14b91314c4c8ae2be0c5bec5af07ebf26fac5075e55c33b516d5",
    "preset_id": "9f5a0625-1eab-4a65-95af-68962138ef86",
    "workspace_size": "5Gi"
  }
}
```

HTTP 200 响应：

```json
{
  "operation_id": "f0ec8a9e-51f3-5acc-9e44-d42ec744c57a",
  "resource_id": "6cfbae3b-3a66-5922-9336-60a7422fd6ce",
  "accepted": true,
  "replayed": false
}
```

### pause

`POST /api/v1/sandbox/6cfbae3b-3a66-5922-9336-60a7422fd6ce:pause`

请求：

```json
{
  "data": {
    "idempotency_key": "sandbox-live-0370b8dd2a78-pause",
    "expected_version": "1"
  }
}
```

HTTP 200 响应：

```json
{
  "operation_id": "883de9cc-a2f1-412e-a358-4d79c7b465a3",
  "resource_id": "6cfbae3b-3a66-5922-9336-60a7422fd6ce",
  "accepted": true,
  "replayed": false
}
```

### resume

`POST /api/v1/sandbox/6cfbae3b-3a66-5922-9336-60a7422fd6ce:resume`

请求：

```json
{
  "data": {
    "idempotency_key": "sandbox-live-0370b8dd2a78-resume",
    "expected_version": "2"
  }
}
```

HTTP 200 响应：

```json
{
  "operation_id": "2431cce7-9e4c-4aa1-9ac8-38487406e313",
  "resource_id": "6cfbae3b-3a66-5922-9336-60a7422fd6ce",
  "accepted": true,
  "replayed": false
}
```

### delete

`DELETE /api/v1/sandbox/6cfbae3b-3a66-5922-9336-60a7422fd6ce`

请求：

```json
{
  "data": {
    "idempotency_key": "sandbox-live-0370b8dd2a78-delete",
    "expected_version": "3"
  }
}
```

HTTP 200 响应：

```json
{
  "operation_id": "7fca2121-2a0b-4ab6-8746-3f890af3fc25",
  "resource_id": "6cfbae3b-3a66-5922-9336-60a7422fd6ce",
  "accepted": true,
  "replayed": false
}
```

创建操作最终 `succeeded`，观察态 `running`、版本 1；同一创建请求重放返回 `replayed=true` 和相同操作及 Sandbox ID。暂停操作 `succeeded`、`paused`、版本 2；恢复 `succeeded`、`running`、版本 3，Pod UID 由 `8cd237ae-4b26-4f9a-9f7b-3da35c2a427d` 变为 `89376bb7-1970-4375-8492-ef584440f092`，PVC UID `dc4b7493-608f-4461-992b-feda77b3cb9d` 保持。删除操作最终 `succeeded`、`deleted`、版本 4，Pod/PVC/PV 均已实际不存在。

租户清理第一次返回 409 `TENANT_SPACE_CLOSING`，状态 CLOSED 后重试 HTTP 200；DELETE 租户返回 409 `SANDBOX_HISTORY_PRESENT`，租户目前 OFF/CLOSED。共享 namespace、其中的 Sandbox role/binding 和 pull secret，以及 Image 项目 `t-sandbox-e2e-a30f26b491de`、套餐 9 和审计/历史记录保留；没有绕过历史保护手动删除数据库记录。测试密码文件已删除。关闭租户空间并不等于删除共享 namespace。

Network 未执行迁移；部署后只读校验仍为 23/23、无校验差异。Network 后台曾观察到一次 provider timeout，影响和原因未确认；不可据此断定全部 Network 功能健康。Gov 启动后 ModelDev GPU 用量投影恢复反复报告 `INVALID_QUOTA_REQUEST / noncanonical ModelDev GPU command`，与本次更新的关系和业务影响未确认，未修改相关代码。

本次只覆盖 image_reference 和创建/暂停/恢复/删除；image_id + image_scope、代码执行、文件和 token 等二期能力未在这个新租户重复验收。登录与租户/权限/套餐查询 HTTP 200；推理 AK 和所有其他业务接口未在本轮完成端到端验收。完整原始观测保存在原执行环境的仓外验收目录；本文件保留请求、响应、状态和清理结果，敏感凭据未写入。
