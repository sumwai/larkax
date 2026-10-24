// Package webui 把编译好的控制台静态目录挂到公开监听上。
//
// 控制台是 Vite 产物：一份 index.html、一份带内容哈希的 assets/ 目录，其余是
// 前端路由。它不参与推理链路，也不读数据库，所以这里只做三件事——找文件、
// 设缓存、把不认识的路径交给 index.html。
package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// inferencePrefix 是交给推理入口的路径前缀。
//
// 这一条是挂载顺序的支点：静态目录挂在 "/"，而推理入口原先也挂在 "/"。
// 两者同挂时后注册的那个赢得不了什么——ServeMux 按前缀长度选，同长度先注册
// 的赢——于是静态目录会把 /v1/chat/completions 一起吃掉。所以进静态处理器
// 之后先按这个前缀分流，推理请求原样交回去。
const inferencePrefix = "/v1/"

// Handler 返回挂在 "/" 上的处理器：/v1/ 交给 inference，其余按静态文件、
// 前端路由回退处理。
//
// 目录里没有 index.html 时返回错误而不是先跑起来：那种状态下每个页面请求都是
// 404，而服务本身完全正常——「部署漏了控制台产物」会被当成前端坏了去查。
func Handler(dir string, inference http.Handler) (http.Handler, error) {
	if inference == nil {
		return nil, errors.New("webui.Handler 需要推理入口，得到 nil")
	}
	info, err := os.Stat(filepath.Join(dir, "index.html"))
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("控制台目录 %q 里没有 index.html：Dockerfile 漏了 web 产物，或者 LARKAX_WEB_DIR 指错了", dir)
	}
	root := os.DirFS(dir)
	return &handler{
		root:  root,
		files: http.FileServerFS(root),
		api:   inference,
	}, nil
}

type handler struct {
	root  fs.FS
	files http.Handler
	api   http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, inferencePrefix) {
		h.api.ServeHTTP(w, r)
		return
	}
	// 静态目录与前端路由只读：写请求要在这里结束，否则它会被当成「找不到的
	// 前端路由」拿到一份 200 的 index.html，看起来像写成功了。
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if info, err := fs.Stat(h.root, name); err == nil && !info.IsDir() {
		h.setCacheControl(w, name)
		h.files.ServeHTTP(w, r)
		return
	}

	// 带扩展名的路径不回落：/assets/index-abc.js 缺了就是缺了，给一份 HTML
	// 会让浏览器在「脚本语法错误」上排查，而真实原因是产物没上传。
	if path.Ext(name) != "" {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

// setCacheControl 按文件种类设缓存。
//
// assets/ 下的文件名带内容哈希（Vite 的 assets/[name]-[hash].js），同一个 URL
// 的内容不会变，可以长缓存；index.html 恰恰相反，它指向当前那一批哈希文件名，
// 缓存住就会让用户在新版本发布后继续加载旧的 assets——白屏，且刷新也无效。
func (h *handler) setCacheControl(w http.ResponseWriter, name string) {
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(h.root, "index.html")
	if err != nil {
		http.Error(w, "控制台入口读不出来", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
