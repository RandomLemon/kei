package ratelimit

import (
	"context"
	"time"
)

// Wait 阻塞直到 key 可放行一个请求，或 ctx 结束。
//
// 用于发送链路这类「不允许丢消息」的场景；调用方必须传入可取消的 ctx，
// 否则在极端速率下会长时间等待。限流被禁用（rate <= 0）时立即返回 nil。
func (l *Limiter) Wait(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.rate <= 0 {
		return nil
	}
	for {
		if l.Allow(key) {
			return nil
		}
		d := l.retryAfter(key)
		if d <= 0 {
			d = time.Millisecond
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// retryAfter 返回 key 从现在起需要等待多久才可能拿到一个令牌。
//
// 估算前把令牌按当前时间补到 now（只读，不修改桶状态），因此不依赖调用方刚刚
// 调用过 Allow：陈旧快照会把等待算长（例如桶已空了半个周期时仍按整整一个令牌
// 计）。0 表示无需等待（桶不存在、令牌已够或限流未启用）。
func (l *Limiter) retryAfter(key string) time.Duration {
	if l == nil || l.rate <= 0 {
		return 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		return 0
	}
	tokens := b.tokens
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		tokens = min(l.burst, tokens+elapsed*l.rate)
	}
	missing := 1 - tokens
	if missing <= 0 {
		return 0
	}
	return time.Duration(missing / l.rate * float64(time.Second))
}
