package utils

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"re-asmr-spider/i18n"
)

// 🔥 Rclone 缓存监控配置 (基于 Rclone --vfs-cache-max-size 20GB)
// 暂停阈值：18GB (当缓存超过此值，程序停止向挂载点移动文件)
const RclonePauseThreshold = 18 * 1024 * 1024 * 1024
// 恢复阈值：15GB (当缓存降到此值，程序恢复写入)
const RcloneResumeThreshold = 15 * 1024 * 1024 * 1024

// Rclone API 地址 (请确保 Rclone 挂载命令中使用了 --rc-addr 127.0.0.1:5572)
const RcloneAPIUrl = "http://127.0.0.1:5572/vfs/stats"

type WorkerChan chan *MultiThreadDownloader

type WorkerPool struct {
	sync.WaitGroup
	cond      *sync.Cond
	TaskQueue WorkerChan
	Limit     int
	Count     int
}

// 定义 Rclone 返回的 JSON 结构 (已修复，嵌套到 diskCache.bytesUsed)
type RcloneVFSStats struct {
	DiskCache struct {
		BytesUsed int64 `json:"bytesUsed"`
	} `json:"diskCache"` 
}

func NewWorkerPool(WorkerCount int) *WorkerPool {
	return &WorkerPool{
		cond:      sync.NewCond(&sync.Mutex{}),
		Limit:     WorkerCount,
		TaskQueue: make(WorkerChan, WorkerCount),
	}
}

// 🔥 新增：通过 API 获取 Rclone 当前缓存占用
func getRcloneCacheUsage() (int64, error) {
	// Rclone RC 接口需要 POST 请求
	resp, err := http.Post(RcloneAPIUrl, "application/json", strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var stats RcloneVFSStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return 0, err
	}

	// 返回嵌套结构中的 BytesUsed
	return stats.DiskCache.BytesUsed, nil
}

func (wp *WorkerPool) Start() {
	go func() {
		for t := range wp.TaskQueue {
			wp.cond.L.Lock()
			for wp.Count >= wp.Limit {
				wp.cond.Wait()
			}
			wp.Add(1)
			wp.cond.L.Unlock()
			go func(t *MultiThreadDownloader) {
				wp.cond.L.Lock()
				wp.Count++
				wp.cond.L.Unlock()
				defer func() {
					wp.cond.L.Lock()
					wp.Count--
					wp.Done()
					wp.cond.Broadcast()
					wp.cond.L.Unlock()
				}()

				// 更新活动时间
				GlobalMonitor.UpdateActivity()

				// 1. 下载到本地临时目录
				err := t.Download()
				if err != nil {
					Error(i18n.T("download_error", t.FullPath, err))
					_ = os.Remove(t.FullPath)
					GlobalMonitor.UpdateActivity()
					if t.OnFailure != nil {
						t.OnFailure(t.Url, t.SavePath, t.FileName, err)
					}
					return
				}

				// 2. 智能流控与移动文件
				if t.FinalPath != "" && t.FinalPath != t.FullPath {
					
					// 🔥🔥 Rclone 缓存监控流控 🔥🔥
					for {
						usage, err := getRcloneCacheUsage()
						if err != nil {
							// 连接失败，打印错误并暂停，避免误判
							Error("无法连接 Rclone API (请确认已添加 --rc 参数): %v", err)
							time.Sleep(10 * time.Second)
							GlobalMonitor.UpdateActivity()
							continue
						}

						usageGB := float64(usage) / 1024 / 1024 / 1024

						// 如果当前缓存超过暂停阈值 (18GB)
						if usage > RclonePauseThreshold {
							Warning("Rclone 缓存爆满 (当前: %.2f GB), 暂停移动文件...", usageGB)
							
							// 进入等待模式，直到缓存降到恢复阈值 (10GB) 以下
							for {
								time.Sleep(10 * time.Second)
								GlobalMonitor.UpdateActivity()
								
								newUsage, err := getRcloneCacheUsage()
								if err == nil {
									if newUsage < RcloneResumeThreshold {
										Success("Rclone 缓存已清理 (当前: %.2f GB), 恢复运行", float64(newUsage)/1024/1024/1024)
										break // 退出内部等待循环
									}
								}
							}
							break // 退出外部检查循环
						} else {
							// 缓存未满，直接通过
							break
						}
					}
					// 🔥🔥 流控结束 🔥🔥

					// 确保目标文件夹存在
					if err := os.MkdirAll(filepath.Dir(t.FinalPath), 0755); err != nil {
						Error(i18n.T("download_error", "Mkdir FinalPath", err))
					} else {
						// 移动文件 (复制+删除)
						srcFile, err := os.Open(t.FullPath)
						if err == nil {
							dstFile, err := os.Create(t.FinalPath)
							if err == nil {
								_, copyErr := io.Copy(dstFile, srcFile)
								srcFile.Close()
								dstFile.Close()
								
								if copyErr == nil {
									os.Remove(t.FullPath) // 成功后删除本地临时文件
								} else {
									Error("写入挂载点失败: %v", copyErr)
								}
							} else {
								srcFile.Close()
								Error("无法创建目标文件: %v", err)
							}
						} else {
							Error("无法打开源文件: %v", err)
						}
					}
				}

				GlobalMonitor.UpdateActivity()
				displayPath := t.FullPath
				if t.FinalPath != "" {
					displayPath = t.FinalPath
				}
				Success(i18n.T("download_completed", displayPath))
			}(t)
		}
	}()
}
