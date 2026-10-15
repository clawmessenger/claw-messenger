package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// 产出文件 key 的形状校验：任何可能逃出目标目录的输入都必须在正则这一层被挡掉，
// 而不是靠后续的 filepath.Clean 兜底。
func TestClawFileKeyValidation(t *testing.T) {
	good := []string{
		"claw-files/user-u1/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293.html",
		"claw-files/node-rc_node_1234/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293",
		"claw-files/user-a/018F4A3B-6C1E-7A2B-9C3D-4E5F60718293.md",
	}
	for _, key := range good {
		if !validClawFileKey(key) {
			t.Fatalf("expected %q to be a valid key", key)
		}
	}
	bad := []string{
		"",
		"claw-files/user-u1/../../etc/passwd",
		"claw-files/user-u1/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293/../../x",
		"/claw-files/user-u1/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293",
		"claw-files/user-u1/not-a-uuid.html",
		"workspaces/abc/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293.html",
		"claw-files/user-u1/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293.html.exe.exe",
		"claw-files/user-u1\\018f4a3b-6c1e-7a2b-9c3d-4e5f60718293.html",
	}
	for _, key := range bad {
		if validClawFileKey(key) {
			t.Fatalf("expected %q to be rejected", key)
		}
	}
}

func newClawFileTestHandler() (*Handler, *mockStorage) {
	store := &mockStorage{}
	return &Handler{Storage: store}, store
}

// uploadThroughStore 直接打存储路径，绕开需要数据库的会话/设备鉴权：
// 本用例验证的是「存下去、key 形状对、URL 可读回」这三件事。
func uploadThroughStore(t *testing.T, h *Handler, scope, filename string, body []byte) map[string]interface{} {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("type", "file"); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.clawStoreUpload(w, req, scope, "file")
	if w.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Code int                    `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode upload response: %v (%s)", err, w.Body.String())
	}
	return envelope.Data
}

func TestClawUploadReturnsReadableKey(t *testing.T) {
	h, store := newClawFileTestHandler()
	html := []byte("<!doctype html><html><body>hi</body></html>")
	data := uploadThroughStore(t, h, "node-rc_node_1", "report.html", html)

	key, _ := data["key"].(string)
	if !validClawFileKey(key) {
		t.Fatalf("stored key %q must satisfy the reader's pattern", key)
	}
	if !strings.HasPrefix(key, "claw-files/node-rc_node_1/") || !strings.HasSuffix(key, ".html") {
		t.Fatalf("key layout wrong: %q", key)
	}
	if url, _ := data["url"].(string); url != "/api/claw/files/"+key {
		t.Fatalf("url = %q, want the API route for %q", url, key)
	}
	if size, _ := data["size"].(float64); int(size) != len(html) {
		t.Fatalf("size = %v, want %d", data["size"], len(html))
	}
	if typ, _ := data["type"].(string); !strings.HasPrefix(typ, "text/html") {
		t.Fatalf("type = %q, want text/html (extension override beats the sniffer)", typ)
	}
	if _, ok := store.files[key]; !ok {
		t.Fatal("file was not written to storage")
	}
}

// 读回口：坏 key 一律 404（不泄露存在性），好 key 200 且带上 iframe 预览需要的
// 安全头 —— 前端要把 PDF/HTML 内嵌进自己的页面，缺了这层会被全局 CSP 拦掉。
func TestClawServeFile(t *testing.T) {
	h, store := newClawFileTestHandler()
	store.files = map[string][]byte{}
	key := "claw-files/user-u1/018f4a3b-6c1e-7a2b-9c3d-4e5f60718293.html"
	store.files[key] = []byte("<html>x</html>")

	router := chi.NewRouter()
	router.Get("/api/claw/files/*", h.ClawServeFile)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := get("/api/claw/files/" + key); w.Code != http.StatusOK {
		t.Fatalf("serve status = %d, body = %s", w.Code, w.Body.String())
	} else {
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("content-type = %q, want text/html", ct)
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors") {
			t.Fatalf("preview CSP missing, got %q", csp)
		}
		if w.Body.String() != "<html>x</html>" {
			t.Fatalf("body = %q", w.Body.String())
		}
	}

	if w := get("/api/claw/files/claw-files/user-u1/../../etc/passwd"); w.Code != http.StatusNotFound {
		t.Fatalf("traversal attempt must 404, got %d", w.Code)
	}
	if w := get("/api/claw/files/claw-files/user-u1/missing.html"); w.Code != http.StatusNotFound {
		t.Fatalf("missing object must 404, got %d", w.Code)
	}
}
