package logic

import (
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 自动维护（切割 + 清理）是**默认行为**：装完插件不配任何参数就生效。
//
// 触发方式与 G-19 的关系（内核仓库门禁：唯一周期来源是 internal/core/scheduler/ 的单 ticker）：
//   - 本文件**没有** ticker / AfterFunc / 周期 goroutine，不构成第二个周期源；
//   - 周期由内核 scheduler 的内置任务 `builtin-logs-maintain` 提供，到点调用 logs.maintain；
//   - 按大小判断在**本方法执行时**完成（没有写入路径可挂：本插件不写宿主日志文件，
//     写日志的是 nginx/mysql/容器；插件只读它们，因此"写入时判断"落在"维护时判断"）。
//
// 切割语义选 copytruncate 而不是 rename：rename 后写日志的进程仍持有旧 inode，
// 不重载就永远写不进新文件（logs.rotate 的手动接口保留这一语义并如实提示）。
// 自动切割必须"不改动写方"，所以默认走 copytruncate——代价是复制与截断之间的
// 极短窗口内写进去的字节可能丢失，这与 logrotate 的 copytruncate 完全同源。

// DefaultRotateSizeMB 是单文件切割阈值默认值（100 MiB）。
//
// 依据：1Panel 的 logrotate 模板用 `maxsize 100M`，nginx 自己的 buffer/单文件
// 运维经验值也是 100 MiB 量级；宝塔与 1Panel 都按天切割、按天/按大小双条件触发。
const DefaultRotateSizeMB = 100

// MaxRotateSizeMB 是切割阈值的上限（1 TiB，实际等于"永不按大小切割"）。
const MaxRotateSizeMB = 1048576

// maintainFilePerm 是自动切割产物的权限（0640：与常见系统日志同口径，组可读、其他不可读）。
const maintainFilePerm os.FileMode = 0o640

// MaintainRequest / MaintainAction / MaintainResult 是 logs.maintain 的入参与结果。
// Now 只由测试注入（JSON "-"），生产取 time.Now。
type MaintainRequest struct {
	Source   string    `json:"source"`
	DryRun   bool      `json:"dry_run"`
	StateDir string    `json:"-"`
	Now      time.Time `json:"-"`
	Fault    Fault     `json:"-"`
	Debugf   Debugf    `json:"-"`
}

// MaintainAction 描述单来源本次做了什么（空字段代表没做）。
type MaintainAction struct {
	Source    string   `json:"source"`
	File      string   `json:"file,omitempty"`
	RotatedTo string   `json:"rotated_to,omitempty"`
	RotatedBy string   `json:"rotated_by,omitempty"` // size=按大小 / daily=按天
	Trimmed   []string `json:"trimmed"`
	Skipped   string   `json:"skipped,omitempty"`
}

// MaintainResult 是 logs.maintain 的结构化结果。
type MaintainResult struct {
	Auto       bool             `json:"auto"`
	DryRun     bool             `json:"dry_run"`
	Sources    []MaintainAction `json:"sources"`
	Rotated    int              `json:"rotated"`
	Cleaned    int              `json:"cleaned"`
	Policy     RetentionPolicy  `json:"policy"`
	Quarantine string           `json:"quarantine_dir,omitempty"`
	Note       string           `json:"note,omitempty"`
}

// Maintain 对登记的文件来源执行一次"先切割、再清理"的自动维护。
//
// 默认行为（不需要任何 retention.set）：
//   - 按天切割：当天还没有切割产物、且活跃文件非空 → 切一份；
//   - 按大小切割：活跃文件超过 rotate_size_mb（默认 100 MiB）→ 切一份；
//   - 按保留策略清理：超天数 / 超份数 / 超体积的切割文件移入回收区（可 logs.clean.restore 恢复）。
//
// 活跃文件永不清除，只被 copytruncate；整个过程不新增定时器。
func (r *Registry) Maintain(req MaintainRequest) (*MaintainResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	stateDir := strings.TrimSpace(req.StateDir)
	if stateDir == "" {
		return nil, methodErr(CodeInvalid, "KORE_STATE_DIR 未设置，无法自动维护日志")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	policies, ferr := loadRetention(stateDir)
	if ferr != nil {
		return nil, ferr
	}
	targets := make([]string, 0, len(r.order))
	if req.Source != "" {
		entry, perr := r.lookup(req.Source)
		if perr != nil {
			return nil, perr
		}
		if entry.spec.Type == SourceTypeJournal {
			return nil, methodErr(CodeUnsupported, "journal 来源没有可切割的日志文件")
		}
		targets = append(targets, entry.spec.Name)
	} else {
		for _, name := range r.order {
			if r.entries[name].spec.Type == SourceTypeFile {
				targets = append(targets, name)
			}
		}
	}
	result := &MaintainResult{Auto: true, DryRun: req.DryRun,
		Sources: make([]MaintainAction, 0, len(targets)),
		Policy:  defaultRetentionPolicy(), Quarantine: filepath.Join(stateDir, "quarantine")}
	for _, name := range targets {
		entry := r.entries[name]
		policy := policyFor(policies, name)
		action := MaintainAction{Source: name, Trimmed: make([]string, 0)}
		realDir, base, derr := r.secureSourceDir(entry)
		if derr != nil {
			action.Skipped = derr.Message
			result.Sources = append(result.Sources, action)
			continue
		}
		action.File = base
		active, aerr := lstatRegular(filepath.Join(realDir, base))
		if aerr != nil {
			action.Skipped = aerr.Message
			result.Sources = append(result.Sources, action)
			continue
		}
		reason := rotateReason(realDir, base, active.size, policy, now)
		if reason != "" && !req.DryRun {
			rotated, rerr := copyTruncate(realDir, base, now)
			if rerr != nil {
				action.Skipped = rerr.Message
				result.Sources = append(result.Sources, action)
				continue
			}
			action.RotatedTo, action.RotatedBy = rotated, reason
			result.Rotated++
			debugf(req.Debugf, "logs.maintain source=%s rotated=%s by=%s", name, rotated, reason)
		}
		files, perr := listSourceFiles(realDir, base)
		if perr != nil {
			action.Skipped = perr.Message
			result.Sources = append(result.Sources, action)
			continue
		}
		candidates := planClean(files, base, policy, now)
		if len(candidates) > 0 && !req.DryRun {
			moved, failed := moveToQuarantine(stateDir, name, realDir, candidates)
			action.Trimmed = moved
			result.Cleaned += len(moved)
			if len(failed) > 0 {
				action.Skipped = failureSummary(failed)
			}
		} else if len(candidates) > 0 {
			for _, cand := range candidates {
				action.Trimmed = append(action.Trimmed, cand.File)
			}
		}
		result.Sources = append(result.Sources, action)
	}
	if !req.DryRun && result.Cleaned > 0 {
		if _, _, qerr := enforceQuarantineQuota(stateDir); qerr != nil {
			return nil, qerr
		}
	}
	if req.DryRun {
		result.Note = "dry_run=true 只预演，不改动任何文件"
		return result, nil
	}
	result.Note = "默认维护：按天/按大小切割（copytruncate，写方无需重载），超策略的切割文件已移入回收区（可 logs.clean.restore 恢复）"
	return result, nil
}

// rotateReason 判断是否需要切割：先看大小，再看当天是否已切过。
// 返回 "size" / "daily" / ""（不需要切割）。
func rotateReason(realDir, base string, activeSize int64, policy RetentionPolicy, now time.Time) string {
	limit := policy.RotateSizeMB
	if limit <= 0 {
		limit = DefaultRotateSizeMB
	}
	if activeSize >= int64(limit)<<20 {
		return "size"
	}
	if activeSize == 0 {
		return ""
	}
	if rotatedToday(realDir, base, now) {
		return ""
	}
	return "daily"
}

// rotatedToday 判断当天（UTC）是否已经有切割产物：按文件名里的 UTC 日期前缀匹配，
// 不读文件内容、不解析 retention.json。
func rotatedToday(realDir, base string, now time.Time) bool {
	prefix := base + "." + now.UTC().Format("20060102")
	entries, err := os.ReadDir(realDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix+"T") {
			return true
		}
	}
	return false
}

// copyTruncate 把活跃文件复制为 <base>.<UTC时间戳>（同秒冲突加 -n 后缀），再原地清空活跃文件。
// 复制必须先成功才截断：复制失败时活跃文件保持原样（宁可这次不切，也不丢日志）。
func copyTruncate(realDir, base string, now time.Time) (string, *FaultError) {
	target := filepath.Join(realDir, base)
	dst := rotatedName(realDir, base, now)
	if err := copyFile(target, dst); err != nil {
		return "", methodErr(CodeInternal, "自动切割复制失败: "+err.Error())
	}
	if err := os.Truncate(target, 0); err != nil {
		_ = os.Remove(dst)
		return "", methodErr(CodeInternal, "自动切割清空活跃文件失败: "+err.Error())
	}
	return filepath.Base(dst), nil
}

// rotatedName 生成不冲突的切割文件名（同秒最多 99 个，与手动 rotate 命名完全一致）。
func rotatedName(realDir, base string, now time.Time) string {
	stamp := now.UTC().Format(rotateStampLayout)
	candidate := filepath.Join(realDir, base+"."+stamp)
	for i := 1; i <= 99; i++ {
		if _, err := os.Lstat(candidate); errors.Is(err, iofs.ErrNotExist) {
			return candidate
		}
		candidate = filepath.Join(realDir, base+"."+stamp+"-"+strconv.Itoa(i))
	}
	return candidate
}

// copyFile 复制常规文件内容（不跟随符号链接、不保留所有者，权限按 0640 收紧）。
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("来源不是常规文件")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, maintainFilePerm)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// lstatRegular 读取常规文件元信息（拒绝符号链接与目录）。
func lstatRegular(path string) (fileEntry, *FaultError) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return fileEntry{}, methodErr(CodeNotFound, "来源文件不存在")
		}
		if errors.Is(err, iofs.ErrPermission) {
			return fileEntry{}, methodErr(CodeInternal, "权限不足，无法读取来源")
		}
		return fileEntry{}, methodErr(CodeInternal, "无法读取来源元信息")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fileEntry{}, methodErr(CodeInvalid, "来源不是常规文件，已跳过")
	}
	return fileEntry{name: filepath.Base(path), size: info.Size(), mod: info.ModTime()}, nil
}

// pruneExports 在写导出文件后按"最多保留 exportKeep 份 / 7 天"清理旧导出，
// 这是真正的**写入时判断**：导出目录由本插件自己写，不依赖任何定时器。
func pruneExports(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type item struct {
		name string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	cutoff := time.Now().Add(-exportMaxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, ierr := entry.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
				continue
			}
			continue
		}
		items = append(items, item{name: entry.Name(), mod: info.ModTime()})
	}
	if len(items) <= exportKeep {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	removed := make([]string, 0, len(items)-exportKeep)
	for _, it := range items[exportKeep:] {
		if err := os.Remove(filepath.Join(dir, it.name)); err == nil {
			removed = append(removed, it.name)
		}
	}
	return removed
}

// 导出自清理参数：最多保留 50 份、7 天（导出是"看一次就走"的中间产物，不需要长期留存）。
const (
	exportKeep   = 50
	exportMaxAge = 7 * 24 * time.Hour
)
