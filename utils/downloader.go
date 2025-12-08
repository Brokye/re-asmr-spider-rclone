package utils

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
)

var (
	defaultUA                    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.198 Safari/537.36"
	ErrUnsupportedMultiThreading = errors.New("unsupported multi-threading")
	// bufferSize 定义缓冲区大小为 8mb，平衡内存占用和 CPU 效率
	bufferSize = 8 * 1024 * 1024
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

func NewDownloader(url string, path string, name string, threadCount int, headers map[string]string) *MultiThreadDownloader {
	return &MultiThreadDownloader{
		Url:         url,
		SavePath:    path,
		FileName:    name,
		FullPath:    path + "/" + name,
		// 注意：这里假设 Client 变量在外部包已定义并初始化
		Client:      Client.Get().(*http.Client),
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
			// 如果不支持多线程（例如服务器不支持 Range），回退到单线程
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

	// 辅助函数：使用 io.CopyBuffer 优化流式复制
	copyStream := func(s io.ReadCloser, size int64) error {
		file, err := os.OpenFile(m.FullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}
		defer file.Close()

		// 使用 bufio 减少磁盘 IO 系统调用
		writer := bufio.NewWriterSize(file, bufferSize)
		defer writer.Flush()

		// 创建进度条
		if size > 0 {
			m.ProgressBar = NewProgressBar(size, m.FileName)
		}

		// 封装 writer 以自动更新进度
		pw := &progressWriter{w: writer, bar: m.ProgressBar}
		
		buf := make([]byte, bufferSize)
		_, err = io.CopyBuffer(pw, s, buf)
		if err != nil {
			return err
		}

		if m.ProgressBar != nil {
			m.ProgressBar.Finish()
		}
		return ErrUnsupportedMultiThreading // 按照原逻辑返回此错误以终止后续多线程逻辑
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
	// 尝试获取文件头信息或探测 Range 支持
	req.Header.Set("range", "bytes=0-")
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("response status unsuccessful: " + strconv.FormatInt(int64(resp.StatusCode), 10))
	}

	// 如果服务器直接返回 200 (不支持 Range) 或者没有 ContentLength
	if resp.StatusCode == 200 {
		return copyStream(resp.Body, resp.ContentLength)
	}

	if resp.StatusCode == 206 {
		contentLength = resp.ContentLength
		// 创建进度条
		if contentLength > 0 {
			m.ProgressBar = NewProgressBar(contentLength, m.FileName)
		}

		blockSize := func() int64 {
			if contentLength > 1024*1024 {
				return (contentLength / int64(m.ThreadCount)) - 10
			}
			return contentLength
		}()

		// 如果块大小等于内容长度，说明不需要分块
		if blockSize == contentLength {
			return copyStream(resp.Body, contentLength)
		}

		// 计算分块
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
	
	// OpenFile 可以在多线程下安全地对同一个文件进行 WriteAt 或 Seek+Write，但要注意文件描述符
	file, err := os.OpenFile(m.FullPath, os.O_WRONLY, 0666) // 移除 O_CREATE，因为 initDownload 或 single 应该已经创建了文件，或者需要确保文件存在
	if err != nil {
		// 如果文件不存在，尝试创建（防御性）
		file, err = os.OpenFile(m.FullPath, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			return err
		}
	}
	defer file.Close()

	// 定位到该块的起始位置
	if _, err := file.Seek(block.BeginOffset, io.SeekStart); err != nil {
		return err
	}
	
	// 即使是 Seek 写入，使用 bufio 也是好的，但要注意 Flush
	writer := bufio.NewWriterSize(file, bufferSize)
	// bufio.Writer 并不支持 Seek 后的随机写安全（它会顺序写）。
	// 但由于我们每个协程持有一个独立的 file descriptor (os.Open)，
	// 且每个协程只负责一段连续的区域，所以 bufio + Seek 是可行的，
	// 只要我们只调用一次 Seek，然后一直 Write 直到结束。
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

	// 优化：增大 Buffer，手动循环以控制 Range 边界
	buffer := make([]byte, bufferSize) // 32KB buffer
	
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			// 计算需要写入的大小，防止多写（虽然 Range 请求应该由服务器保证，但客户端检查更安全）
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

	// 优化：使用 bufio
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

	// 创建进度条
	if resp.ContentLength > 0 {
		m.ProgressBar = NewProgressBar(resp.ContentLength, m.FileName)
	}

	// 优化：使用 io.CopyBuffer 替代手动循环
	pw := &progressWriter{w: writer, bar: m.ProgressBar}
	buf := make([]byte, bufferSize)
	
	if _, err := io.CopyBuffer(pw, resp.Body, buf); err != nil {
		return err
	}

	if m.ProgressBar != nil {
		m.ProgressBar.Finish()
	}
	return nil
}
