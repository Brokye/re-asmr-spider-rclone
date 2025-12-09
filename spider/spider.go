package spider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"path/filepath"

	"re-asmr-spider/config"
	"re-asmr-spider/i18n"
	"re-asmr-spider/utils"
)

var Conf *config.Config
const LocalTempDir = "/root/asmr_temp"
func moveFile(src, dst string) error {
	// 尝试直接重命名（如果是在同一个分区可能成功，但挂载点通常不行）
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	// 如果重命名失败（跨设备移动），则进行 复制+删除
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	// 确保目标目录存在
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		return err
	}

	// 复制成功后，关闭文件并删除源文件
	sourceFile.Close()
	destFile.Close()
	return os.Remove(src)
}

func init() {
	var err error
	Conf, err = config.GetConfig()
	if err != nil {
		fmt.Printf("Failed to get config: %v\n", err)
		os.Exit(1)
	}

	// 初始化i18n (使用配置中的语言设置)
	if err := i18n.Init(Conf.Language); err != nil {
		fmt.Printf("Failed to initialize i18n: %v\n", err)
		os.Exit(1)
	}

	// 初始化代理设置
	if Conf.Proxy != "" {
		if err := utils.SetProxy(Conf.Proxy); err != nil {
			fmt.Printf("Failed to set proxy: %v\n", err)
		}
	}
}

type FailedTask struct {
	URL       string
	DirPath   string
	FileName  string
	RetryCount int
}

type ASMRClient struct {
	Authorization string
	WorkerPool    *utils.WorkerPool
	ThreadCount   int
	FailedTasks   []FailedTask
	MaxRetry      int
	mu            sync.Mutex
}

type track struct {
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	Children         []track `json:"children,omitempty"`
	Hash             string  `json:"hash,omitempty"`
	WorkTitle        string  `json:"workTitle,omitempty"`
	MediaStreamURL   string  `json:"mediaStreamUrl,omitempty"`
	MediaDownloadURL string  `json:"mediaDownloadUrl,omitempty"`
}

func NewASMRClient(maxTask int, maxThread int, maxRetry int) *ASMRClient {
	return &ASMRClient{
		WorkerPool:  utils.NewWorkerPool(maxTask),
		ThreadCount: maxThread,
		FailedTasks: make([]FailedTask, 0),
		MaxRetry:    maxRetry,
	}
}

func (ac *ASMRClient) Login() error {
	utils.GlobalMonitor.UpdateActivity()

	payload, err := json.Marshal(map[string]string{
		"name":     Conf.Account,
		"password": Conf.Password,
	})
	if err != nil {
		utils.Error(i18n.T("parse_error", err))
		return err
	}
	client := utils.Client.Get().(*http.Client)
	req, _ := http.NewRequest("POST", "https://api.asmr.one/api/auth/me", bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://www.asmr.one/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.198 Safari/537.36")
	resp, err := client.Do(req)
	utils.Client.Put(client)
	if err != nil {
		utils.Error(i18n.T("network_error", err))
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.Error(i18n.T("request_failed", err))
		return err
	}
	res := make(map[string]string)
	err = json.Unmarshal(all, &res)
	ac.Authorization = "Bearer " + res["token"]
	utils.GlobalMonitor.UpdateActivity()
	utils.Success(i18n.T("login_success"))
	return nil
}

func (ac *ASMRClient) GetVoiceTracks(id string) ([]track, error) {
	utils.GlobalMonitor.UpdateActivity()

	client := utils.Client.Get().(*http.Client)
	req, _ := http.NewRequest("GET", "https://api.asmr.one/api/tracks/"+id, nil)
	req.Header.Set("Authorization", ac.Authorization)
	req.Header.Set("Referer", "https://www.asmr.one/")
	req.Header.Set("User-Agent", "PostmanRuntime/7.29.0")
	resp, err := client.Do(req)
	utils.Client.Put(client)
	if err != nil {
		utils.Error(i18n.T("request_failed", err))
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.Error(i18n.T("request_failed", err))
		return nil, err
	}
	res := make([]track, 0)
	err = json.Unmarshal(all, &res)
	utils.GlobalMonitor.UpdateActivity()
	return res, nil
}

// AddFailedTask 添加失败任务到重试队列
func (ac *ASMRClient) AddFailedTask(url, dirPath, fileName string, retryCount int) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.FailedTasks = append(ac.FailedTasks, FailedTask{
		URL:       url,
		DirPath:   dirPath,
		FileName:  fileName,
		RetryCount: retryCount,
	})
}

// RetryFailedTasks 重试所有失败的任务
func (ac *ASMRClient) RetryFailedTasks() bool {
	ac.mu.Lock()
	if len(ac.FailedTasks) == 0 {
		ac.mu.Unlock()
		return false
	}

	tasks := make([]FailedTask, len(ac.FailedTasks))
	copy(tasks, ac.FailedTasks)
	ac.FailedTasks = make([]FailedTask, 0)
	ac.mu.Unlock()

	utils.Warning(i18n.T("retrying", len(tasks), ac.MaxRetry))

	permanentlyFailed := make([]FailedTask, 0)
	retriedCount := 0
	for _, task := range tasks {
		if task.RetryCount >= ac.MaxRetry {
			utils.Error(i18n.T("max_retry_reached", task.FileName))
			permanentlyFailed = append(permanentlyFailed, task)
			continue
		}
		utils.Info(i18n.T("retrying", task.RetryCount+1, ac.MaxRetry) + ": " + task.FileName)
		ac.downloadFileWithRetry(task.URL, task.DirPath, task.FileName, task.RetryCount+1)
		retriedCount++
	}

	// 将永久失败的任务放回列表
	if len(permanentlyFailed) > 0 {
		ac.mu.Lock()
		ac.FailedTasks = append(ac.FailedTasks, permanentlyFailed...)
		ac.mu.Unlock()
	}

	// 只有当有任务被重试时才返回 true
	return retriedCount > 0
}

func (ac *ASMRClient) Download(id string) {
	id = strings.Replace(id, "RJ", "", 1)
	utils.Info(i18n.T("fetching_work_info", "RJ"+id))
	tracks, err := ac.GetVoiceTracks(id)
	if err != nil {
		utils.Error(i18n.T("request_failed", err))
		return
	}
	basePath := "downloads/RJ" + id
	ac.EnsureDir(tracks, basePath)
	utils.Success(i18n.T("work_info_fetched", "RJ"+id))
}

func (ac *ASMRClient) downloadFileWithRetry(url string, dirPath string, fileName string, retryCount int) {
	ac.downloadFileInternal(url, dirPath, fileName, retryCount)
}

func (ac *ASMRClient) DownloadFile(url string, dirPath string, fileName string) {
	ac.downloadFileInternal(url, dirPath, fileName, 0)
}

// 修改 downloadFileInternal 方法
func (ac *ASMRClient) downloadFileInternal(url string, dirPath string, fileName string, retryCount int) {
	if runtime.GOOS == "windows" {
		for _, str := range []string{"?", "<", ">", ":", "/", "\\", "*", "|"} {
			fileName = strings.Replace(fileName, str, "_", -1)
		}
	}
	
	// 最终保存路径 (Rclone 挂载路径)
	finalSavePath := dirPath + "/" + fileName

	// 1. 检查最终目标是否存在 (逻辑不变)
	headers := map[string]string{
		"Referer": "https://www.asmr.one/",
	}

	if utils.PathExists(finalSavePath) {
		localSize, err := utils.GetFileSize(finalSavePath)
		if err != nil {
			utils.Warning(i18n.T("file_error", err))
		} else {
			remoteSize, err := utils.GetRemoteFileSize(url, headers)
			if err != nil {
				utils.Warning(i18n.T("network_error", err))
				utils.Info(i18n.T("file_exists", finalSavePath))
				return
			}
			if localSize == remoteSize {
				utils.Info(i18n.T("file_exists", finalSavePath))
				return
			} else {
				utils.Warning(i18n.T("file_error", fmt.Sprintf("size mismatch: local=%d, remote=%d", localSize, remoteSize)))
			}
		}
	}

	// 2. 构造本地临时路径
	// 保持目录结构，避免文件名冲突
	// 例如: /root/asmr_temp/RJ123456/sound.wav
	relDir := strings.TrimPrefix(dirPath, "downloads/") // 假设 base 是 downloads
	tempDir := filepath.Join(LocalTempDir, relDir)
	_ = os.MkdirAll(tempDir, 0755)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		utils.Error("Failed to create temp dir: %v", err)
		return 
	}
	
	// 临时文件全路径
	tempFullPath := filepath.Join(tempDir, fileName)

	// 3. 修改 Downloader 初始化，下载到 tempFullPath
	// 注意：这里传入 tempDir 和 fileName
	downloader := utils.NewDownloader(url, tempDir, fileName, ac.ThreadCount, headers)
	downloader.FinalPath = finalSavePath
	downloader.RetryCount = retryCount
	
	// 这里需要拦截 Downloader 的 OnFailure，如果下载失败不移动
	originalFailure := downloader.OnFailure
	downloader.OnFailure = func(failedUrl, failedPath, failedName string, err error) {
		// 失败时，删除临时文件
		os.Remove(tempFullPath)
		if ac.FailedTasks != nil { // 确保 ac.AddFailedTask 可用
             ac.AddFailedTask(failedUrl, dirPath, failedName, retryCount) // 注意这里存回原始 dirPath
        }
        // 调用原始逻辑（如果有）
        if originalFailure != nil {
            originalFailure(failedUrl, failedPath, failedName, err)
        }
	}

	// 我们需要包装一下 TaskQueue 的处理逻辑，
    // 因为 Downloader 是在 WorkerPool 里异步执行的，
    // 我们无法直接在这里写 moveFile。
    
    // **最佳修改方案**：
    // 不改 WorkerPool，而是利用 Downloader 成功后的回调机制。
    // 但是现在的 Downloader 没有 Success 回调。
    // 我们可以在 downloader.go 中增加 OnSuccess，或者简单一点：
    // 修改 worker.go 的逻辑（见下文）。
    
	ac.WorkerPool.TaskQueue <- downloader
}

func (ac *ASMRClient) EnsureDir(tracks []track, basePath string) {
	path := basePath
	_ = os.MkdirAll(path, os.ModePerm)
	for _, t := range tracks {
		if t.Type != "folder" {
			ac.DownloadFile(t.MediaDownloadURL, path, t.Title)
		} else {
			ac.EnsureDir(t.Children, path+"/"+t.Title)
		}
	}
}
