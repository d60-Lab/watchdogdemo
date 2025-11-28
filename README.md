# 一文讲透文件监控 Watchdog

> 仓库地址：https://github.com/d60-lab/watchdogdemo

## 引言

在软件开发中，我们经常需要监控文件系统的变化：配置文件热更新、日志文件轮转、代码热重载、文件同步服务等场景都离不开文件监控。今天我们就来深入理解文件监控的核心原理和设计思想。

## 一、什么是文件监控

文件监控（File Watching / File System Notification）是指程序能够**感知文件系统的变化**，包括：

- 文件/目录的创建（Create）
- 文件/目录的删除（Delete）
- 文件内容的修改（Modify）
- 文件/目录的重命名/移动（Rename/Move）
- 文件属性的变化（Chmod）

## 二、为什么需要文件监控

### 传统轮询方案的问题

最朴素的文件监控方式是**轮询（Polling）**：

```
while True:
    current_state = get_file_stats()
    if current_state != last_state:
        handle_change()
    sleep(interval)
```

这种方案存在明显缺陷：

| 问题 | 描述 |
|------|------|
| CPU 浪费 | 即使文件无变化，也在持续消耗 CPU |
| 响应延迟 | 延迟取决于轮询间隔，无法实时响应 |
| 扩展性差 | 监控文件数量增加，开销线性增长 |
| 难以捕获瞬时变化 | 快速的创建-删除可能被遗漏 |

### 事件驱动的优势

现代操作系统都提供了**基于事件的文件监控 API**，采用"订阅-通知"模式：

1. 程序向内核注册"我对这个目录感兴趣"
2. 内核在文件变化时主动通知程序
3. 程序被唤醒处理事件

这种方式：
- **零轮询开销**：无变化时程序休眠
- **实时响应**：事件发生即刻通知
- **内核级效率**：由操作系统高效管理

## 三、操作系统层的文件监控机制

不同操作系统提供了不同的底层 API：

### Linux: inotify

inotify 是 Linux 2.6.13+ 引入的文件监控子系统。

**核心概念：**
- `inotify_init()`: 创建 inotify 实例，返回文件描述符
- `inotify_add_watch()`: 添加监控路径
- `read()`: 阻塞读取事件（可配合 epoll 使用）
- `inotify_rm_watch()`: 移除监控

**支持的事件类型：**
```
IN_CREATE    - 文件/目录被创建
IN_DELETE    - 文件/目录被删除  
IN_MODIFY    - 文件被修改
IN_MOVED_FROM/TO - 文件被移动
IN_ATTRIB    - 元数据变化
IN_CLOSE_WRITE - 可写文件被关闭
```

**系统限制：**
```bash
# 每个用户的最大 watch 数量
cat /proc/sys/fs/inotify/max_user_watches
# 默认通常是 8192，可以调大
sysctl fs.inotify.max_user_watches=524288
```

### macOS/BSD: kqueue + FSEvents

**kqueue**: BSD 系统的通用事件通知机制
- 通过 `EVFILT_VNODE` 过滤器监控文件
- 需要为每个文件打开文件描述符（受 fd 限制）

**FSEvents**: macOS 专有的高层 API
- 基于路径而非 fd，更适合监控大量文件
- 支持历史事件回放
- 粒度较粗（目录级别）

### Windows: ReadDirectoryChangesW

Windows 提供 `ReadDirectoryChangesW` API：
- 支持同步和异步（IOCP）模式
- 可监控子目录
- 事件类型丰富

## 四、Watchdog 的设计思想

一个优秀的文件监控库需要解决以下问题：

### 1. 跨平台抽象

核心设计是**策略模式**：

```
                    ┌─────────────────┐
                    │   Observer      │
                    │   (抽象接口)     │
                    └────────┬────────┘
                             │
         ┌───────────────────┼───────────────────┐
         │                   │                   │
         ▼                   ▼                   ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│  InotifyEmitter │ │ FSEventsEmitter │ │ WindowsEmitter  │
│    (Linux)      │ │    (macOS)      │ │   (Windows)     │
└─────────────────┘ └─────────────────┘ └─────────────────┘
```

用户代码只依赖 `Observer` 接口，具体实现由库根据平台自动选择。

### 2. 事件模型统一

不同系统的事件类型和语义各异，watchdog 需要将它们映射到统一的事件模型：

```
FileSystemEvent
    ├── src_path: 事件发生的路径
    ├── is_directory: 是否是目录
    └── event_type: CREATE | DELETE | MODIFY | MOVE

DirCreatedEvent / FileCreatedEvent
DirDeletedEvent / FileDeletedEvent  
DirModifiedEvent / FileModifiedEvent
DirMovedEvent / FileMovedEvent (包含 dest_path)
```

### 3. 观察者模式

这是整个架构的核心：

```
┌────────────┐      schedule      ┌────────────────┐
│  Observer  │ ──────────────────▶│ EventHandler   │
│            │                    │                │
│  - start() │◀────  events  ─────│ - on_created() │
│  - stop()  │                    │ - on_deleted() │
│  - join()  │                    │ - on_modified()│
└────────────┘                    │ - on_moved()   │
       │                          └────────────────┘
       │ 内部维护
       ▼
┌────────────────────────────────────────────┐
│          ObservedWatch 列表                 │
│  ┌──────────────────────────────────────┐  │
│  │ path: /home/user/project             │  │
│  │ recursive: true                      │  │
│  │ handler: MyEventHandler              │  │
│  └──────────────────────────────────────┘  │
└────────────────────────────────────────────┘
```

### 4. 递归监控的处理

inotify 不原生支持递归监控，需要库来处理：

**方案一：初始扫描 + 动态添加**
1. 启动时扫描目录树，为每个子目录添加 watch
2. 收到 CREATE 事件且是目录时，动态添加 watch
3. 收到 DELETE 事件且是目录时，移除 watch

**方案二：使用支持递归的 API**
- FSEvents 原生支持递归
- ReadDirectoryChangesW 可指定递归标志

### 5. 事件合并与去重

文件系统事件可能产生大量冗余：

```
# 一次 vim 保存可能产生：
MODIFY file.txt
MODIFY file.txt
MODIFY file.txt
CREATE file.txt~
DELETE file.txt~
CHMOD file.txt
```

**去抖动（Debounce）设计：**
```
                    ┌─────────────────┐
  Events ──────────▶│  Event Queue    │
                    │  + Timer        │
                    └────────┬────────┘
                             │
                             │ 延迟 N ms 后
                             │ 合并同路径事件
                             ▼
                    ┌─────────────────┐
                    │  Handler        │
                    └─────────────────┘
```

### 6. 线程模型

典型设计采用**生产者-消费者**模式：

```
┌─────────────────────────────────────────────────┐
│                 Observer Thread                  │
│  ┌─────────────┐     ┌───────────────────────┐  │
│  │   Emitter   │────▶│    Event Channel      │  │
│  │ (读取系统事件)│     │ (线程安全的队列)       │  │
│  └─────────────┘     └───────────┬───────────┘  │
└──────────────────────────────────┼──────────────┘
                                   │
                                   ▼
                    ┌─────────────────────────────┐
                    │       Handler Goroutine     │
                    │   (用户定义的事件处理逻辑)    │
                    └─────────────────────────────┘
```

## 五、Go 实现示例

让我们用 Go 实现一个简单的文件监控器，展示核心设计思想：

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

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

// FileWatcher 文件监控器
type FileWatcher struct {
	watcher *fsnotify.Watcher
	handler EventHandler
	done    chan struct{}
}

// NewFileWatcher 创建新的文件监控器
func NewFileWatcher(handler EventHandler) (*FileWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &FileWatcher{
		watcher: watcher,
		handler: handler,
		done:    make(chan struct{}),
	}, nil
}

// Watch 添加要监控的路径
func (fw *FileWatcher) Watch(path string) error {
	return fw.watcher.Add(path)
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
			fw.dispatchEvent(event)

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

	// 创建文件监控器
	watcher, err := NewFileWatcher(handler)
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

	log.Printf("Watching: %s", watchPath)
	log.Println("Press Ctrl+C to stop...")

	// 启动监控
	watcher.Start()

	// 等待中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
}
```

### 代码解析

这个示例体现了几个核心设计：

**1. 接口抽象（策略模式）**
```go
type EventHandler interface {
    OnCreate(path string)
    OnWrite(path string)
    // ...
}
```
用户只需实现这个接口，就能定义自己的处理逻辑。

**2. 事件分发**
```go
func (fw *FileWatcher) dispatchEvent(event fsnotify.Event) {
    if event.Has(fsnotify.Create) {
        fw.handler.OnCreate(event.Name)
    }
    // ...
}
```
将底层事件映射到高层接口方法。

**3. 优雅关闭**
```go
case <-fw.done:
    return
```
通过 channel 信号实现优雅退出。

## 六、常见陷阱与解决方案

### 1. 事件风暴

**问题**：编辑器保存文件可能触发多个事件

**解决**：实现事件去抖动
```go
// 简化的去抖动逻辑
type Debouncer struct {
    timer    *time.Timer
    duration time.Duration
    callback func()
}

func (d *Debouncer) Trigger() {
    if d.timer != nil {
        d.timer.Stop()
    }
    d.timer = time.AfterFunc(d.duration, d.callback)
}
```

### 2. 递归监控

**问题**：inotify 不支持递归，新建子目录不会自动监控

**解决**：监听 CREATE 事件，动态添加 watch
```go
if event.Has(fsnotify.Create) {
    info, _ := os.Stat(event.Name)
    if info.IsDir() {
        watcher.Add(event.Name)  // 动态添加新目录
    }
}
```

### 3. 文件移动 vs 重命名

**问题**：同目录下重命名和跨目录移动是不同的

**解决**：
- 同目录重命名：收到 RENAME 事件
- 跨目录移动：源目录收到 DELETE，目标目录收到 CREATE

### 4. 符号链接

**问题**：是否跟踪符号链接指向的实际文件？

**解决**：明确配置 `follow_symlinks` 选项

### 5. Watch 数量限制

**问题**：inotify 有 `max_user_watches` 限制

**解决**：
- 调大系统限制
- 只监控必要的目录
- 对于超大项目，考虑使用轮询作为降级方案

## 七、实际应用场景

### 1. 配置热更新
```
监控 config.yaml → 检测 MODIFY → 重新加载配置 → 应用生效
```

### 2. 开发服务器热重载
```
监控 *.go 文件 → 检测 MODIFY → 重新编译 → 重启服务
```

### 3. 日志收集
```
监控 /var/log/*.log → 检测 WRITE → 读取增量 → 发送到日志系统
```

### 4. 文件同步服务
```
监控本地目录 → 检测任何变化 → 同步到远端
```

## 八、总结

文件监控的核心设计思想：

1. **事件驱动优于轮询**：利用操作系统内核的通知机制，实现零开销等待

2. **平台抽象**：通过接口隔离不同系统的实现细节

3. **观察者模式**：解耦事件源和事件处理逻辑

4. **生产者-消费者**：异步处理，避免阻塞事件读取

5. **优雅降级**：在不支持原生通知的文件系统上回退到轮询

理解这些原理，无论是使用现有的 watchdog 库，还是需要实现自己的文件监控逻辑，都能做到心中有数。

## 九、快速开始

### 克隆仓库

```bash
git clone https://github.com/d60-lab/watchdogdemo.git
cd watchdogdemo
```

### 编译运行

```bash
# 下载依赖
go mod tidy

# 编译
go build -o watchdogdemo .

# 运行（监控当前目录）
./watchdogdemo

# 运行（监控指定目录）
./watchdogdemo /path/to/watch
```

### 测试效果

在一个终端运行监控程序：
```bash
./watchdogdemo testdir
```

在另一个终端进行文件操作：
```bash
# 创建文件
touch testdir/test.txt

# 修改文件
echo "hello" >> testdir/test.txt

# 删除文件
rm testdir/test.txt

# 创建子目录（会自动添加监控）
mkdir testdir/subdir
touch testdir/subdir/file.txt
```

你将在监控终端看到类似输出：
```
2025/01/01 12:00:00 [CREATE] testdir/test.txt
2025/01/01 12:00:01 [WRITE] testdir/test.txt
2025/01/01 12:00:02 [REMOVE] testdir/test.txt
2025/01/01 12:00:03 Adding watch for new directory: testdir/subdir
2025/01/01 12:00:03 [CREATE] testdir/subdir
2025/01/01 12:00:04 [CREATE] testdir/subdir/file.txt
```

---

*参考资源：*
- Linux inotify(7) man page
- fsnotify/fsnotify (Go)
- gorakhargosh/watchdog (Python)
- Facebook Watchman
