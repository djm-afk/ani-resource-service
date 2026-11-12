# VM 查询兼容恢复｜2026-11-11

本次用户确认保留66当前数据库和ACC改造，前向修复VM查询，不整套回退r8。基线927a2a9112ec1a6bf0466ac100e03377447c672c，分支codex/res-vm-query-compat-20261111，WSL隔离工作树开发/构建，集成目标10.10.1.66（node1，context kubernetes-admin@ani-kcn-full）。

根因：已受理配置的展示调用严格的CreateSpec解码/新创建校验，r8的clone_source、expected_source_version及VM_CLONE未在当前新创建合同内，导致一条记录使整页失败。查询改为先保留正常严格投影，再仅在展示端解码已知字段，保留兼容标记。类型不可读则保留元数据与诊断，不隐藏资源。未改动新命令校验、数据库迁移、canonical bytes、摘要、GPU合同或证书/配置。尚未支持的克隆新建与执行不因本修复而获得支持。

验证：先新增回归，原实现失败（单条扩展记录使列表Unavailable）；修复后 go test ./internal/service/vm ./api/vm/v1 -count=1 通过，涵盖混合页、克隆详情、不可解码记录、已有reason、tenant拒绝、原始bytes保留及新克隆准入仍拒绝。受影响包 go vet、服务主程序 go build -trimpath 与 git diff --check 同时通过。镜像构建与真实部署/HTTP回执由完成后追加。

全量make verify/go test ./...未执行，遵循本会话定向验证范围。未进行VM电源、删除、克隆执行或共享设施变更。
