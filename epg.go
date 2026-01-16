package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/enetx/g"
	"github.com/enetx/surf"
)

// Config 应用配置
type Config struct {
	ChannelMapURL string `json:"channel_map_url"`
	EPGDataURL    string `json:"epg_data_url"`
	Port          string `json:"port"`
	AliasFile     string `json:"alias_file"`
}

// ChannelMap 频道映射结构
type ChannelMap map[string]string

// EPGResponse EPG接口响应结构
type EPGResponse struct {
	Channel   ChannelInfo `json:"channel"`
	Schedules []Schedule  `json:"schedules"`
}

// ChannelInfo 频道信息
type ChannelInfo struct {
	Code  string `json:"code"`
	Title string `json:"title"`
}

// Schedule 节目单信息
type Schedule struct {
	Title         string `json:"title"`
	StartTime     string `json:"starttime"`
	EndTime       string `json:"endtime"`
	ShowStartTime string `json:"showStarttime"`
}

// APIResponse API返回结构
type APIResponse struct {
	Date        string    `json:"date"`
	ChannelName string    `json:"channel_name"`
	URL         string    `json:"url"`
	EPGData     []EPGData `json:"epg_data"`
}

// EPGData 节目数据
type EPGData struct {
	Start string `json:"start"`
	Desc  string `json:"desc"`
	End   string `json:"end"`
	Title string `json:"title"`
}

// MetricsResponse 指标响应结构
type MetricsResponse struct {
	CacheEntries     int    `json:"cache_entries"`
	ChannelMappings  int    `json:"channel_mappings"`
	AliasMappings    int    `json:"alias_mappings"`
	ActiveLocks      int    `json:"active_locks"`
	ChannelBlackouts int    `json:"channel_blackouts"`
	CacheDuration    string `json:"cache_duration"`
	MaxCacheSize     int    `json:"max_cache_size"`
	MaxChannelLocks  int    `json:"max_channel_locks"`

	// HTTP客户端指标
	HTTPClientInitialized  bool   `json:"http_client_initialized"`
	MaxIdleConns           int    `json:"http_max_idle_conns"`
	MaxIdleConnsPerHost    int    `json:"http_max_idle_conns_per_host"`
	MaxConnsPerHost        int    `json:"http_max_conns_per_host"`
	IdleConnTimeout        string `json:"http_idle_conn_timeout"`
	DisableKeepAlives      bool   `json:"http_disable_keep_alives"`
	TLSHandshakeTimeout    string `json:"http_tls_handshake_timeout"`
	MaxResponseHeaderBytes int64  `json:"http_max_response_header_bytes"`
}

// AliasMap 别名映射结构
type AliasMap map[string]string

// CacheItem 缓存项结构
type CacheItem struct {
	Data      []EPGData `json:"data"`
	Timestamp time.Time `json:"timestamp"`
}

// Cache 缓存结构
type Cache map[string]CacheItem

// ChannelBlackouts 频道黑屏记录，直接使用频道名到时间的映射
type ChannelBlackouts map[string]time.Time

// AppContext 应用上下文，封装所有共享资源
type AppContext struct {
	Cache            Cache
	CacheMutex       sync.RWMutex
	ChannelMap       ChannelMap
	ChannelMutexes   map[string]*sync.RWMutex
	AliasMap         AliasMap
	ChannelBlackouts ChannelBlackouts
	Config           Config
	Ctx              context.Context
	Cancel           context.CancelFunc
	MutexMapMutex    sync.Mutex
}

const (
	CacheDuration   = 216 * time.Hour // 9天缓存
	UserAgent       = "okhttp/3.10.0" // 自定义User-Agent
	MaxCacheSize    = 10000           // 最大缓存条目数
	MaxChannelLocks = 100             // 最大频道锁数量
)

var app *AppContext
var globalHTTPClient *http.Client
var globalSurfBrowser *surf.Client

func main() {
	// 创建应用上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app = &AppContext{
		Cache:            make(Cache),
		ChannelMutexes:   make(map[string]*sync.RWMutex),
		AliasMap:         make(AliasMap),
		ChannelBlackouts: make(ChannelBlackouts),
		Ctx:              ctx,
		Cancel:           cancel,
	}

	// 解析命令行参数
	app.parseFlags()

	// 加载配置
	if err := app.loadConfig(); err != nil {
		log.Printf("配置加载警告: %v", err)
	}

	// 打印启动参数
	app.printStartupInfo()

	// 获取频道映射
	if err := app.loadChannelMap(); err != nil {
		log.Println("获取频道映射失败:", err)
	}

	// 加载别名映射
	if err := app.loadAliasMap(); err != nil {
		log.Printf("加载别名映射失败: %v, 继续使用空映射", err)
	}

	// 启动后台任务
	go app.cleanupRoutine()

	// 启动HTTP服务
	app.startHTTPServer()
}

// parseFlags 解析命令行参数
func (a *AppContext) parseFlags() {
	channelMapURL := flag.String("url", "http://120.87.12.38:8083/epg/api/page/biz_59417088.json", "频道映射API的URL")
	epgDataURL := flag.String("epgurl", "https://0777.112114.xyz/", "EPG数据API的URL")
	port := flag.String("port", "8080", "服务器端口")
	aliasFile := flag.String("alias", "alias.json", "别名映射文件路径")
	configFile := flag.String("config", "", "配置文件路径")

	flag.Parse()

	// 设置默认配置
	if a.Config.ChannelMapURL == "" {
		a.Config.ChannelMapURL = *channelMapURL
	}
	if a.Config.EPGDataURL == "" {
		a.Config.EPGDataURL = *epgDataURL
	}
	if a.Config.Port == "" {
		a.Config.Port = *port
	}
	if a.Config.AliasFile == "" {
		a.Config.AliasFile = *aliasFile
	}

	// 从配置文件加载（如果提供）
	if *configFile != "" {
		if err := a.loadConfigFromFile(*configFile); err != nil {
			log.Printf("加载配置文件失败: %v, 使用命令行参数", err)
		} else {
			log.Println("成功从配置文件加载配置")
		}
	}
}

// loadConfigFromFile 从文件加载配置
func (a *AppContext) loadConfigFromFile(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 合并配置
	if config.ChannelMapURL != "" {
		a.Config.ChannelMapURL = config.ChannelMapURL
	}
	if config.EPGDataURL != "" {
		a.Config.EPGDataURL = config.EPGDataURL
	}
	if config.Port != "" {
		a.Config.Port = config.Port
	}
	if config.AliasFile != "" {
		a.Config.AliasFile = config.AliasFile
	}

	return nil
}

// loadConfig 验证并加载配置
func (a *AppContext) loadConfig() error {
	// 验证端口
	if a.Config.Port == "" {
		return fmt.Errorf("端口不能为空")
	}

	// 验证URL格式（简单检查）
	if a.Config.ChannelMapURL == "" {
		return fmt.Errorf("频道映射URL不能为空")
	}

	return nil
}

// printStartupInfo 打印启动信息
func (a *AppContext) printStartupInfo() {
	fmt.Println("=== 启动参数 ===")
	fmt.Printf("频道映射API URL: %s\n", a.Config.ChannelMapURL)
	fmt.Printf("EPG数据API URL: %s\n", a.Config.EPGDataURL)
	fmt.Printf("服务器端口: %s\n", a.Config.Port)
	fmt.Printf("别名映射文件: %s\n", a.Config.AliasFile)
	fmt.Println("================")
}

// loadChannelMap 获取频道映射
func (a *AppContext) loadChannelMap() error {
	client := createHTTPClient()
	req, err := createHTTPRequest("GET", a.Config.ChannelMapURL)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求频道映射失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("频道映射API返回非200状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应体失败: %w", err)
	}

	var result struct {
		AreaDatas []struct {
			Items []struct {
				ItemTitle string `json:"itemTitle"`
				DataLink  string `json:"dataLink"`
			} `json:"items"`
		} `json:"areaDatas"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析频道映射失败: %w", err)
	}

	channelMap := make(ChannelMap)
	for _, area := range result.AreaDatas {
		for _, item := range area.Items {
			channelMap[item.ItemTitle] = item.DataLink
		}
	}

	if len(channelMap) == 0 {
		return fmt.Errorf("频道映射为空")
	}

	a.ChannelMap = channelMap
	log.Printf("成功加载 %d 个频道映射", len(channelMap))
	return nil
}

// loadAliasMap 加载别名映射
func (a *AppContext) loadAliasMap() error {
	data, err := os.ReadFile(a.Config.AliasFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("别名文件不存在: %s, 使用空映射", a.Config.AliasFile)
			a.AliasMap = make(AliasMap)
			return nil
		}
		return fmt.Errorf("读取别名文件失败: %w", err)
	}

	var aliasMap AliasMap
	if err := json.Unmarshal(data, &aliasMap); err != nil {
		return fmt.Errorf("解析别名映射失败: %w", err)
	}

	a.AliasMap = aliasMap
	log.Printf("成功加载 %d 个别名映射", len(aliasMap))
	return nil
}

// initHTTPClient 初始化全局HTTP客户端
func initHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// 连接池配置
			MaxIdleConns:        200,               // 最大空闲连接数
			MaxIdleConnsPerHost: 20,                // 每个主机最大空闲连接数
			IdleConnTimeout:     120 * time.Second, // 空闲连接超时时间
			MaxConnsPerHost:     50,                // 每个主机最大连接数
			DisableKeepAlives:   false,             // 启用Keep-Alive

			// DNS解析配置
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 5 * time.Second,
			}).DialContext,

			// TLS配置
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,

			// 连接复用优化
			MaxResponseHeaderBytes: 1 << 20, // 1MB 最大响应头
		},
	}
}

// createHTTPClient 创建或返回全局HTTP客户端
func createHTTPClient() *http.Client {
	if globalHTTPClient == nil {
		globalHTTPClient = initHTTPClient()
		log.Println("全局HTTP客户端已初始化，连接池配置已应用")
	}
	return globalHTTPClient
}

// createHTTPRequest 创建带User-Agent的HTTP请求
func createHTTPRequest(method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")

	return req, nil
}

// handleEPGRequest 处理EPG请求
func handleEPGRequest(w http.ResponseWriter, r *http.Request) {
	// 记录请求开始时间
	startTime := time.Now()

	ch := r.URL.Query().Get("ch")
	date := r.URL.Query().Get("date")

	if ch == "" || date == "" {
		http.Error(w, "缺少参数ch或date", http.StatusBadRequest)
		return
	}

	// 验证日期格式
	if !isValidDate(date) {
		http.Error(w, "日期格式不正确，应为 YYYY-MM-DD 格式", http.StatusBadRequest)
		return
	}

	// 将频道名转换为全大写并删除"标清"字符串
	actualCh := strings.ReplaceAll(ch, "标清", "")
	actualCh = strings.ToUpper(actualCh)

	// 处理别名
	if alias, exists := app.AliasMap[actualCh]; exists {
		actualCh = alias
	}

	// 获取频道链接
	url, exists := app.ChannelMap[actualCh]

	// 获取EPG数据
	epgData, err := app.getEPGData(actualCh, url, date)
	if err != nil {
		if !exists {
			log.Printf("未找到频道 %s", actualCh)
		}
		log.Printf("获取EPG数据失败:", err.Error())
		http.Error(w, "获取EPG数据失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 构造返回结果
	response := APIResponse{
		Date:        date,
		ChannelName: actualCh,
		URL:         url,
		EPGData:     epgData,
	}

	// 设置响应头
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Response-Time", time.Since(startTime).String())
	json.NewEncoder(w).Encode(response)
}

// isValidDate 验证日期格式是否为 YYYY-MM-DD
func isValidDate(date string) bool {
	_, err := time.Parse("2006-01-02", date)
	return err == nil
}

// getEPGData 获取EPG数据
func (a *AppContext) getEPGData(ch, iptvurl, date string) ([]EPGData, error) {
	// 检查目标日期的缓存
	targetDate := strings.ReplaceAll(date, "-", "")
	cacheKey := fmt.Sprintf("%s_%s", ch, date)

	// 检查频道是否被标记为禁用EPG请求
	targetDateObj, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, fmt.Errorf("无效的日期格式: %s", date)
	}
	// 检查频道是否被标记为禁用EPG请求（从特定日期开始）
	if blackout, exists := a.ChannelBlackouts[ch]; exists {
		// 如果目标日期大于等于禁用开始日期，则返回{"title":"精彩节目","start":"00:00","end":"23:59"}]
		if targetDateObj.After(blackout) || targetDateObj.Equal(blackout) || blackout.After(time.Date(2099, 1, 1, 1, 1, 1, 1, time.Local)) {
			log.Printf("频道 %s 在日期 %s 已被标记为禁用EPG请求，跳过API调用", ch, date)
			return []EPGData{{Title: "精彩节目", Start: "00:00", End: "23:59"}}, nil
		}
	}

	// 获取频道专用锁
	a.MutexMapMutex.Lock()
	channelMutex, exists := a.ChannelMutexes[ch]
	if !exists {
		channelMutex = &sync.RWMutex{}
		a.ChannelMutexes[ch] = channelMutex
		// 限制锁的数量
		if len(a.ChannelMutexes) > MaxChannelLocks {
			// 清理一些旧的锁（简单策略：清理前10个）
			count := 0
			for oldCh := range a.ChannelMutexes {
				if oldCh != ch {
					delete(a.ChannelMutexes, oldCh)
					count++
					if count >= 10 {
						break
					}
				}
			}
		}
	}

	a.MutexMapMutex.Unlock()

	// 获取写锁进行缓存更新
	channelMutex.Lock()
	defer channelMutex.Unlock()

	// 双重检查，防止并发重复请求
	if cachedItem, exists := a.Cache[cacheKey]; exists {
		if time.Since(cachedItem.Timestamp) < CacheDuration {
			return cachedItem.Data, nil
		}
		delete(a.Cache, cacheKey)
	}

	client := createHTTPClient()

	// 检查URL是否为空
	currentTime := time.Now()
	if iptvurl != "" {
		// 计算获取数据的日期范围
		endtime := currentTime.AddDate(0, 0, 2).Format("20060102")
		begintime := currentTime.AddDate(0, 0, -9)
		start, err := time.Parse("2006-01-02", date)
		if err == nil {
			if currentTime.Sub(start).Hours() > 0 {
				begintime = start.AddDate(0, 0, -7)
			}
		}

		// 检查最近7天内哪些日期的数据不存在或已过期
		for i := 0; i <= 7; i++ {
			begintime = begintime.AddDate(0, 0, 1)
			checkCacheKey := fmt.Sprintf("%s_%s", ch, begintime.Format("2006-01-02"))
			item, exists := a.Cache[checkCacheKey]
			if exists && time.Since(item.Timestamp) < CacheDuration {
				continue
			} else {
				break
			}
		}

		// 将日期参数添加到URL中
		if strings.Contains(iptvurl, "?") {
			iptvurl += fmt.Sprintf("&begintime=%s&endtime=%s", begintime.Format("20060102"), endtime)
		} else {
			iptvurl += fmt.Sprintf("?begintime=%s&endtime=%s", begintime.Format("20060102"), endtime)
		}

		req, err := createHTTPRequest("GET", iptvurl)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("EPG API返回非200状态码: %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		var epgResp EPGResponse
		if err := json.Unmarshal(body, &epgResp); err != nil {
			return nil, err
		}

		// 按日期分组缓存数据
		dateMap := make(map[string][]EPGData)

		for _, schedule := range epgResp.Schedules {
			if len(schedule.StartTime) >= 8 {
				scheduleDate := schedule.StartTime[:8]

				epgItem := EPGData{
					Start: formatTime(schedule.StartTime),
					End:   formatTime(schedule.EndTime),
					Desc:  "",
					Title: schedule.Title,
				}

				dateMap[scheduleDate] = append(dateMap[scheduleDate], epgItem)
			}
		}

		// 缓存所有获取到的日期数据
		for scheduleDate, schedules := range dateMap {
			if len(scheduleDate) == 8 {
				formattedDate := fmt.Sprintf("%s-%s-%s", scheduleDate[:4], scheduleDate[4:6], scheduleDate[6:8])
				dateKey := fmt.Sprintf("%s_%s", ch, formattedDate)

				// 检查缓存大小限制
				if len(a.Cache) >= MaxCacheSize {
					// 简单的清理策略：清理过期的缓存
					now := time.Now()
					for key, item := range a.Cache {
						if now.Sub(item.Timestamp) >= CacheDuration {
							delete(a.Cache, key)
						}
					}
					// 如果还是太大，清理最旧的
					if len(a.Cache) >= MaxCacheSize {
						oldestKey := ""
						oldestTime := time.Now()
						for key, item := range a.Cache {
							if item.Timestamp.Before(oldestTime) {
								oldestTime = item.Timestamp
								oldestKey = key
							}
						}
						if oldestKey != "" {
							delete(a.Cache, oldestKey)
						}
					}
				}

				item, exists := a.Cache[dateKey]
				if !exists || time.Since(item.Timestamp) >= CacheDuration {
					a.Cache[dateKey] = CacheItem{
						Data:      schedules,
						Timestamp: time.Now(),
					}
				}
			}
		}

		// 检查是否有目标日期的数据
		if targetSchedules, exists := dateMap[targetDate]; exists {
			return targetSchedules, nil
		}
	}

	// 如果没有找到目标日期的数据，直接向EPG数据API发起请求
	var apiURL string
	if a.Config.EPGDataURL != "" {
		baseURL, err := url.Parse(a.Config.EPGDataURL)
		if err != nil {
			return []EPGData{}, fmt.Errorf("EPG数据API URL格式错误: %w", err)
		}

		queryParams := url.Values{}
		queryParams.Set("ch", ch)
		queryParams.Set("date", date)
		baseURL.RawQuery = queryParams.Encode()

		apiURL = baseURL.String()
	} else {
		return []EPGData{}, fmt.Errorf("未找到频道 %s 在日期 %s 的数据，且没有指定EPG数据API URL", ch, date)
	}

	log.Printf("未找到频道 %s 在日期 %s 的数据，正在向EPG数据API请求: %s", ch, date, apiURL)

	// 使用 surf 库发起请求
	if globalSurfBrowser == nil {
		globalSurfBrowser = surf.NewClient().Builder().Impersonate().Chrome().Build()
	}

	resp := globalSurfBrowser.Get(g.String(apiURL)).Do()
	if resp.Err() != nil {
		return []EPGData{}, resp.Err()
	}
	if resp.Ok().StatusCode != http.StatusOK {
		resp.Ok().Debug().Request().Print()
		resp.Ok().Debug().Response().Print()
		return []EPGData{}, fmt.Errorf("EPG数据API返回非200状态码 %d", resp.Ok().StatusCode)
	}

	var apiResponse APIResponse
	if err := resp.Ok().Body.JSON(&apiResponse); err != nil {
		return []EPGData{}, err
	}
	if apiResponse.EPGData[0].Title == "精彩节目" {
		// 当EPG数据API返回的数据不符合预期格式时，标记该ch在当天不再请求EPG数据API

		// 设置为当天的日期，表示跳过请求
		if targetDateObj.Before(currentTime) {
			targetDateObj = time.Date(2099, 12, 12, 0, 0, 0, 0, time.Local)
		}
		// 获取锁来更新ChannelBlackouts
		a.MutexMapMutex.Lock()
		a.ChannelBlackouts[ch] = targetDateObj
		a.MutexMapMutex.Unlock()

		log.Printf("频道 %s 的EPG数据API返回的数据不符合预期格式，仅在 %s 禁用EPG请求", ch, targetDateObj.Format("2006-01-02"))
		// 返回从API获取的实际数据
		return apiResponse.EPGData, nil
	}

	// 缓存API返回的数据
	dateKey := fmt.Sprintf("%s_%s", ch, date)

	item, exists := a.Cache[dateKey]
	if !exists || time.Since(item.Timestamp) >= CacheDuration {
		a.Cache[dateKey] = CacheItem{
			Data:      apiResponse.EPGData,
			Timestamp: time.Now(),
		}
	}

	log.Printf("从EPG数据API获取到频道 %s 在日期 %s 的 %d 条数据", ch, date, len(apiResponse.EPGData))
	return apiResponse.EPGData, nil
}

// formatTime 格式化时间
func formatTime(timeStr string) string {
	if len(timeStr) >= 12 {
		hour := timeStr[8:10]
		minute := timeStr[10:12]
		return fmt.Sprintf("%s:%s", hour, minute)
	}
	return ""
}

// cleanupRoutine 后台清理任务
func (a *AppContext) cleanupRoutine() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-a.Ctx.Done():
			log.Println("正在清理资源...")
			a.cleanupResources()
			log.Println("资源清理完成")
			return

		case <-ticker.C:
			a.cleanupExpiredCache()
			a.cleanupChannelLocks()
			a.cleanupChannelBlackouts()
		}
	}
}

// cleanupExpiredCache 清理过期缓存
func (a *AppContext) cleanupExpiredCache() {
	now := time.Now()
	a.CacheMutex.Lock()
	defer a.CacheMutex.Unlock()

	deleted := 0
	for key, item := range a.Cache {
		if now.Sub(item.Timestamp) >= CacheDuration {
			delete(a.Cache, key)
			deleted++
		}
	}

	if deleted > 0 {
		log.Printf("清理了 %d 个过期缓存条目", deleted)
	}
}

// cleanupChannelLocks 清理频道锁
func (a *AppContext) cleanupChannelLocks() {
	a.MutexMapMutex.Lock()
	defer a.MutexMapMutex.Unlock()

	if len(a.ChannelMutexes) > MaxChannelLocks {
		// 清理一半的锁
		toDelete := len(a.ChannelMutexes) / 2
		count := 0
		for ch := range a.ChannelMutexes {
			delete(a.ChannelMutexes, ch)
			count++
			if count >= toDelete {
				break
			}
		}
		log.Printf("清理了 %d 个频道锁，剩余 %d 个", count, len(a.ChannelMutexes))
	}
}

// cleanupChannelBlackouts 清理过期的频道黑名单标记
func (a *AppContext) cleanupChannelBlackouts() {
	now := time.Now()
	// 构造当天0点时间
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	a.MutexMapMutex.Lock()
	defer a.MutexMapMutex.Unlock()

	// 清理昨天及更早的标记
	for ch, blackoutDate := range a.ChannelBlackouts {
		if blackoutDate.Before(midnight) {
			delete(a.ChannelBlackouts, ch)
		}
	}
}

// cleanupResources 清理所有资源
func (a *AppContext) cleanupResources() {
	a.CacheMutex.Lock()
	a.Cache = make(Cache)
	a.CacheMutex.Unlock()

	a.MutexMapMutex.Lock()
	a.ChannelMutexes = make(map[string]*sync.RWMutex)
	a.ChannelBlackouts = make(ChannelBlackouts)
	a.MutexMapMutex.Unlock()
}

// startHTTPServer 启动HTTP服务器
func (a *AppContext) startHTTPServer() {
	// 注册路由
	http.HandleFunc("/", handleEPGRequest)
	http.HandleFunc("/health", a.healthHandler)
	http.HandleFunc("/metrics", a.metricsHandler)

	server := &http.Server{
		Addr:    ":" + a.Config.Port,
		Handler: nil,
	}

	// 启动服务器
	go func() {
		log.Printf("服务器启动在端口 %s...", a.Config.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP服务器启动失败: %v", err)
		}
	}()

	// 等待关闭信号
	<-a.Ctx.Done()
	log.Println("正在关闭服务器...")

	// 优雅关闭
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("服务器关闭失败: %v", err)
	}

	log.Println("服务器已关闭")
}

// healthHandler 健康检查处理器
func (a *AppContext) healthHandler(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"status":            "healthy",
		"timestamp":         time.Now().Format(time.RFC3339),
		"cache_size":        len(a.Cache),
		"channel_map":       len(a.ChannelMap),
		"alias_map":         len(a.AliasMap),
		"locks":             len(a.ChannelMutexes),
		"channel_blackouts": len(a.ChannelBlackouts),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// metricsHandler 指标处理器
func (a *AppContext) metricsHandler(w http.ResponseWriter, r *http.Request) {
	response := MetricsResponse{
		CacheEntries:     len(a.Cache),
		ChannelMappings:  len(a.ChannelMap),
		AliasMappings:    len(a.AliasMap),
		ActiveLocks:      len(a.ChannelMutexes),
		ChannelBlackouts: len(a.ChannelBlackouts),
		CacheDuration:    CacheDuration.String(),
		MaxCacheSize:     MaxCacheSize,
		MaxChannelLocks:  MaxChannelLocks,
	}

	// 添加HTTP客户端指标
	if globalHTTPClient != nil && globalHTTPClient.Transport != nil {
		transport := globalHTTPClient.Transport.(*http.Transport)
		response.HTTPClientInitialized = true
		response.MaxIdleConns = transport.MaxIdleConns
		response.MaxIdleConnsPerHost = transport.MaxIdleConnsPerHost
		response.MaxConnsPerHost = transport.MaxConnsPerHost
		response.IdleConnTimeout = transport.IdleConnTimeout.String()
		response.DisableKeepAlives = transport.DisableKeepAlives
		response.TLSHandshakeTimeout = transport.TLSHandshakeTimeout.String()
		response.MaxResponseHeaderBytes = transport.MaxResponseHeaderBytes
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
