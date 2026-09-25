// Package logger —— 插件内日志（内存环 + stderr + 可选文件）。
//
// 移植自 qoder2api/logger，做了三处收敛：
//   - sink 从 stdout 改为 stderr：动态库被 CPA 宿主加载，stdout 属于宿主进程；
//   - 去掉 gzip 归档，只保留按天文件与保留期清理；
//   - 内存环供管理页展示最近日志。
//
// 安全约定：调用方不得把完整凭证写入日志，只允许输出前缀（见 tokenPrefix）。
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level 是日志级别。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelError Level = "error"
)

// Entry 是一条日志记录。
type Entry struct {
	Seq     int       `json:"seq"`
	Time    time.Time `json:"time"`
	Level   Level     `json:"level"`
	Message string    `json:"message"`
}

// LogPage 是增量拉取结果。
type LogPage struct {
	Entries []Entry `json:"entries"`
	LastSeq int     `json:"last_seq"`
}

const (
	maxEntries = 1000
	retainDays = 7
)

var (
	mu           sync.RWMutex
	entries      []Entry
	nextSeq      = 1
	currentLevel = LevelInfo

	fileMu     sync.Mutex
	fileDir    string
	fileHandle *os.File
	fileDate   string
)

// SetLevel 设置最低输出级别（未知值按 info 处理）。
func SetLevel(level string) {
	mu.Lock()
	defer mu.Unlock()
	switch Level(strings.ToLower(strings.TrimSpace(level))) {
	case LevelDebug:
		currentLevel = LevelDebug
	case LevelError:
		currentLevel = LevelError
	default:
		currentLevel = LevelInfo
	}
}

// CurrentLevel 返回当前最低输出级别。
func CurrentLevel() Level {
	mu.RLock()
	defer mu.RUnlock()
	return currentLevel
}

// InitFile 启用/停用文件 sink。
// enabled=false 或 dir 为空时关闭文件输出；失败不阻断业务，只降级为 stderr。
func InitFile(enabled bool, dir string) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	if !enabled || strings.TrimSpace(dir) == "" {
		closeFileLocked()
		fileDir = ""
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	fileDir = dir
	cleanupOldFiles(dir, retainDays)
	_, err := ensureFileLocked()
	return err
}

// Close 关闭文件 sink，插件 shutdown 时调用。
func Close() {
	fileMu.Lock()
	defer fileMu.Unlock()
	closeFileLocked()
	fileDir = ""
}

func closeFileLocked() {
	if fileHandle != nil {
		_ = fileHandle.Close()
		fileHandle = nil
		fileDate = ""
	}
}

// ensureFileLocked 保证 fileHandle 指向当天文件。调用方必须持有 fileMu。
func ensureFileLocked() (*os.File, error) {
	if fileDir == "" {
		return nil, fmt.Errorf("file sink not initialized")
	}
	today := time.Now().Format("20060102")
	if fileHandle != nil && fileDate == today {
		return fileHandle, nil
	}
	if fileHandle != nil {
		_ = fileHandle.Close()
		fileHandle = nil
	}
	path := filepath.Join(fileDir, "qoder2api-"+today+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fileHandle = f
	fileDate = today
	return f, nil
}

// cleanupOldFiles 删除超过保留期的日志文件。
func cleanupOldFiles(dir string, retain int) {
	if retain <= 0 {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "qoder2api-*.log"))
	cutoff := time.Now().AddDate(0, 0, -retain)
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func shouldLog(level Level) bool {
	mu.RLock()
	defer mu.RUnlock()
	priority := map[Level]int{LevelDebug: 0, LevelInfo: 1, LevelError: 2}
	return priority[level] >= priority[currentLevel]
}

func log(level Level, format string, args ...interface{}) {
	if !shouldLog(level) {
		return
	}
	entry := Entry{Time: time.Now(), Level: level, Message: fmt.Sprintf(format, args...)}

	mu.Lock()
	entry.Seq = nextSeq
	nextSeq++
	entries = append(entries, entry)
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	mu.Unlock()

	line := fmt.Sprintf("[qoder2api][%s][%s] %s", entry.Time.Format("15:04:05.000"), level, entry.Message)
	fmt.Fprintln(os.Stderr, line)

	fileMu.Lock()
	if f, err := ensureFileLocked(); err == nil {
		_, _ = f.WriteString(line + "\n")
	}
	fileMu.Unlock()
}

// Debug 输出 debug 级日志。
func Debug(format string, args ...interface{}) { log(LevelDebug, format, args...) }

// Info 输出 info 级日志。
func Info(format string, args ...interface{}) { log(LevelInfo, format, args...) }

// Error 输出 error 级日志。
func Error(format string, args ...interface{}) { log(LevelError, format, args...) }

// GetLogs 返回最近 limit 条日志（limit<=0 返回全部）。
func GetLogs(limit int) []Entry {
	mu.RLock()
	defer mu.RUnlock()
	if limit <= 0 || limit > len(entries) {
		limit = len(entries)
	}
	start := len(entries) - limit
	result := make([]Entry, limit)
	copy(result, entries[start:])
	return result
}

// GetLogsSince 返回 seq > afterSeq 的增量条目与当前最大 seq。
func GetLogsSince(afterSeq, limit int) LogPage {
	mu.RLock()
	defer mu.RUnlock()
	result := make([]Entry, 0, 32)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Seq <= afterSeq {
			break
		}
		result = append(result, entries[i])
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Seq < result[j].Seq })
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	last := 0
	if len(entries) > 0 {
		last = entries[len(entries)-1].Seq
	}
	return LogPage{Entries: result, LastSeq: last}
}
