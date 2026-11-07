# VM 镜像 CDI 内部故障运维接替

本入口由集群运维负责，通过既有 kubercloud@10.10.1.66 SSH 连接使用
kubernetes-admin@ani-kcn-full context。它是限定故障维护说明，不授予 finalizer
写权限；实际责任接收、授权、对象 UID 和执行结果见本次[执行报告](../execution/records/VM-PHASE1-20261106/PHASE1-REPORT.md)。
未实际执行验证时，R09 不得判 PASS。

仅从平台已登记镜像出发，使用报告中的 namespace、source DV/PVC name/UID。
先经正式 Gov→Res Delete 使 source DV 进入原生删除，确认上传已结束。
只定向读取该链实际 ownerReferences 所指的 CDI PVC、uploader Pod，核对每跳
name/UID、终态、deletionTimestamp、阻塞 finalizer 和挂载关系。不通过全集群扫描
找“孤儿”，不操作其他用户资源。

若组件正常 GC 已完成，则等待平台正常观察关闭。若确认终态 uploader 的
foregroundDeletion 与 PVC protection 构成已知循环，先将具体 Pod UID、当前
resourceVersion、保留的其他 finalizer 及影响交给授权方审核。只有已有授权明确
覆盖此对象和动作才执行以下单对象 JSON Patch：

1. test /metadata/uid 为刚核对的 UID。
2. test /metadata/resourceVersion 为刚核对的版本。
3. test /metadata/finalizers 为完整已读列表。
4. replace 该列表，仅去掉 foregroundDeletion，保留全部其他项。

发生冲突重新读取并重新核对；运行中的 uploader、owner 链不完整、UID 不符或
原因未知均停止该维护动作，保留真实未完成原因。不得批量清空 finalizers、强删
运行 Pod、删除 PV/CSI 对象或把本操作加入 Res worker/常驻 helper。

处置后定向观察原生 GC、业务 DV/PVC/Job/上传请求与平台删除终态，记录必要 UID、
动作、时间及结果。旧已登记内部 Cleanup 逐项移交；业务对象仍留在正常删除集合，
历史 PV/CSI 与已移交内部引用可读。使用同一候选版本实际验证 failed uploader
场景，并保留 PR #65 的退休 CloneAbsent 引用释放回归。
