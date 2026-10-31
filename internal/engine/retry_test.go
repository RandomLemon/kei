package engine

import (
	"testing"
	"time"
)

// TestNextSendBackoff 验证退避倍增并封顶，且大值不会溢出为 0/负数。
func TestNextSendBackoff(t *testing.T) {
	tests := []struct {
		current time.Duration
		want    time.Duration
	}{
		{100 * time.Millisecond, 200 * time.Millisecond},
		{10 * time.Second, 20 * time.Second},
		{20 * time.Second, maxSendBackoff},
		{maxSendBackoff, maxSendBackoff},
		{time.Duration(1 << 62), maxSendBackoff},
	}
	for _, tc := range tests {
		if got := nextSendBackoff(tc.current); got != tc.want {
			t.Errorf("nextSendBackoff(%v) = %v, want %v", tc.current, got, tc.want)
		}
	}
}
