package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// EventHandler 定义事件处理器接口（观察者模式）
type EventHandler interface {
	OnCreate(path string)
	OnWrite(path string)
	OnRemove(path string)
	OnRename(path string)
	OnChmod(path string)
}

// LoggingHandler 一个简单的日志处理器实现
type LoggingHandler struct{}

func (h *LoggingHandler) OnCreate(path string) {
	log.Printf("[CREATE] %s", path)
}

func (h *LoggingHandler) OnWrite(path string) {
	log.Printf("[WRITE] %s", path)
}

func (h *LoggingHandler) OnRemove(path string) {
	log.Printf("[REMOVE] %s", path)
}

func (h *LoggingHandler) OnRename(path string) {
	log.Printf("[RENAME] %s", path)
}

func (h *LoggingHandler) OnChmod(path string) {
	log.Printf("[CHMOD] %s", path)
}

// Debouncer 事件去抖动器，避免事件风暴
type Debouncer struct {
	mu       sync.Mutex
	timers   map[string]*time.Timer
	duration time.Duration
}

// NewDebouncer 创建新的去抖动器
func NewDebouncer(duration time.Duration) *Debouncer {
	return &Debouncer{
		timers:   make(map[string]*time.Timer),
		duration: duration,
	}
}

// Debounce 对指定路径的事件进行去抖动处理
func (d *Debouncer) Debounce(path string, callback func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 如果已存在该路径的定时器，先停止它
	if timer, exists := d.timers[path]; exists {
		timer.Stop()
	}

	// 创建新的定时器
	d.timers[path] = time.AfterFunc(d.duration, func() {
		callback()
		d.mu.Lock()
		delete(d.timers, path)
		d.mu.Unlock()
	})
}

// FileWatcher 文件监控器
type FileWatcher struct {
	watcher   *fsnotify.Watcher
	handler   EventHandler
	done      chan struct{}
	recursive bool
	debouncer *Debouncer
}

// WatcherOption 配置选项函数类型
type WatcherOption func(*FileWatcher)

// WithRecursive 启用递归监控
func WithRecursive(recursive bool) WatcherOption {
	return func(fw *FileWatcher) {
		fw.recursive = recursive
	}
}

// WithDebounce 启用事件去抖动
func WithDebounce(duration time.Duration) WatcherOption {
	return func(fw *FileWatcher) {
		fw.debouncer = NewDebouncer(duration)
	}
}

// NewFileWatcher 创建新的文件监控器
func NewFileWatcher(handler EventHandler, opts ...WatcherOption) (*FileWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	fw := &FileWatcher{
		watcher:   watcher,
		handler:   handler,
		done:      make(chan struct{}),
		recursive: false,
		debouncer: nil,
	}

	// 应用配置选项
	for _, opt := range opts {
		opt(fw)
	}

	return fw, nil
}

// Watch 添加要监控的路径
func (fw *FileWatcher) Watch(path string) error {
	if fw.recursive {
		return fw.watchRecursive(path)
	}
	return fw.watcher.Add(path)
}

// watchRecursive 递归添加目录监控
func (fw *FileWatcher) watchRecursive(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			log.Printf("Adding watch: %s", path)
			if err := fw.watcher.Add(path); err != nil {
				return err
			}
		}
		return nil
	})
}

// Start 启动监控（非阻塞，启动后台goroutine）
func (fw *FileWatcher) Start() {
	go fw.eventLoop()
}

// eventLoop 事件处理循环
func (fw *FileWatcher) eventLoop() {
	for {
		select {
		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}
			fw.handleEvent(event)

		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)

		case <-fw.done:
			return
		}
	}
}

// handleEvent 处理事件（支持去抖动）
func (fw *FileWatcher) handleEvent(event fsnotify.Event) {
	// 如果是新建目录且启用了递归监控，动态添加watch
	if fw.recursive && event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			log.Printf("Adding watch for new directory: %s", event.Name)
			fw.watcher.Add(event.Name)
		}
	}

	// 如果启用了去抖动，则延迟处理
	if fw.debouncer != nil {
		fw.debouncer.Debounce(event.Name, func() {
			fw.dispatchEvent(event)
		})
	} else {
		fw.dispatchEvent(event)
	}
}

// dispatchEvent 分发事件到对应的处理方法
func (fw *FileWatcher) dispatchEvent(event fsnotify.Event) {
	// fsnotify 使用位掩码表示事件类型
	// 一个事件可能同时包含多种操作

	if event.Has(fsnotify.Create) {
		fw.handler.OnCreate(event.Name)
	}
	if event.Has(fsnotify.Write) {
		fw.handler.OnWrite(event.Name)
	}
	if event.Has(fsnotify.Remove) {
		fw.handler.OnRemove(event.Name)
	}
	if event.Has(fsnotify.Rename) {
		fw.handler.OnRename(event.Name)
	}
	if event.Has(fsnotify.Chmod) {
		fw.handler.OnChmod(event.Name)
	}
}

// Stop 停止监控
func (fw *FileWatcher) Stop() error {
	close(fw.done)
	return fw.watcher.Close()
}

func main() {
	// 创建事件处理器
	handler := &LoggingHandler{}

	// 创建文件监控器（启用递归监控和100ms去抖动）
	watcher, err := NewFileWatcher(
		handler,
		WithRecursive(true),
		WithDebounce(100*time.Millisecond),
	)
	if err != nil {
		log.Fatalf("failed to create watcher: %v", err)
	}
	defer watcher.Stop()

	// 添加要监控的路径（监控当前目录）
	watchPath := "."
	if len(os.Args) > 1 {
		watchPath = os.Args[1]
	}

	if err := watcher.Watch(watchPath); err != nil {
		log.Fatalf("failed to watch path %s: %v", watchPath, err)
	}

	log.Printf("Watching: %s (recursive: %v)", watchPath, true)
	log.Println("Press Ctrl+C to stop...")

	// 启动监控
	watcher.Start()

	// 等待中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
}
