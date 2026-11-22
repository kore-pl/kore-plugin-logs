package logic

// checkFault 在方法入口统一注入故障，返回结构化方法级错误。
// 四类故障分别映射协议错误码：进程失联/超时 → E_UPSTREAM，
// 磁盘满/cgroup OOM → E_INTERNAL；均由主包映射为 pluginapi.Error。
func checkFault(f Fault) *FaultError {
	switch f {
	case FaultNone:
		return nil
	case FaultKill:
		return methodErr(CodeUpstream, "进程被 kill -9 终止，方法无响应")
	case FaultTimeout:
		return methodErr(CodeUpstream, "方法执行超时（timeout）")
	case FaultDiskFull:
		return methodErr(CodeInternal, "磁盘满（ENOSPC），无法写入日志缓冲")
	case FaultCgroup:
		return methodErr(CodeInternal, "cgroup memory.max 超限，进程被 OOM killer 终止")
	default:
		return methodErr(CodeInvalid, "未知故障注入模式："+string(f))
	}
}

// ParseFault 把环境变量字符串收敛为受控 Fault 枚举，未知值不静默吞掉，
// 而是保留原值交由 checkFault 返回 E_INVALID（不伪装成正常路径）。
func ParseFault(raw string) Fault { return Fault(raw) }
