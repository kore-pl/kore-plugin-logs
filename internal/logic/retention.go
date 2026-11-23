package logic

import (
	"encoding/json"
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 保留策略默认值与边界（0 表示该维度不限制；clean/usage 都按同一套判据）。
const (
	DefaultRetentionDays   = 30
	DefaultRetentionSizeMB = 1024
	DefaultRetentionFiles  = 7
	MaxRetentionDays       = 3650
	MaxRetentionSizeMB     = 1048576
	MaxRetentionFiles      = 10000
)

// quarantineQuotaBytes 是插件回收区总大小上限：超限淘汰最旧文件（真正删除，只限回收区内）。
// 声明为变量便于测试注入更小上限，生产默认 64 MiB。
var quarantineQuotaBytes int64 = 64 << 20

// retentionFileName 是保留策略落盘文件名（插件 state 内，形状显式、无 object/any）。
const retentionFileName = "retention.json"

// RetentionPolicy 是单来源保留策略；0 表示该维度不限制。
type RetentionPolicy struct {
	MaxAgeDays int `json:"max_age_days"`
	MaxSizeMB  int `json:"max_size_mb"`
	MaxFiles   int `json:"max_files"`
}

// retentionFile 是 retention.json 的显式形状：来源名 → 策略。
type retentionFile struct {
	Policies map[string]RetentionPolicy `json:"policies"`
}

// RetentionGetRequest / RetentionGetResult 是 logs.retention.get 的入参与结果。
type RetentionGetRequest struct {
	StateDir string `json:"-"`
	Fault    Fault  `json:"-"`
	Debugf   Debugf `json:"-"`
}

type RetentionGetResult struct {
	Defaults RetentionPolicy            `json:"defaults"`
	Policies map[string]RetentionPolicy `json:"policies"`
	Note     string                     `json:"note,omitempty"`
}

// RetentionSetRequest / RetentionSetResult 是 logs.retention.set 的入参与结果。
// 未传的维度保持不变（指针区分「未传」与「显式 0=不限制」）。
type RetentionSetRequest struct {
	Source     string `json:"source"`
	MaxAgeDays *int   `json:"max_age_days"`
	MaxSizeMB  *int   `json:"max_size_mb"`
	MaxFiles   *int   `json:"max_files"`
	StateDir   string `json:"-"`
	Fault      Fault  `json:"-"`
	Debugf     Debugf `json:"-"`
}

type RetentionSetResult struct {
	Source string          `json:"source"`
	Policy RetentionPolicy `json:"policy"`
	Saved  bool            `json:"saved"`
	Note   string          `json:"note,omitempty"`
}

// UsageRequest / UsageEntry / UsageResult 是 logs.usage：各来源占用与是否超策略。
type UsageRequest struct {
	StateDir string `json:"-"`
	Fault    Fault  `json:"-"`
	Debugf   Debugf `json:"-"`
}

type UsageEntry struct {
	Source       string          `json:"source"`
	File         string          `json:"file"`
	Type         SourceType      `json:"type"`
	ActiveBytes  int64           `json:"active_bytes"`
	RotatedBytes int64           `json:"rotated_bytes"`
	TotalBytes   int64           `json:"total_bytes"`
	Files        int             `json:"files"`
	OldestAt     string          `json:"oldest_at,omitempty"`
	Policy       RetentionPolicy `json:"policy"`
	OverPolicy   bool            `json:"over_policy"`
	Reasons      []string        `json:"reasons"`
	Note         string          `json:"note,omitempty"`
}

type UsageResult struct {
	Sources    []UsageEntry `json:"sources"`
	TotalBytes int64        `json:"total_bytes"`
	OverCount  int          `json:"over_count"`
	Note       string       `json:"note,omitempty"`
}

// CleanCandidate 是 dry_run 返回的待清理项（只含登记来源内的旧日志文件，不含当前活跃文件）。
type CleanCandidate struct {
	Source     string `json:"source"`
	File       string `json:"file"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Reason     string `json:"reason"`
}

// CleanRequest 是 logs.clean 的入参：DryRun 默认 true，真实清理必须 Confirm=true。
type CleanRequest struct {
	Source   string `json:"source"`
	DryRun   bool   `json:"dry_run"`
	Confirm  bool   `json:"confirm"`
	StateDir string `json:"-"`
	Fault    Fault  `json:"-"`
	Debugf   Debugf `json:"-"`
}

type CleanResult struct {
	Source          string           `json:"source,omitempty"`
	DryRun          bool             `json:"dry_run"`
	Candidates      []CleanCandidate `json:"candidates"`
	TotalBytes      int64            `json:"total_bytes"`
	Moved           []string         `json:"moved"`
	QuarantineDir   string           `json:"quarantine_dir"`
	QuarantineBytes int64            `json:"quarantine_bytes"`
	Evicted         []string         `json:"evicted"`
	Note            string           `json:"note,omitempty"`
}

// RestoreRequest / RestoreResult 是 logs.clean.restore：把回收区文件移回原目录。
type RestoreRequest struct {
	Source   string `json:"source"`
	StateDir string `json:"-"`
	Fault    Fault  `json:"-"`
	Debugf   Debugf `json:"-"`
}

type RestoreResult struct {
	Restored []string `json:"restored"`
	Skipped  []string `json:"skipped"`
	Note     string   `json:"note,omitempty"`
}

// fileEntry 是来源目录内一个常规文件（活跃文件或轮转文件）的元信息。
type fileEntry struct {
	name string
	size int64
	mod  time.Time
}

// RetentionGet 返回默认策略与已保存的按来源策略。
func (r *Registry) RetentionGet(req RetentionGetRequest) (*RetentionGetResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	policies, ferr := loadRetention(req.StateDir)
	if ferr != nil {
		return nil, ferr
	}
	result := &RetentionGetResult{
		Defaults: RetentionPolicy{MaxAgeDays: DefaultRetentionDays, MaxSizeMB: DefaultRetentionSizeMB, MaxFiles: DefaultRetentionFiles},
		Policies: policies,
	}
	if len(policies) == 0 {
		result.Note = "尚未按来源自定义策略，全部来源使用默认值"
	}
	return result, nil
}

// RetentionSet 保存单来源策略：未传维度保持原值（新来源从默认值起），先校验后原子落盘。
func (r *Registry) RetentionSet(req RetentionSetRequest) (*RetentionSetResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	entry, perr := r.lookup(req.Source)
	if perr != nil {
		return nil, perr
	}
	stateDir := strings.TrimSpace(req.StateDir)
	if stateDir == "" {
		return nil, methodErr(CodeInvalid, "KORE_STATE_DIR 未设置，无法保存保留策略")
	}
	policies, ferr := loadRetention(stateDir)
	if ferr != nil {
		return nil, ferr
	}
	policy, ok := policies[entry.spec.Name]
	if !ok {
		policy = RetentionPolicy{MaxAgeDays: DefaultRetentionDays, MaxSizeMB: DefaultRetentionSizeMB, MaxFiles: DefaultRetentionFiles}
	}
	if req.MaxAgeDays != nil {
		if *req.MaxAgeDays < 0 || *req.MaxAgeDays > MaxRetentionDays {
			return nil, methodErr(CodeInvalid, "max_age_days 必须在 0 到 3650 之间（0=不限制）")
		}
		policy.MaxAgeDays = *req.MaxAgeDays
	}
	if req.MaxSizeMB != nil {
		if *req.MaxSizeMB < 0 || *req.MaxSizeMB > MaxRetentionSizeMB {
			return nil, methodErr(CodeInvalid, "max_size_mb 必须在 0 到 1048576 之间（0=不限制）")
		}
		policy.MaxSizeMB = *req.MaxSizeMB
	}
	if req.MaxFiles != nil {
		if *req.MaxFiles < 0 || *req.MaxFiles > MaxRetentionFiles {
			return nil, methodErr(CodeInvalid, "max_files 必须在 0 到 10000 之间（0=不限制）")
		}
		policy.MaxFiles = *req.MaxFiles
	}
	policies[entry.spec.Name] = policy
	if err := saveRetention(stateDir, policies); err != nil {
		return nil, methodErr(CodeInternal, "保存保留策略失败: "+err.Error())
	}
	debugf(req.Debugf, "logs.retention.set source=%s policy=%+v", req.Source, policy)
	return &RetentionSetResult{Source: entry.spec.Name, Policy: policy, Saved: true}, nil
}

// Usage 汇总各来源占用（活跃文件 + 轮转文件）并按策略给出是否失控。
func (r *Registry) Usage(req UsageRequest) (*UsageResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	policies, ferr := loadRetention(req.StateDir)
	if ferr != nil {
		return nil, ferr
	}
	result := &UsageResult{Sources: make([]UsageEntry, 0, len(r.order))}
	for _, name := range r.order {
		entry := r.entries[name]
		usage := UsageEntry{Source: name, Type: entry.spec.Type, Policy: policyFor(policies, name)}
		usage.Reasons = make([]string, 0, 3)
		if entry.spec.Type == SourceTypeJournal {
			usage.Note = "journal 不占插件可清理的日志文件"
			result.Sources = append(result.Sources, usage)
			continue
		}
		realDir, base, derr := r.secureSourceDir(entry)
		if derr != nil {
			usage.Note = derr.Message
			result.Sources = append(result.Sources, usage)
			continue
		}
		all, perr := listSourceFiles(realDir, base)
		if perr != nil {
			usage.Note = perr.Message
			result.Sources = append(result.Sources, usage)
			continue
		}
		var oldest time.Time
		for _, f := range all {
			if f.name == base {
				usage.ActiveBytes = f.size
			} else {
				usage.RotatedBytes += f.size
				usage.Files++
			}
			if oldest.IsZero() || f.mod.Before(oldest) {
				oldest = f.mod
			}
		}
		usage.TotalBytes = usage.ActiveBytes + usage.RotatedBytes
		if !oldest.IsZero() {
			usage.OldestAt = oldest.UTC().Format(time.RFC3339)
		}
		recordUsageReasons(&usage, oldest, time.Now())
		if usage.OverPolicy {
			result.OverCount++
		}
		result.TotalBytes += usage.TotalBytes
		result.Sources = append(result.Sources, usage)
	}
	if result.OverCount > 0 {
		result.Note = "存在超出保留策略的来源，可先 logs.clean 的 dry_run 查看待清理清单"
	}
	return result, nil
}

// Clean 按策略清理旧日志：默认 dry_run（只列清单不动文件）；dry_run=false 必须 confirm=true。
// 删除不是 rm，而是移动到 <state>/quarantine/<source>/，可 clean.restore 恢复；
// 当前活跃文件永不清除（正在被 follow 的正是活跃文件，因此跟随不受影响），只处理同目录的轮转文件。
func (r *Registry) Clean(req CleanRequest) (*CleanResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	stateDir := strings.TrimSpace(req.StateDir)
	if stateDir == "" {
		return nil, methodErr(CodeInvalid, "KORE_STATE_DIR 未设置，无法清理日志")
	}
	if !req.DryRun && !req.Confirm {
		return nil, methodErr(CodeInvalid, "真实清理需要 confirm=true（建议先 dry_run 查看清单）")
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
			return nil, methodErr(CodeUnsupported, "journal 来源没有可清理的日志文件")
		}
		targets = append(targets, entry.spec.Name)
	} else {
		for _, name := range r.order {
			if r.entries[name].spec.Type == SourceTypeFile {
				targets = append(targets, name)
			}
		}
	}
	result := &CleanResult{
		DryRun: req.DryRun, Candidates: make([]CleanCandidate, 0), Moved: make([]string, 0),
		Evicted: make([]string, 0), QuarantineDir: filepath.Join(stateDir, "quarantine"),
	}
	now := time.Now()
	for _, name := range targets {
		entry := r.entries[name]
		realDir, base, derr := r.secureSourceDir(entry)
		if derr != nil {
			return nil, derr
		}
		files, perr := listSourceFiles(realDir, base)
		if perr != nil {
			return nil, perr
		}
		candidates := planClean(files, base, policyFor(policies, name), now)
		for _, cand := range candidates {
			result.Candidates = append(result.Candidates, cand)
			result.TotalBytes += cand.SizeBytes
		}
		if req.DryRun || len(candidates) == 0 {
			continue
		}
		moved, merr := moveToQuarantine(stateDir, name, realDir, candidates)
		if merr != nil {
			return nil, merr
		}
		result.Moved = append(result.Moved, moved...)
	}
	if req.DryRun {
		result.Note = "dry_run=true 只列出待清理文件，未移动任何文件；确认后传 dry_run=false + confirm=true"
		return result, nil
	}
	evicted, qbytes, qerr := enforceQuarantineQuota(stateDir)
	if qerr != nil {
		return nil, qerr
	}
	result.Evicted = evicted
	result.QuarantineBytes = qbytes
	result.Note = "已将待清理文件移入回收区（可 logs.clean.restore 恢复）；活跃文件与白名单外路径未触碰"
	return result, nil
}

// Restore 把回收区文件移回登记来源目录；目标已存在则跳过，绝不覆盖。
func (r *Registry) Restore(req RestoreRequest) (*RestoreResult, *FaultError) {
	if ferr := checkFault(req.Fault); ferr != nil {
		return nil, ferr
	}
	stateDir := strings.TrimSpace(req.StateDir)
	if stateDir == "" {
		return nil, methodErr(CodeInvalid, "KORE_STATE_DIR 未设置，无法恢复")
	}
	names := make([]string, 0, len(r.order))
	if req.Source != "" {
		entry, perr := r.lookup(req.Source)
		if perr != nil {
			return nil, perr
		}
		if entry.spec.Type != SourceTypeFile {
			return nil, methodErr(CodeUnsupported, "journal 来源没有可恢复的日志文件")
		}
		names = append(names, entry.spec.Name)
	} else {
		for _, name := range r.order {
			if r.entries[name].spec.Type == SourceTypeFile {
				names = append(names, name)
			}
		}
	}
	result := &RestoreResult{Restored: make([]string, 0), Skipped: make([]string, 0)}
	for _, name := range names {
		entry := r.entries[name]
		realDir, _, derr := r.secureSourceDir(entry)
		if derr != nil {
			return nil, derr
		}
		qdir := filepath.Join(stateDir, "quarantine", name)
		entries, err := os.ReadDir(qdir)
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) {
				continue
			}
			return nil, methodErr(CodeInternal, "读取回收区失败")
		}
		for _, qe := range entries {
			if qe.IsDir() {
				continue
			}
			src := filepath.Join(qdir, qe.Name())
			dst := filepath.Join(realDir, qe.Name())
			if _, err := os.Lstat(dst); err == nil {
				result.Skipped = append(result.Skipped, qe.Name())
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				return nil, methodErr(CodeInternal, "恢复 "+qe.Name()+" 失败")
			}
			result.Restored = append(result.Restored, qe.Name())
		}
		_ = os.Remove(qdir)
	}
	if len(result.Skipped) > 0 {
		result.Note = "同名目标已存在，已跳过（不覆盖）"
	}
	return result, nil
}

// secureSourceDir 复用 rotate 同款安全口径，返回来源的真实目录与活跃文件名。
func (r *Registry) secureSourceDir(entry *sourceEntry) (string, string, *FaultError) {
	fs := entry.file
	if fs == nil {
		return "", "", methodErr(CodeInvalid, "来源类型不是文件")
	}
	if fs.loadErr != nil {
		return "", "", fs.loadErr
	}
	realDir, err := filepath.EvalSymlinks(fs.dir)
	if err != nil {
		return "", "", methodErr(CodeNotFound, "来源目录不可访问")
	}
	if realDir != fs.realDir {
		return "", "", methodErr(CodeInvalid, "来源目录经符号链接解析后与配置不一致，已拒绝")
	}
	return realDir, fs.base, nil
}

// listSourceFiles 列出真实目录内属于该来源的常规文件：活跃文件 + 以 "<base>." 开头的轮转文件；
// 目录、符号链接一律跳过（纵深防御：白名单路径之外的文件不可能成为候选）。
func listSourceFiles(realDir, base string) ([]fileEntry, *FaultError) {
	entries, err := os.ReadDir(realDir)
	if err != nil {
		return nil, methodErr(CodeInternal, "读取来源目录失败")
	}
	files := make([]fileEntry, 0, len(entries))
	for _, de := range entries {
		name := de.Name()
		if name != base && !strings.HasPrefix(name, base+".") {
			continue
		}
		if de.IsDir() {
			continue
		}
		path := filepath.Join(realDir, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, fileEntry{name: name, size: info.Size(), mod: info.ModTime()})
	}
	return files, nil
}

// planClean 计算待清理的轮转文件：超过保留份数 / 超过保留天数 / 超过保留体积三个维度取并集。
// 活跃文件（name == base）永不入选。
func planClean(files []fileEntry, base string, policy RetentionPolicy, now time.Time) []CleanCandidate {
	rotated := make([]fileEntry, 0, len(files))
	for _, f := range files {
		if f.name != base {
			rotated = append(rotated, f)
		}
	}
	sort.Slice(rotated, func(i, j int) bool {
		if rotated[i].mod.Equal(rotated[j].mod) {
			return rotated[i].name > rotated[j].name
		}
		return rotated[i].mod.After(rotated[j].mod)
	})
	candidates := make([]CleanCandidate, 0)
	cutoff := time.Time{}
	if policy.MaxAgeDays > 0 {
		cutoff = now.Add(-time.Duration(policy.MaxAgeDays) * 24 * time.Hour)
	}
	var keptBytes int64
	for i, f := range rotated {
		reasons := make([]string, 0, 3)
		if policy.MaxFiles > 0 && i >= policy.MaxFiles {
			reasons = append(reasons, "超过保留份数")
		}
		if !cutoff.IsZero() && f.mod.Before(cutoff) {
			reasons = append(reasons, "超过保留天数")
		}
		if policy.MaxSizeMB > 0 && keptBytes+f.size > int64(policy.MaxSizeMB)<<20 {
			reasons = append(reasons, "超过体积上限")
		}
		if len(reasons) > 0 {
			candidates = append(candidates, CleanCandidate{
				File: f.name, SizeBytes: f.size, ModifiedAt: f.mod.UTC().Format(time.RFC3339),
				Reason: strings.Join(reasons, "、"),
			})
			continue
		}
		keptBytes += f.size
	}
	return candidates
}

// moveToQuarantine 把候选文件移动到 <state>/quarantine/<source>/；同秒重名追加时间戳后缀，
// 绝不覆盖已有回收文件，也绝不写回收区之外的路径。
func moveToQuarantine(stateDir, source, realDir string, candidates []CleanCandidate) ([]string, *FaultError) {
	qdir := filepath.Join(stateDir, "quarantine", source)
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return nil, methodErr(CodeInternal, "创建回收区失败: "+err.Error())
	}
	moved := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		src := filepath.Join(realDir, cand.File)
		if filepath.Dir(src) != realDir {
			return moved, methodErr(CodeInvalid, "清理目标越出来源目录，已拒绝")
		}
		dst := filepath.Join(qdir, cand.File)
		if _, err := os.Lstat(dst); err == nil {
			dst = filepath.Join(qdir, cand.File+"."+time.Now().UTC().Format("20060102T150405Z"))
		}
		if err := os.Rename(src, dst); err != nil {
			return moved, methodErr(CodeInternal, "移动待清理文件失败: "+cand.File)
		}
		moved = append(moved, cand.File)
	}
	return moved, nil
}

// enforceQuarantineQuota 统计回收区总大小，超限淘汰最旧文件（真正删除），返回淘汰清单与剩余字节。
func enforceQuarantineQuota(stateDir string) ([]string, int64, *FaultError) {
	root := filepath.Join(stateDir, "quarantine")
	type qfile struct {
		path string
		size int64
		mod  time.Time
	}
	all := make([]qfile, 0)
	_ = filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil
		}
		all = append(all, qfile{path: path, size: info.Size(), mod: info.ModTime()})
		return nil
	})
	var total int64
	for _, f := range all {
		total += f.size
	}
	evicted := make([]string, 0)
	if total <= quarantineQuotaBytes {
		return evicted, total, nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, f := range all {
		if total <= quarantineQuotaBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			return evicted, total, methodErr(CodeInternal, "淘汰回收区旧文件失败")
		}
		total -= f.size
		evicted = append(evicted, filepath.Base(f.path))
	}
	return evicted, total, nil
}

// policyFor 取来源策略：已保存的优先，否则默认值。
func policyFor(policies map[string]RetentionPolicy, source string) RetentionPolicy {
	if p, ok := policies[source]; ok {
		return p
	}
	return RetentionPolicy{MaxAgeDays: DefaultRetentionDays, MaxSizeMB: DefaultRetentionSizeMB, MaxFiles: DefaultRetentionFiles}
}

// recordUsageReasons 按策略判读是否失控：体积超限 / 轮转份数超限 / 最旧文件超龄。
func recordUsageReasons(usage *UsageEntry, oldest time.Time, now time.Time) {
	p := usage.Policy
	if p.MaxSizeMB > 0 && usage.TotalBytes > int64(p.MaxSizeMB)<<20 {
		usage.Reasons = append(usage.Reasons, "总占用超过体积上限")
	}
	if p.MaxFiles > 0 && usage.Files > p.MaxFiles {
		usage.Reasons = append(usage.Reasons, "轮转文件数超过保留份数")
	}
	if p.MaxAgeDays > 0 && !oldest.IsZero() && oldest.Before(now.Add(-time.Duration(p.MaxAgeDays)*24*time.Hour)) {
		usage.Reasons = append(usage.Reasons, "最旧文件超过保留天数")
	}
	usage.OverPolicy = len(usage.Reasons) > 0
}

// loadRetention 读取 retention.json；文件不存在视为空策略。
func loadRetention(stateDir string) (map[string]RetentionPolicy, *FaultError) {
	policies := map[string]RetentionPolicy{}
	if strings.TrimSpace(stateDir) == "" {
		return policies, nil
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, retentionFileName))
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return policies, nil
		}
		return nil, methodErr(CodeInternal, "读取保留策略失败: "+err.Error())
	}
	var file retentionFile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, methodErr(CodeInternal, "保留策略文件损坏: "+err.Error())
	}
	if file.Policies != nil {
		policies = file.Policies
	}
	return policies, nil
}

// saveRetention 0600 原子写 retention.json（临时文件 + rename）。
func saveRetention(stateDir string, policies map[string]RetentionPolicy) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(retentionFile{Policies: policies}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, retentionFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
