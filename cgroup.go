package monitoring

// cgroup.go - 容器感知的资源限制检测补丁
// 解决 komari-agent 在容器/受限 cgroup 环境中上报宿主机而非容器配额的问题
//
// 原理：
//   - 内存：优先读取 cgroup 的 memory.max (v2) / memory.limit_in_bytes (v1)
//     作为 Total，memory.current (v2) / memory.usage_in_bytes (v1) 作为 Used
//   - CPU：读取 cpu.max (v2) / cpu.cfs_quota_us+cpu.cfs_period_us (v1)
//     算出可用核数；CPU 使用率通过 cpu.stat 的 usage_usec 差值计算

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cgroupBase 返回当前进程所属 cgroup 在 /sys/fs/cgroup 下的挂载路径
func cgroupBase() string {
	// 优先尝试统一层级直接路径（容器内通常直接可见）
	// 其次解析 /proc/self/cgroup 拿到相对路径
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/sys/fs/cgroup"
	}
	// cgroup v2: "0::/kubepods/xxx" ; v1: "2:memory:/docker/xxx"
	best := ""
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		// v2 统一层级
		if parts[1] == "" {
			p := filepath.Join("/sys/fs/cgroup", parts[2])
			// 验证 memory.max 或 memory.current 是否存在
			if cgroupFileExists(filepath.Join(p, "memory.max")) || cgroupFileExists(filepath.Join(p, "memory.current")) {
				return p
			}
			if best == "" {
				best = p
			}
		}
	}
	if best != "" {
		return best
	}
	return "/sys/fs/cgroup"
}

func cgroupFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readUintFile(p string) (uint64, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	// "max" 表示无限制
	if s == "max" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// CgroupMemoryLimit 返回 cgroup 内存上限（字节），0 表示无限制或非容器环境
func CgroupMemoryLimit() uint64 {
	base := cgroupBase()

	// cgroup v2
	if v, err := readUintFile(filepath.Join(base, "memory.max")); err == nil && v > 0 {
		return v
	}
	// cgroup v1
	if v, err := readUintFile(filepath.Join(base, "memory.limit_in_bytes")); err == nil && v > 0 {
		// v1 无限制时是个接近 2^63 的巨大值，过滤掉
		if v < 1<<62 {
			return v
		}
	}
	// 兜底：直接在 /sys/fs/cgroup 根下找（某些容器挂载方式）
	if base != "/sys/fs/cgroup" {
		if v, err := readUintFile("/sys/fs/cgroup/memory.max"); err == nil && v > 0 {
			return v
		}
		if v, err := readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil && v > 0 && v < 1<<62 {
			return v
		}
	}
	return 0
}

// CgroupMemoryUsage 返回 cgroup 当前内存用量（字节），0 表示读不到
func CgroupMemoryUsage() uint64 {
	base := cgroupBase()

	// cgroup v2: memory.current（包含 page cache；与 htop 口径接近已足够）
	if v, err := readUintFile(filepath.Join(base, "memory.current")); err == nil && v > 0 {
		return v
	}
	// cgroup v1
	if v, err := readUintFile(filepath.Join(base, "memory.usage_in_bytes")); err == nil && v > 0 {
		return v
	}
	if base != "/sys/fs/cgroup" {
		if v, err := readUintFile("/sys/fs/cgroup/memory.current"); err == nil && v > 0 {
			return v
		}
		if v, err := readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// CgroupCPUCount 返回 cgroup 允许的 CPU 核数，0 表示无限制或读不到
func CgroupCPUCount() float64 {
	base := cgroupBase()

	// cgroup v2: cpu.max 格式为 "$MAX $PERIOD" 或 "max $PERIOD"
	if data, err := os.ReadFile(filepath.Join(base, "cpu.max")); err == nil {
		fields := strings.Fields(strings.TrimSpace(string(data)))
		if len(fields) == 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseUint(fields[0], 10, 64)
			period, err2 := strconv.ParseUint(fields[1], 10, 64)
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				return float64(quota) / float64(period)
			}
		}
	}
	// cgroup v1: cpu.cfs_quota_us / cpu.cfs_period_us
	readV1 := func(dir string) float64 {
		quota, err1 := readUintFile(filepath.Join(dir, "cpu.cfs_quota_us"))
		period, err2 := readUintFile(filepath.Join(dir, "cpu.cfs_period_us"))
		// quota == -1（读作超大值）表示无限制；readUintFile 对负数解析失败，直接走 err 分支
		if err1 == nil && err2 == nil && period > 0 && quota > 0 {
			return float64(quota) / float64(period)
		}
		return 0
	}
	if n := readV1(base); n > 0 {
		return n
	}
	if n := readV1(filepath.Join(base, "cpu")); n > 0 {
		return n
	}
	if base != "/sys/fs/cgroup" {
		if n := readV1("/sys/fs/cgroup"); n > 0 {
			return n
		}
		if n := readV1("/sys/fs/cgroup/cpu"); n > 0 {
			return n
		}
	}
	return 0
}

// ---- cgroup CPU 使用率（基于 usage_usec 差值）----

var cgroupCPULast struct {
	sync.Mutex
	usageUsec uint64
	t         time.Time
	set       bool
}

func readCgroupCPUUsageUsec() (uint64, bool) {
	base := cgroupBase()
	tryFiles := []string{
		filepath.Join(base, "cpu.stat"),          // v2
		filepath.Join(base, "cpuacct.usage"),     // v1 (纳秒)
		"/sys/fs/cgroup/cpu.stat",
		"/sys/fs/cgroup/cpu/cpuacct.usage",
	}
	for _, p := range tryFiles {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		// v2 cpu.stat 里找 usage_usec
		if strings.Contains(s, "usage_usec") {
			for _, line := range strings.Split(s, "\n") {
				f := strings.Fields(line)
				if len(f) == 2 && f[0] == "usage_usec" {
					if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
						return v, true
					}
				}
			}
		} else {
			// v1 cpuacct.usage 是纯数字纳秒
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				return v / 1000, true // 转微秒
			}
		}
	}
	return 0, false
}

// CgroupCPUUsage 返回容器 CPU 使用率（相对 cgroup 配额的百分比，0-100+）。
// ok=false 表示读不到 cgroup 数据，调用方应回退到原有逻辑。
func CgroupCPUUsage() (usage float64, ok bool) {
	usageUsec, found := readCgroupCPUUsageUsec()
	if !found {
		return 0, false
	}
	cpus := CgroupCPUCount()
	if cpus <= 0 {
		// 无配额限制时无法算相对值，返回 false 让调用方回退
		return 0, false
	}
	now := time.Now()

	cgroupCPULast.Lock()
	defer cgroupCPULast.Unlock()
	if !cgroupCPULast.set {
		cgroupCPULast.usageUsec = usageUsec
		cgroupCPULast.t = now
		cgroupCPULast.set = true
		return 0, false // 首次采样无差值，回退一次
	}
	dUsage := float64(usageUsec) - float64(cgroupCPULast.usageUsec)
	dTime := now.Sub(cgroupCPULast.t).Seconds()
	cgroupCPULast.usageUsec = usageUsec
	cgroupCPULast.t = now

	if dTime <= 0 || dUsage < 0 {
		return 0, false
	}
	// usage_usec 是该 cgroup 所有 CPU 时间总和，除以 (核数*时间) 得利用率
	usage = dUsage / 1e6 / (cpus * dTime) * 100
	if usage < 0 {
		usage = 0
	}
	return usage, true
}
