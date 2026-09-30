//go:build linux

package manage

import (
	"strings"
	"testing"
)

func TestParseCPUInfo(t *testing.T) {
	const content = `processor	: 0
model name	: AMD Ryzen AI 9 HX 370 w/ Radeon 890M
physical id	: 0
cpu cores	: 12

processor	: 1
model name	: AMD Ryzen AI 9 HX 370 w/ Radeon 890M
physical id	: 0
cpu cores	: 12
`
	model, physical, logical := parseCPUInfo(content)
	if model != "AMD Ryzen AI 9 HX 370 w/ Radeon 890M" {
		t.Fatalf("model = %q", model)
	}
	if physical != 12 {
		t.Fatalf("physical = %d, 期望 12", physical)
	}
	if logical != 2 {
		t.Fatalf("logical = %d, 期望 2", logical)
	}
}

func TestParseCPUInfoMultiSocket(t *testing.T) {
	const content = "processor\t: 0\ncpu cores\t: 8\nphysical id\t: 0\nprocessor\t: 1\ncpu cores\t: 8\nphysical id\t: 1\n"
	if _, physical, logical := parseCPUInfo(content); physical != 16 || logical != 2 {
		t.Fatalf("physical=%d logical=%d, 期望 16/2", physical, logical)
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, err := parseLoadavg("1.34 0.68 0.36 4/1446 19401\n")
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if l1 != 1.34 || l5 != 0.68 || l15 != 0.36 {
		t.Fatalf("loadavg = %v/%v/%v", l1, l5, l15)
	}
	if _, _, _, err := parseLoadavg("1.0 2.0"); err == nil {
		t.Fatal("字段不足时应返回错误")
	}
}

func TestParseStatCPU(t *testing.T) {
	idle, total, err := parseStatCPU("cpu  1 2 3 4 5 6 7 8 9 10\ncpu0 1 2 3 4\n")
	if err != nil {
		t.Fatalf("parseStatCPU: %v", err)
	}
	if idle != 9 { // idle(4) + iowait(5)
		t.Fatalf("idle = %d, 期望 9", idle)
	}
	if total != 55 {
		t.Fatalf("total = %d, 期望 55", total)
	}
	if _, _, err := parseStatCPU("intr 1 2 3\n"); err == nil {
		t.Fatal("缺少 cpu 行时应返回错误")
	}
}

func TestParseMeminfo(t *testing.T) {
	const content = `MemTotal:       1000000 kB
MemFree:         200000 kB
MemAvailable:    400000 kB
Buffers:          10000 kB
Cached:           50000 kB
SwapTotal:       500000 kB
SwapFree:        400000 kB
`
	total, available, swapTotal, swapFree := parseMeminfo(content)
	if total != 1000000*1024 || available != 400000*1024 {
		t.Fatalf("内存 = %d/%d", total, available)
	}
	if swapTotal != 500000*1024 || swapFree != 400000*1024 {
		t.Fatalf("交换 = %d/%d", swapTotal, swapFree)
	}
}

func TestParseMeminfoAvailableFallback(t *testing.T) {
	const content = "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 20 kB\nCached: 30 kB\n"
	_, available, _, _ := parseMeminfo(content)
	if available != 150*1024 {
		t.Fatalf("available = %d, 期望 150 KiB", available)
	}
}

func TestParseMounts(t *testing.T) {
	entries := parseMounts("/dev/sda1 /mnt/my\\040disk ext4 rw 0 0\nproc /proc proc rw 0 0\nbadline\n")
	if len(entries) != 2 {
		t.Fatalf("解析出 %d 行, 期望 2", len(entries))
	}
	if entries[0].mountpoint != "/mnt/my disk" {
		t.Fatalf("八进制转义未还原: %q", entries[0].mountpoint)
	}
	if entries[0].device != "/dev/sda1" || entries[0].fstype != "ext4" {
		t.Fatalf("解析结果 = %+v", entries[0])
	}
}

func TestIsCardDir(t *testing.T) {
	cases := map[string]bool{"card0": true, "card12": true, "card0-DP-1": false, "card": false, "cardX": false, "renderD128": false}
	for in, want := range cases {
		if got := isCardDir(in); got != want {
			t.Fatalf("isCardDir(%q) = %v, 期望 %v", in, got, want)
		}
	}
}

// TestStatusCommand 走真实 Linux 主机采集路径（/proc、/sys 恒存在），
// 只断言与主机无关的结构性前缀，不断言具体数值。
func TestStatusCommand(t *testing.T) {
	reg := setup(t, nil, nil)
	got := invoke(t, reg, "status")

	for _, want := range []string{"主机状态：", "CPU：", "CPU 使用率：", "内存：", "磁盘：", "GPU："} {
		if !strings.Contains(got, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, got)
		}
	}
}
