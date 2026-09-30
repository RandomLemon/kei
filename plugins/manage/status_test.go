package manage

import (
	"strings"
	"testing"
)

func TestFormatHostInfo(t *testing.T) {
	const gib = 1 << 30
	h := hostInfo{
		cpuModel:     "Test CPU",
		cpuPhysical:  2,
		cpuLogical:   4,
		cpuUsage:     0.5,
		load1:        1,
		load5:        2,
		load15:       3,
		memTotal:     8 * gib,
		memAvailable: 6 * gib,
		swapTotal:    4 * gib,
		swapFree:     3 * gib,
		disks: []diskInfo{{
			device: "/dev/sda1", fstype: "ext4", mountpoint: "/",
			total: 100 * gib, used: 25 * gib, available: 75 * gib,
		}},
		gpus: []gpuInfo{{name: "Test GPU", vendorID: "0x10de", deviceID: "0x1234"}},
	}

	got := formatHostInfo(h)
	for _, want := range []string{
		"主机状态：",
		"CPU：Test CPU（2 核 4 线程）",
		"CPU 使用率：50.0% · 负载 1.00 / 2.00 / 3.00（1/5/15 分钟）",
		"内存：已用 2.0 GiB / 8.0 GiB（25.0%）· 可用 6.0 GiB",
		"交换：已用 1.0 GiB / 4.0 GiB（25.0%）",
		"- /dev/sda1（ext4）已用 25.0 GiB / 100.0 GiB（25.0%）· 挂载 /",
		"- Test GPU（0x10de:0x1234）",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, got)
		}
	}
}

func TestFormatHostInfoMissingData(t *testing.T) {
	got := formatHostInfo(hostInfo{cpuUsage: -1})
	for _, want := range []string{
		"CPU：未知型号",
		"CPU 使用率：不可用",
		"内存：不可用",
		"交换：无",
		"磁盘：无可用信息",
		"GPU：未检测到",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, got)
		}
	}
}

func TestFormatHostInfoMergedMounts(t *testing.T) {
	h := hostInfo{cpuUsage: -1, disks: []diskInfo{{
		device: "/dev/nvme0n1p2", fstype: "btrfs", mountpoint: "/",
		extraMounts: 5, total: 1 << 40, used: 1 << 39, available: 1 << 39,
	}}}
	if got := formatHostInfo(h); !strings.Contains(got, "挂载 / 等 6 处") {
		t.Fatalf("多挂载点未合并展示:\n%s", got)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 << 30, "3.0 GiB"},
		{1 << 40, "1.0 TiB"},
		{1 << 50, "1.0 PiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Fatalf("formatBytes(%d) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestPercent(t *testing.T) {
	if got := percent(1, 0); got != 0 {
		t.Fatalf("percent(1, 0) = %v, 期望 0", got)
	}
	if got := percent(1, 4); got != 25 {
		t.Fatalf("percent(1, 4) = %v, 期望 25", got)
	}
}
