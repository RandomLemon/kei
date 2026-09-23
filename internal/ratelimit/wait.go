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

// retryAfter 返回 key 需要等待多久才可能拿到一个令牌。
func (l *Limiter) retryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		return 0
	}
	missing := 1 - b.tokens
	if missing <= 0 {
		return 0
	}
	return time.Duration(missing / l.rate * float64(time.Second))
}
