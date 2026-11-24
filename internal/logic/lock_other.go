//go:build !unix

package logic

import (
	"os"
	"sync"
)

// fallbackLocks 保存非 unix 平台的进程内锁：该平台没有 flock 语义，
// 同一进程内的并发写仍然串行；跨进程并发写不在降级范围内（日志插件生产目标是 Linux）。
var fallbackLocks sync.Map // path -> *sync.Mutex

// lockFile 在非 unix 平台上退化为进程内排他锁，返回释放函数。
func lockFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = file.Close()
	value, _ := fallbackLocks.LoadOrStore(path, &sync.Mutex{})
	mutex, ok := value.(*sync.Mutex)
	if !ok {
		return nil, os.ErrInvalid
	}
	mutex.Lock()
	return mutex.Unlock, nil
}
