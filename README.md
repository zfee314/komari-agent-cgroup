# komari-agent cgroup 补丁

修复 komari-agent 在容器环境中上报宿主机而非容器配额的 bug。

## 问题

`monitoring/unit/mem.go` 直接读 `/proc/meminfo`，`cpu.go` 用 gopsutil 读 `/proc/cpuinfo`，
容器里这些全是宿主机的数据。代码中零 cgroup 感知。

在容器（如 botkeep 的 0.5核/512MB）中运行时，面板显示的是宿主机整机的内存和 CPU，数据完全错误。

## 补丁内容

- **cgroup.go**（新增）：cgroup v1/v2 检测，从 `memory.max`/`memory.limit_in_bytes` 读内存上限，
  `memory.current`/`memory.usage_in_bytes` 读实际用量，`cpu.max`/`cpu.cfs_quota_us` 算 CPU 配额，
  `cpu.stat` 的 `usage_usec` 差值算 CPU 使用率
- **mem.go**：`Ram()` 在 cgroup 限制 < 宿主机总量时，用 cgroup 口径修正 Total/Used
- **cpu.go**：`Cpu()`/`CpuStaticInfo()` 优先用 cgroup 口径的使用率和核数

## 使用

```bash
# 把 cgroup.go 放到 monitoring/unit/ 目录
# 用下面的补丁修改 mem.go 和 cpu.go（见 PATCH.diff）
# 然后编译：
go build -o komari-agent .
```

部署时建议加 `--disable-auto-update`，否则会被官方自动更新覆盖。
