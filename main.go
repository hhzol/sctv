package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ========== 配置 ==========
const (
	getSecretURL         = "https://gw.scgchc.com/app/v1/anti/getLiveSecret"
	templateFile         = "interface.m3u"
	authKeyCacheDuration = 300 * time.Minute
)

// ========== 全局状态 ==========
var (
	mu          sync.RWMutex
	accessToken string

	authKeyCache   = make(map[string]*authCacheItem)
	authKeyCacheMu sync.RWMutex
)

type authCacheItem struct {
	authKey  string
	expireAt time.Time
}

// ========== Secret 响应结构 ==========
type SecretResp struct {
	Rs    int    `json:"rs"`
	Error string `json:"error"`
	Data  struct {
		Secret string `json:"secret"`
	} `json:"data"`
}

// ========== 获取 auth_key（带缓存 + 详细日志） ==========
func getSecret(channelID string) (string, error) {
	authKeyCacheMu.RLock()
	item, found := authKeyCache[channelID]
	authKeyCacheMu.RUnlock()

	if found && time.Now().Before(item.expireAt) {
		log.Printf("[getSecret] 频道 %s 命中缓存，auth_key 有效，剩余时间: %v", channelID, time.Until(item.expireAt).Round(time.Second))
		return item.authKey, nil
	}

	if found {
		log.Printf("[getSecret] 频道 %s 缓存已过期，将重新请求", channelID)
	} else {
		log.Printf("[getSecret] 频道 %s 无缓存，首次请求", channelID)
	}

	mu.RLock()
	token := accessToken
	mu.RUnlock()
	if token == "" {
		log.Printf("[getSecret] 频道 %s 请求失败: access_token 未设置", channelID)
		return "", fmt.Errorf("access_token 未设置")
	}

	streamName := "/live/" + channelID
	if !strings.HasSuffix(streamName, ".m3u8") {
		streamName += ".m3u8"
	}
	txTime := fmt.Sprintf("%d", time.Now().Unix())
	reqURL := fmt.Sprintf("%s?streamName=%s&txTime=%s", getSecretURL, streamName, txTime)

	log.Printf("[getSecret] 频道 %s 发起请求: %s", channelID, reqURL)

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		log.Printf("[getSecret] 频道 %s 创建请求失败: %v", channelID, err)
		return "", err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0")
	req.Header.Set("Referer", "https://www.sctv.com/")
	req.Header.Set("Origin", "https://www.sctv.com")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[getSecret] 频道 %s 请求失败: %v", channelID, err)
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[getSecret] 频道 %s 读取响应失败: %v", channelID, err)
		return "", err
	}

	if resp.StatusCode != 200 {
		log.Printf("[getSecret] 频道 %s 请求返回非200状态码: %d, body: %s", channelID, resp.StatusCode, string(body))
		return "", fmt.Errorf("getLiveSecret 返回 %d: %s", resp.StatusCode, string(body))
	}

	var secretResp SecretResp
	if err := json.Unmarshal(body, &secretResp); err != nil {
		log.Printf("[getSecret] 频道 %s 解析JSON失败: %v, body: %s", channelID, err, string(body))
		return "", fmt.Errorf("解析 secret 失败: %v", err)
	}

	if secretResp.Rs != 200 {
		log.Printf("[getSecret] 频道 %s 业务错误: rs=%d, error=%s", channelID, secretResp.Rs, secretResp.Error)
		return "", fmt.Errorf("getLiveSecret 错误: %s", secretResp.Error)
	}

	authKey := secretResp.Data.Secret
	log.Printf("[getSecret] 频道 %s 获取 auth_key 成功: %s", channelID, authKey)

	authKeyCacheMu.Lock()
	authKeyCache[channelID] = &authCacheItem{
		authKey:  authKey,
		expireAt: time.Now().Add(authKeyCacheDuration),
	}
	authKeyCacheMu.Unlock()
	log.Printf("[getSecret] 频道 %s 缓存已更新，有效期至 %s", channelID, time.Now().Add(authKeyCacheDuration).Format("15:04:05"))

	return authKey, nil
}

// ========== 通用 URL 重写辅助函数 ==========
func rewriteURL(rawURI, currentURL, baseURL string) string {
	rawURI = strings.Trim(rawURI, `"`)
	var fullURL string
	if strings.HasPrefix(rawURI, "http://") || strings.HasPrefix(rawURI, "https://") {
		fullURL = rawURI
	} else {
		lastSlash := strings.LastIndex(currentURL, "/")
		if lastSlash == -1 {
			fullURL = currentURL + "/" + rawURI
		} else {
			fullURL = currentURL[:lastSlash+1] + rawURI
		}
	}
	encoded := url.QueryEscape(fullURL)
	return fmt.Sprintf("%s/ts?url=%s", baseURL, encoded)
}

// 替换标签内指定属性（如 URI="xxx"）的辅助方法
func processTagURI(line, attrKey, targetURL, baseURL string) string {
	idx := strings.Index(line, attrKey)
	if idx == -1 {
		return line
	}
	startQuote := idx + len(attrKey)
	if startQuote >= len(line) || line[startQuote] != '"' {
		return line
	}
	endQuote := strings.Index(line[startQuote+1:], `"`)
	if endQuote == -1 {
		return line
	}
	endQuote += startQuote + 1

	rawURI := line[startQuote+1 : endQuote]
	proxiedURI := rewriteURL(rawURI, targetURL, baseURL)

	return line[:startQuote+1] + proxiedURI + line[endQuote:]
}

// ========== 核心：通用 M3U8 文本解析与代理重写 ==========
func processM3U8Content(targetURL string, baseURL string) (string, error) {
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0")
	req.Header.Set("Referer", "https://www.sctv.com/")
	req.Header.Set("Origin", "https://www.sctv.com")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("获取 m3u8 失败状态码: %d, body: %s", resp.StatusCode, string(body))
	}

	content := string(body)
	lines := strings.Split(content, "\n")
	var result []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 1. 处理 EXT-X-MEDIA (音频轨定义)
		if strings.HasPrefix(trimmed, "#EXT-X-MEDIA:") {
			line = processTagURI(line, "URI=", targetURL, baseURL)
			result = append(result, line)
			continue
		}

		// 2. 处理 EXT-X-MAP (fMP4 初始化片段)
		if strings.HasPrefix(trimmed, "#EXT-X-MAP:") {
			line = processTagURI(line, "URI=", targetURL, baseURL)
			result = append(result, line)
			continue
		}

		// 3. 忽略其他注释/标签
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			result = append(result, line)
			continue
		}

		// 4. 处理数据切片或子 m3u8 路径 (.ts, .m4s, .m3u8 等)
		proxyLine := rewriteURL(trimmed, targetURL, baseURL)
		result = append(result, proxyLine)
	}

	return strings.Join(result, "\n"), nil
}

// ========== 代理主频道的 m3u8 ==========
func proxyM3U8(channelID string, baseURL string) (string, error) {
	log.Printf("[proxyM3U8] 开始处理频道 %s (实时获取)", channelID)

	authKey, err := getSecret(channelID)
	if err != nil {
		log.Printf("[proxyM3U8] 频道 %s 获取 auth_key 失败: %v", channelID, err)
		return "", err
	}

	streamName := "/live/" + channelID
	if !strings.HasSuffix(streamName, ".m3u8") {
		streamName += ".m3u8"
	}
	var realURL string
	if strings.HasPrefix(channelID, "4ksctv1") {
		// 4K 超高清频道使用特殊域名
		alyunols := ""
		if channelID != "4ksctv1" {
			alyunols = "aliyunols=on&"
		}
		realURL = "https://hmmslivef.scgczm.com" + streamName + "?" + alyunols + authKey
	} else {
		// 普通频道
		realURL = "https://tvshowf.scgczm.com" + streamName + "?" + authKey
	}
	log.Printf("[proxyM3U8] 频道 %s 请求真实 m3u8: %s", channelID, realURL)

	proxied, err := processM3U8Content(realURL, baseURL)
	if err != nil {
		log.Printf("[proxyM3U8] 频道 %s 处理 m3u8 失败: %v", channelID, err)
		return "", err
	}

	log.Printf("[proxyM3U8] 频道 %s 代理 m3u8 完成，长度 %d 字节", channelID, len(proxied))
	return proxied, nil
}

// ========== HTTP 处理器 ==========

func playlistHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("[playlistHandler] 收到播放列表请求")
	file, err := os.Open(templateFile)
	if err != nil {
		log.Printf("[playlistHandler] 打开模板失败: %v", err)
		http.Error(w, "读取模板失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[playlistHandler] 扫描模板失败: %v", err)
		http.Error(w, "解析模板失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	var result []string
	for _, line := range lines {
		if strings.HasPrefix(line, "${replace}") {
			parts := strings.SplitN(line, "/", 2)
			if len(parts) < 2 {
				result = append(result, line)
				continue
			}
			channelID := strings.TrimSpace(parts[1])
			proxyEntry := fmt.Sprintf("%s/stream/%s", baseURL, channelID)
			result = append(result, proxyEntry)
		} else {
			result = append(result, line)
		}
	}
	playlist := strings.Join(result, "\n")
	log.Printf("[playlistHandler] 播放列表生成成功，共 %d 行", len(result))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	if path == "" {
		log.Println("[streamHandler] 请求缺少频道ID")
		http.Error(w, "缺少频道ID", http.StatusBadRequest)
		return
	}
	channelID := strings.Split(path, "/")[0]
	log.Printf("[streamHandler] 收到频道 %s 的流请求", channelID)

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	m3u8Content, err := proxyM3U8(channelID, baseURL)
	if err != nil {
		log.Printf("[streamHandler] 频道 %s 处理失败: %v", channelID, err)
		http.Error(w, "获取流失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(m3u8Content))
	log.Printf("[streamHandler] 频道 %s 流响应完成", channelID)
}

func tsProxyHandler(w http.ResponseWriter, r *http.Request) {
	encodedURL := r.URL.Query().Get("url")
	if encodedURL == "" {
		log.Println("[tsProxyHandler] 缺少 url 参数")
		http.Error(w, "缺少 url 参数", http.StatusBadRequest)
		return
	}
	realURL, err := url.QueryUnescape(encodedURL)
	if err != nil {
		log.Printf("[tsProxyHandler] url解码失败: %v", err)
		http.Error(w, "url 解码失败", http.StatusBadRequest)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	// 如果请求的是子 m3u8 文件（音频/视频流主文件），执行解析重写逻辑
	if strings.Contains(realURL, ".m3u8") {
		log.Printf("[tsProxyHandler] 代理子 m3u8: %s", realURL)
		m3u8Content, err := processM3U8Content(realURL, baseURL)
		if err != nil {
			log.Printf("[tsProxyHandler] 处理子 m3u8 失败 (%s): %v", realURL, err)
			http.Error(w, "代理 m3u8 失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(m3u8Content))
		return
	}

	// 如果是 .ts / .m4s 媒体切片，执行透传代理
	log.Printf("[tsProxyHandler] 代理媒体片段: %s", realURL)

	req, err := http.NewRequest("GET", realURL, nil)
	if err != nil {
		log.Printf("[tsProxyHandler] 创建请求失败: %v", err)
		http.Error(w, "创建请求失败", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0")
	req.Header.Set("Referer", "https://www.sctv.com/")
	req.Header.Set("Origin", "https://www.sctv.com")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[tsProxyHandler] 代理请求失败: %v", err)
		http.Error(w, "代理请求失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	log.Printf("[tsProxyHandler] 媒体代理返回状态码: %d", resp.StatusCode)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ========== 管理页面 ==========
func adminHandler(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Token 管理</title>
    <style>
        body { font-family: Arial; padding: 20px; }
        .box { border: 1px solid #ccc; padding: 15px; margin-bottom: 15px; border-radius: 5px; }
        textarea { width: 100%; height: 120px; font-family: monospace; }
        button { margin-right: 10px; padding: 8px 16px; cursor: pointer; }
        .status { color: green; }
        .error { color: red; }
    </style>
</head>
<body>
    <h1>Token 管理</h1>
    <div class="box">
        <p><strong>当前 Access Token：</strong> <span id="tokenStatus">未设置</span></p>
        <p><strong>状态：</strong> <span id="statusText">未知</span></p>
    </div>
    <div class="box">
        <h3>设置 Access Token</h3>
        <textarea id="tokenInput" placeholder="粘贴 access_token 值..."></textarea>
        <br>
        <button onclick="setToken()">保存</button>
        <button onclick="clearToken()">清空</button>
    </div>
    <div class="box">
        <h3>播放列表地址</h3>
        <p><code>http://` + r.Host + `/interface.m3u</code></p>
    </div>
    <script>
        async function getStatus() {
            const res = await fetch('/admin/status');
            const data = await res.json();
            document.getElementById('tokenStatus').textContent = data.has_token ? '已设置' : '未设置';
            document.getElementById('statusText').textContent = data.has_token ? '有效' : '请先设置 Access Token';
        }

        async function setToken() {
            const token = document.getElementById('tokenInput').value.trim();
            if (!token) { alert('请粘贴 access_token'); return; }
            const res = await fetch('/admin/token', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({access_token: token})
            });
            if (res.ok) {
                alert('保存成功');
                getStatus();
                document.getElementById('tokenInput').value = '';
            } else {
                const err = await res.text();
                alert('保存失败: ' + err);
            }
        }

        async function clearToken() {
            if (!confirm('确定清空 Token 吗？')) return;
            const res = await fetch('/admin/token', { method: 'DELETE' });
            if (res.ok) {
                alert('已清空');
                getStatus();
            } else {
                alert('清空失败');
            }
        }

        getStatus();
    </script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// ========== 状态 API ==========
func statusHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	has := accessToken != ""
	mu.RUnlock()
	json.NewEncoder(w).Encode(map[string]bool{"has_token": has})
}

// ========== 设置 Token ==========
func setTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.AccessToken == "" {
		http.Error(w, "access_token 不能为空", http.StatusBadRequest)
		return
	}
	mu.Lock()
	accessToken = req.AccessToken
	mu.Unlock()
	log.Println("[setTokenHandler] access_token 已更新")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ========== 清空 Token ==========
func clearTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	accessToken = ""
	mu.Unlock()
	log.Println("[clearTokenHandler] access_token 已清空")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ========== 健康检查 ==========
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

// ========== main ==========
func main() {
	if _, err := os.Stat(templateFile); os.IsNotExist(err) {
		log.Fatalf("模板文件 %s 不存在", templateFile)
	}

	http.HandleFunc("/interface.m3u", playlistHandler)
	http.HandleFunc("/stream/", streamHandler)
	http.HandleFunc("/ts", tsProxyHandler)
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/admin", adminHandler)
	http.HandleFunc("/admin/status", statusHandler)
	http.HandleFunc("/admin/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			setTokenHandler(w, r)
		} else if r.Method == http.MethodDelete {
			clearTokenHandler(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "6622"
	}
	log.Printf("服务启动，监听端口 %s", port)
	log.Printf("管理页面: http://localhost:%s/admin", port)
	log.Printf("播放列表: http://localhost:%s/interface.m3u", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
