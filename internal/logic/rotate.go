package logic

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// rotateStampLayout 是切割文件名的 UTC 时间戳格式：<base>.<20261123T131500Z>。
// 该格式按字典序即时间序，保留策略无需解析时间。
const rotateStampLayout = "20060102T150405Z"

// Rotate 实现 logs.rotate：把一条已登记文件来源原子重命名（rename）为同目录下的
// <base>.<UTC时间戳>，再按保留份数 Kept 删除更早的切割文件。
//
// 安全边界（破坏性操作，缺一不可）：
//   - confirm 必须显式为 true；
//   - source 必须是 KORE_LOGS_SOURCES / 配置文件里登记的文件来源，绝不接受路径；
//   - 目标文件必须位于登记目录内（装载时固定的真实目录）、Lstat 非符号链接、常规文件；
//   - 只删除「同目录 + 文件名匹配 <base>.<时间戳>(-n)」的常规文件，其他文件一律不碰；
//   - 删除前逐条校验父目录仍是装载时的真实目录，且目标不是符号链接。
//
// 注意：rename 只移动目录项，正在写该文件的进程仍持有旧 inode 继续写切割文件；
// 需要进程重开日志（重载/重启）后才会写回原路径，结果 Note 里如实说明。
func (r *Registry) Rotate(req RotateRequest) (*RotateResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	if !req.Confirm {
		return nil, methodErr(CodeInvalid, "确认切割需要显式传 confirm=true（破坏性操作）")
	}
	keep, perr := effectiveRotateKeep(req.Keep)
	if perr != nil {
		return nil, perr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	if entry.spec.Type == SourceTypeJournal {
		return nil, methodErr(CodeUnsupported, "journal 来源没有可切割的日志文件")
	}
	target, info, ferr := r.resolveRotateTarget(entry)
	if ferr != nil {
		return nil, ferr
	}
	rotated, rerr := rotateRename(target, info)
	if rerr != nil {
		return nil, rerr
	}
	debugf(req.Debugf, "logs.rotate source=%s path=%s keep=%d", entry.spec.Name, target, keep)

	removed, ferr := enforceRotateRetention(entry.file.realDir, entry.file.base, keep)
	if ferr != nil {
		return nil, ferr
	}
	result := &RotateResult{
		Source: entry.spec.Name, File: entry.file.base, RotatedTo: rotated,
		SizeBytes: info.Size(), Kept: keep, Retention: keep, Removed: removed,
		Note: "切割为原子 rename：正在写该文件的进程仍写旧 inode，请让对应服务重载/重开日志后再写新文件",
	}
	return result, nil
}

// effectiveRotateKeep 校验保留份数：0 取默认 5，合法区间 1..100。
func effectiveRotateKeep(keep int) (int, *FaultError) {
	if keep == 0 {
		return DefaultRotateKeep, nil
	}
	if keep < 1 || keep > MaxRotateKeep {
		return 0, methodErr(CodeInvalid, fmt.Sprintf("keep 必须在 1 到 %d 之间", MaxRotateKeep))
	}
	return keep, nil
}

// resolveRotateTarget 按登记来源解析可切割目标：父目录真实路径必须与装载时一致、
// 目标必须存在且是常规文件（非符号链接），打开后再用 os.SameFile 复核 inode。
func (r *Registry) resolveRotateTarget(entry *sourceEntry) (string, os.FileInfo, *FaultError) {
	fs := entry.file
	if fs == nil {
		return "", nil, methodErr(CodeInvalid, "来源类型不是文件")
	}
	if fs.loadErr != nil {
		return "", nil, fs.loadErr
	}
	realDir, err := filepath.EvalSymlinks(fs.dir)
	if err != nil {
		return "", nil, methodErr(CodeNotFound, "来源目录不可访问")
	}
	if realDir != fs.realDir {
		return "", nil, methodErr(CodeInvalid, "来源目录经符号链接解析后与配置不一致，已拒绝切割")
	}
	target := filepath.Join(realDir, fs.base)
	if filepath.Dir(target) != realDir {
		return "", nil, methodErr(CodeInvalid, "切割目标越出来源目录，已拒绝")
	}
	before, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return "", nil, methodErr(CodeNotFound, "来源文件不存在")
		}
		if errors.Is(err, iofs.ErrPermission) {
			return "", nil, methodErr(CodeInternal, "权限不足，无法读取来源")
		}
		return "", nil, methodErr(CodeInternal, "无法读取来源元信息")
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return "", nil, methodErr(CodeInvalid, "来源文件是符号链接，与配置路径不一致，已拒绝切割")
	}
	if !before.Mode().IsRegular() {
		return "", nil, methodErr(CodeInvalid, "来源不是常规文件")
	}
	file, err := os.Open(target)
	if err != nil {
		return "", nil, methodErr(CodeInternal, "权限不足，无法读取来源")
	}
	opened, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil || !os.SameFile(before, opened) {
		return "", nil, methodErr(CodeInvalid, "来源文件在切割前被替换，已拒绝")
	}
	return target, before, nil
}

// rotateRename 把 target 重命名为 <base>.<UTC时间戳>；同秒冲突时追加 -1..-99 后缀。
func rotateRename(target string, info os.FileInfo) (string, *FaultError) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	stamp := time.Now().UTC().Format(rotateStampLayout)
	candidate := filepath.Join(dir, base+"."+stamp)
	for i := 1; i <= 99; i++ {
		if _, err := os.Lstat(candidate); errors.Is(err, iofs.ErrNotExist) {
			if err := os.Rename(target, candidate); err != nil {
				return "", methodErr(CodeInternal, "切割重命名失败: "+err.Error())
			}
			return candidate, nil
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s.%s-%d", base, stamp, i))
	}
	return "", methodErr(CodeInternal, "同一秒内切割冲突过多（>99），已拒绝")
}

// enforceRotateRetention 按保留份数删除更早的切割文件：只匹配 <base>.<时间戳>(-n) 的
// 常规文件（不含符号链接），按名字倒序（即时间倒序）保留最新 keep 份，其余逐个删除。
// 返回被删除的文件名（不含目录）。
func enforceRotateRetention(realDir, base string, keep int) ([]string, *FaultError) {
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(base) + `\.[0-9]{8}T[0-9]{6}Z(-[0-9]{1,2})?$`)
	entries, err := os.ReadDir(realDir)
	if err != nil {
		return nil, methodErr(CodeInternal, "读取来源目录失败")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !pattern.MatchString(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	removed := make([]string, 0)
	for i := keep; i < len(names); i++ {
		name := names[i]
		path := filepath.Join(realDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if filepath.Dir(path) != realDir {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, methodErr(CodeInternal, "删除旧切割文件失败: "+name)
		}
		removed = append(removed, name)
	}
	return removed, nil
}
