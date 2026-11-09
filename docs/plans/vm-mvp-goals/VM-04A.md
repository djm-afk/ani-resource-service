# VM-04A 在线和离线快照及完整性展示

目标：完成VM-04A；本轮按docs/plans/vm-mvp-complete-goal-20261023.md及用户执行指令连续推进16卡，完成后按依赖继续。共享变更和正式发布仍须具体授权。

先读：docs/START-HERE.md#vm-mvp、docs/specs/vm-mvp.md、docs/contracts/vm-v1.md、docs/plans/vm-mvp.md中本卡、docs/verification/vm-mvp-acceptance.md；进入实际仓库后读AGENTS、START-HERE/CONTEXT及适用remote-execution；本轮重任务仅ssh fedora、集群仅172.16.101.10～12，Git传输使用SSH。若已将正式文件接入仓库，用其唯一最新适用版本，不另抄第二份规则。

前置：VM-03A/VM-03B的持久盘合同，VM-02B的Windows；Snapshot API/controller/driver/gate前置满足。

主责：Resource + Gov + 前端

唯一规则范围：S09–S10；C03。

允许修改位置：vm snapshot记录/native adapter、Create/Get/List/DeleteSnapshot、Gov/UI快照与一致性展示。

具体工作：

1. 只创建原生VirtualMachineSnapshot，冻结source VM UID与当前持久root/data逻辑盘清单；等待原生结果，核对included/excluded。
2. 完整性、Agent连接/quiesce与原生indications分别展示，必要盘缺失不得标完整可恢复。
3. Linux/Windows各覆盖在线/离线快照，系统盘+至少两块持久数据盘；无Agent路径明确不保证冻结，不暗中关机。
4. 删除快照完成本服务原生对象与引用的管理责任；Content/CSI 回收按组件策略处理，不作 operation 完成前置，不逐盘编排或删除其他快照数据。

产出：快照API/UI、逻辑盘inventory与完整性合同、双系统快照结果。

必须验证：V18、V23快照归属、V22快照清理。

完成条件：在线/离线覆盖和失败展示通过；能恢复的数据证明在VM-04B闭合，不能提前宣布全功能快照验收完成。

禁止扩展：不提供RAM恢复、备份调度、group snapshot保证、数据库事务一致保证或自有CSI快照引擎。

执行约束：保留无关未提交修改；真实环境先核身份和获准对象范围。device_ownership_from_security_context的共享runtime变更仅VM-00R在用户后来确认空闲并明确授权后可执行，任何其他卡不得顺手修改。前置不满足时完成可独立进行的代码/隔离验证，记录准确阻塞，不换Filesystem或共享网络制造通过。

报告：按docs/plans/vm-mvp-templates/task-report.md提供变更文件、实际源码/host/命令/退出码、验收ID的PASS/FAIL/NOT_RUN、清理结果与下一张可执行卡；更新既有唯一状态，按完整Goal依赖连续推进下一张可执行卡。未跑现场要明确NOT_RUN，不能将本卡代码完成等同一期产品验收。
