# VM 克隆原生权限说明

VM 克隆需要 `clone.kubevirt.io` 的 `virtualmachineclones` 权限。这里**不提供**
ClusterRole manifest，原因是该权限已并入集群上由 VM 环境搭建方手工维护的
`ani-vm-resource-20261025`；仓库新增第二个角色会与该角色形成两份授权，重放环境
清单时更容易失配。

## 授权来源

| 项 | 值 |
|---|---|
| 目标角色 | `ClusterRole/ani-vm-resource-20261025` |
| 绑定主体 | `ServiceAccount/ani-system/ani-resource-service`（ClusterRoleBinding `ani-vm-resource-20261025`） |
| 追加规则 | `apiGroups: ["clone.kubevirt.io"]`, `resources: ["virtualmachineclones"]`, `verbs: [get, list, watch, create, delete, patch]` |
| 变更时间 | 2026-11-10 |
| 变更方式 | 集群侧 `kubectl patch clusterrole --type=json`（未入仓） |
| 变更前备份 | 执行机 `/tmp/vm-clone-rbac-20261110/cr-ani-vm-resource-20261025.before.yaml` |

该角色不含 `virtualmachineclones/status` 与 `virtualmachineclones/finalizers`：
只读状态由主资源的 `get` 覆盖，平台不接管 finalizer。

## 复现环境时必须做的

新建或重放 VM 环境清单后，按上表再补一次该规则，否则克隆提交会 403。补完后核对：

```bash
for v in get list watch create delete patch; do
  kubectl auth can-i "$v" virtualmachineclones.clone.kubevirt.io \
    -n <tenant-namespace> --as=system:serviceaccount:ani-system:ani-resource-service
done
```

六项应全部为 `yes`。回滚时按变更前备份恢复该 ClusterRole。

## 为什么不用独立角色

曾经在仓内新增过 `deployments/vm/clone-rbac.yaml`（`ClusterRole/ani-resource-vm-clone`
加独立 Binding），已按方案 §4.1「不新建第二个 ClusterRole」撤回并删除。并入既有角色
可以避免同一 ServiceAccount 上有两份同源授权，代价是这条规则不在 git 中，需在本文件
与部署记录中留痕。
