# 实例只读诊断权限

[diagnostics-rbac.yaml](diagnostics-rbac.yaml) 是对现有 Resource Kubernetes 身份的最小诊断权限补充，应用前替换实际实例 Namespace，并由部署负责人绑定至该服务原有身份；不绑定至 workspace ServiceAccount。不新增 exec、Secret 内容读取、任意日志路径、清理或写权限。

该文件不代替现有实例创建/工作区权限，也不自动创建 Namespace 或发布服务。运行目标固定来自可信租户实例数据库中的 cluster/namespace，经 Notebook → StatefulSet → Pod UID 核验，当前单集群使用部署唯一连接。

[network-template-rbac.yaml](network-template-rbac.yaml) 单独授予 Resource 在配置的租户实例 Namespace 删除 StatefulSet 的权限，用于手动主网卡变更在安全停止、零副本且无受控 Pod 后，按原 UID/RV 退休旧模板并等待 Notebook controller 重建；不扩展 diagnostics 读角色，也不删除 Notebook、PVC 或 VNIC。

GPU 使用新增 priority 引用且实际 Pod 指定 Volcano PodGroup 时，补充 `get podgroups.scheduling.volcano.sh` 以核对冻结 priority 与 Group 一致；没有新增引用不读取或修改 Group。该权限不允许创建/修改 PodGroup。

日志专用 HTTP client 不使用普通 Kubernetes 15 秒整体超时；开流等待默认 15 秒，连接默认 10 分钟，单帧 16 KiB，总缓冲上限 256 KiB，发送等待 10 秒，进程/租户/实例并发上限为 100/10/3。Service.LogLimits 可管理这些上限；普通 unary 和 Kubernetes 读取期限保留。Gov 心跳和 SSE 传输由其专用入口负责。
