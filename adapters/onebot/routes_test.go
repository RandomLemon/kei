package onebot

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRoutesAcceptServeMuxMetaCharacters 验证配置里的 path 只按字面量精确匹配：
// 含 "{" 等 ServeMux 元字符的路径不得在构造路由时 panic。
func TestRoutesAcceptServeMuxMetaCharacters(t *testing.T) {
	const path = "/hook/{token"
	a := &Adapter{
		topo:      topoHTTP,
		path:      path,
		authorize: func(*http.Request) bool { return true },
		log:       slog.New(slog.DiscardHandler),
	}

	h := a.routes(nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = path
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// GET 会让事件源 handler 返回 405；关键是这里没有 panic。
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("精确路径状态码 = %d, want 405", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = path + "/extra"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("子路径状态码 = %d, want 404", rec.Code)
	}
}
