//go:build unix

package logic

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockWaitTimeout 是获取文件锁的最长等待时间：并发写者只持有毫秒级窗口，
// 超时说明有异常持有者，宁可返回结构化错误也不无限阻塞 RPC。
const lockWaitTimeout = 5 * time.Second

// lockFile 在 path 上取排他 flock（跨进程 + 跨 goroutine），返回释放函数。
// 非阻塞尝试 + 短退避重试：等待上限 lockWaitTimeout，超时返回错误。
func lockFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, errors.New("等待文件锁超时（5s），可能有异常持有者")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
