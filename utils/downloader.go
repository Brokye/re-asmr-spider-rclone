package utils

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	defaultUA                    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.198 Safari/537.36"
	ErrUnsupportedMultiThreading = errors.New("unsupported multi-threading")
	// 缓冲区维持 4MB
	bufferSize = 8 * 1024 * 1024
	
	// 🔥 【新增】下载限速设置
	// 设置为 20MB/s (20 * 1024 * 1024)
	// 如果你的 Rclone 上传能稳定 30MB/s，可以改大；如果只有 10MB/s，请改小。
	// 目的：防止下载太快填满 Rclone 缓存导致程序假死。
	SpeedLimit = 50 * 1024 * 1024
)

type BlockMetaData struct {
	BeginOffset    int64
	EndOffset      int64
	DownloadedSize int64
}

type MultiThreadDownloader struct {
	Url         string
	SavePath    string
	FileName    string
	FullPath    string
	FinalPath   string
	Client      *http.Client
	Headers     map[string]string
	Blocks      []*BlockMetaData
	ThreadCount int
	ProgressBar *ProgressBar
	OnFailure   func(url, savePath, fileName string, err error)
	RetryCount  int
}

// progressWriter 封装 io.Writer 以更新进度条
type progressWriter struct {
	w   io.Writer
	bar *ProgressBar
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if n > 0 && pw.bar != nil {
		pw.bar.Add(int64(n))
	}
	return n, err
}

// 🔥 【新增】限速读取器
// 通过在 Read 操作中增加延时来实现限速
type RateLimitedReader struct {
	r     io.Reader
	start time.Time
}

func (r *RateLimitedReader) Read(p []byte) (int, error) {
	// 记录开始时间
	start := time.Now()
	
	n, err := r.r.Read(p)
	
	if n > 0 && SpeedLimit > 0 {
		// 计算读取这些数据理论上需要的最少时间
		// 期望耗时 = 数据量 / 限制速度
		expectedDuration := time.Duration(float64(n) / float64(SpeedLimit) * float64(time.Second))
		
		// 实际耗时
		elapsed := time.Since(start)
		
		// 如果读得太快（实际耗时 < 期望耗时），就睡一会儿
		if elapsed < expectedDuration {
			time.Sleep(expectedDuration - elapsed)
		}
	}
	return n, err
}

func NewDownloader(url string, path string, name string, threadCount int, headers map[string]string) *MultiThreadDownloader {
	// 修复超时问题：复制 Client 并移除超时限制
	globalClient := Client.Get().(*http.Client)
	downloadClient := *globalClient
	downloadClient.Timeout = 0 // 设置为 0，防止大文件下载超时

	return &MultiThreadDownloader{
		Url:         url,
		SavePath:    path,
		FileName:    name,
		FullPath:    path + "/" + name,
		Client:      &downloadClient,
		Headers:     headers,
		Blocks:      nil,
		ThreadCount: threadCount,
	}
}

func (m *MultiThreadDownloader) Download() error {
	if m.ThreadCount < 2 {
		return m.singleThreadDownload()
	}
	if err := m.initDownload(); err != nil {
		if err == ErrUnsupportedMultiThreading {
			return m.singleThreadDownload()
		}
		return err
	}
	wg := sync.WaitGroup{}
	wg.Add(len(m.Blocks))
	var lastErr error
	for i := range m.Blocks {
		go func(b *BlockMetaData) {
			defer wg.Done()
			if err := m.downloadBlocks(b); err != nil {
				lastErr = err
			}
		}(m.Blocks[i])
	}
	wg.Wait()
	if m.ProgressBar != nil {
		m.ProgressBar.Finish()
	}
	return lastErr
}

func (m *MultiThreadDownloader) initDownload() error {
	var contentLength int64

	copyStream := func(s io.ReadCloser, size int64) error {
		file, err := os.OpenFile(m.FullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}
		defer file.Close()

		writer := bufio.NewWriterSize(file, bufferSize)
		defer writer.Flush()

		if size > 0 {
			m.ProgressBar = NewProgressBar(size, m.FileName)
		}

		pw := &progressWriter{w: writer, bar: m.ProgressBar}
		
		// 🔥 使用限速读取器包裹 Body
		limiter := &RateLimitedReader{r: s}

		buf := make([]byte, bufferSize)
		_, err = io.CopyBuffer(pw, limiter, buf)
		if err != nil {
			return err
		}

		if m.ProgressBar != nil {
			m.ProgressBar.Finish()
		}
		return ErrUnsupportedMultiThreading
	}

	req, err := http.NewRequest("GET", m.Url, nil)
	if err != nil {
		return err
	}

	for k, v := range m.Headers {
		req.Header.Set(k, v)
	}
	if _, ok := m.Headers["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{defaultUA}
	}
	req.Header.Set("range", "bytes=0-")
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("response status unsuccessful: " + strconv.FormatInt(int64(resp.StatusCode), 10))
	}

	if resp.StatusCode == 200 {
		return copyStream(resp.Body, resp.ContentLength)
	}

	if resp.StatusCode == 206 {
		contentLength = resp.ContentLength
		if contentLength > 0 {
			m.ProgressBar = NewProgressBar(contentLength, m.FileName)
		}

		blockSize := func() int64 {
			if contentLength > 1024*1024 {
				return (contentLength / int64(m.ThreadCount)) - 10
			}
			return contentLength
		}()

		if blockSize == contentLength {
			return copyStream(resp.Body, contentLength)
		}

		var tmp int64
		for tmp+blockSize < contentLength {
			m.Blocks = append(m.Blocks, &BlockMetaData{
				BeginOffset: tmp,
				EndOffset:   tmp + blockSize - 1,
			})
			tmp += blockSize
		}
		m.Blocks = append(m.Blocks, &BlockMetaData{
			BeginOffset: tmp,
			EndOffset:   contentLength - 1,
		})
		return nil
	}
	return errors.New("unknown status code")
}

func (m *MultiThreadDownloader) downloadBlocks(block *BlockMetaData) error {
	req, _ := http.NewRequest("GET", m.Url, nil)
	file, err := os.OpenFile(m.FullPath, os.O_WRONLY, 0666)
	if err != nil {
		file, err = os.OpenFile(m.FullPath, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			return err
		}
	}
	defer file.Close()

	if _, err := file.Seek(block.BeginOffset, io.SeekStart); err != nil {
		return err
	}
	
	writer := bufio.NewWriterSize(file, bufferSize)
	defer writer.Flush()

	for k, v := range m.Headers {
		req.Header.Set(k, v)
	}
	if _, ok := m.Headers["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{defaultUA}
	}
	req.Header.Set("range", "bytes="+strconv.FormatInt(block.BeginOffset, 10)+"-"+strconv.FormatInt(block.EndOffset, 10))
	
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("response status unsuccessful: " + strconv.FormatInt(int64(resp.StatusCode), 10))
	}

	buffer := make([]byte, bufferSize)
	
	// 🔥 仅在循环内部通过 Sleep 简单控制，不复用 Reader 以简化 Seek 逻辑
	for {
		// 记录开始时间
		start := time.Now()
		
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			// 1. 先进行限速控制
			if SpeedLimit > 0 {
				expectedDuration := time.Duration(float64(n) / float64(SpeedLimit) * float64(time.Second))
				elapsed := time.Since(start)
				if elapsed < expectedDuration {
					time.Sleep(expectedDuration - elapsed)
				}
			}

			// 2. 再处理写入逻辑
			bytesToWrite := int64(n)
			remaining := block.EndOffset + 1 - block.BeginOffset
			if bytesToWrite > remaining {
				bytesToWrite = remaining
			}

			if _, writeErr := writer.Write(buffer[:bytesToWrite]); writeErr != nil {
				return writeErr
			}
			
			block.BeginOffset += bytesToWrite
			block.DownloadedSize += bytesToWrite

			if m.ProgressBar != nil {
				m.ProgressBar.Add(bytesToWrite)
			}
			
			if block.BeginOffset > block.EndOffset {
				break
			}
		}
		
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func (m *MultiThreadDownloader) singleThreadDownload() error {
	file, err := os.OpenFile(m.FullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriterSize(file, bufferSize)
	defer writer.Flush()

	req, err := http.NewRequest("GET", m.Url, nil)
	if err != nil {
		return err
	}

	for k, v := range m.Headers {
		req.Header.Set(k, v)
	}
	if _, ok := m.Headers["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{defaultUA}
	}

	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.ContentLength > 0 {
		m.ProgressBar = NewProgressBar(resp.ContentLength, m.FileName)
	}

	pw := &progressWriter{w: writer, bar: m.ProgressBar}
	// 🔥 使用限速 Reader
	limiter := &RateLimitedReader{r: resp.Body}
	buf := make([]byte, bufferSize)
	
	if _, err := io.CopyBuffer(pw, limiter, buf); err != nil {
		return err
	}

	if m.ProgressBar != nil {
		m.ProgressBar.Finish()
	}
	return nil
}
