package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 配置备份与回滚。
//
// 契约见 docs/design/14-settings.md §5.3：
//
//	每次写入前，把旧文件复制到 /data/config/backups/application.yaml.<时间戳>
//	保留最近 20 份，超出按时间淘汰
//	回滚 = 把备份文件复制回原位 + 重启容器（内存快照与文件已经不一致）
//
// ⚠️ **备份的是 YAML 文件，不是 DB**。
// DB（system_settings）才是运行期的真理源——
// 回滚 YAML 只是把"下次启动时的基线"换回去，
// 不改 DB 就没有真正回滚。两者都改才是完整回滚，
// 但 DB 的键值是逐条管理的，粒度与文件备份不同；
// 所以本实现只回滚文件，并在响应里明确说明
// "运行期仍以 DB 为准，需重启才生效"。

// backupKeep 是保留的备份份数。
const backupKeep = 20

// backupTimeFormat 是备份文件名里的时间戳格式。
//
// 用 UTC + 秒级精度：文件名按字典序排就是时间序，
// 列表与淘汰都不用再解析时间。
const backupTimeFormat = "2006-01-02T15-04-05Z"

// ConfigBackup 是一份配置备份。
type ConfigBackup struct {
	// Name 是备份的标识（时间戳部分），回滚时用它指定目标。
	Name string `json:"name"`
	// File 是备份文件的完整路径。
	File string `json:"file"`
	// Size 是文件字节数。
	Size int64 `json:"size"`
	// CreatedAt 是备份时间（unix 秒）。
	CreatedAt int64 `json:"created_at"`
}

// backupsDir 返回备份目录。
//
// 它是配置文件所在目录下的 backups/ ——
// 与 application.yaml 同级，但独立目录，
// 免得备份文件被当成配置文件读进去。
func (m *SettingsManager) backupsDir() string {
	if m.yamlPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.yamlPath), "backups")
}

// backupPrefix 是备份文件名前缀（application.yaml.）。
func (m *SettingsManager) backupPrefix() string {
	if m.yamlPath == "" {
		return ""
	}
	return filepath.Base(m.yamlPath) + "."
}

// backupName 拼出某个时间点对应的备份文件名。
func (m *SettingsManager) backupName(t time.Time) string {
	return m.backupPrefix() + t.UTC().Format(backupTimeFormat)
}

// backupNow 把当前配置文件复制一份到备份目录。
//
// ⚠️ **只在真的要写文件之前调用**：
// 它备份的是"即将被覆盖的那份"。
//
// 失败必须返回错误：备份失败就意味着这次写入会让旧配置**无法找回**，
// 那就不能继续写（AGENTS.md 规则 1）。
func (m *SettingsManager) backupNow() error {
	if m.yamlPath == "" {
		return nil
	}
	old, err := os.ReadFile(m.yamlPath)
	if err != nil {
		// 文件还不存在（首次导出）——没有旧版本可备份，不算失败。
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取待备份的配置 %s: %w", m.yamlPath, err)
	}
	dir := m.backupsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建备份目录 %s: %w", dir, err)
	}
	name := m.backupName(time.Now())
	dst := filepath.Join(dir, name)
	// O_EXCL：同名文件已存在说明同一秒内写了两次，
	// 覆盖会丢掉前一份——加后缀另存。
	if _, err := os.Stat(dst); err == nil {
		for i := 1; i < 100; i++ {
			dst = filepath.Join(dir, fmt.Sprintf("%s-%d", name, i))
			if _, err := os.Stat(dst); os.IsNotExist(err) {
				break
			}
		}
	}
	// 0600：配置文件里可能含 DSN 与口令。
	if err := os.WriteFile(dst, old, 0o600); err != nil {
		return fmt.Errorf("写备份 %s: %w", dst, err)
	}
	m.pruneBackups()
	return nil
}

// pruneBackups 淘汰超出保留份数的旧备份。
//
// 按文件名倒序（= 时间倒序）保留最新的 backupKeep 份，其余删除。
// 删除失败只记日志——备份目录里多几份旧文件不影响正确性。
func (m *SettingsManager) pruneBackups() {
	dir := m.backupsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := m.backupPrefix()
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= backupKeep {
		return
	}
	// 文件名里的时间戳定长且可字典序比较 → 倒序即最新在前。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names[backupKeep:] {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil && m.logger != nil {
			m.logger.Warn("淘汰旧配置备份失败", "file", p, "err", err)
		}
	}
}

// ListBackups 列出可用备份（最新在前）。
func (m *SettingsManager) ListBackups() ([]ConfigBackup, error) {
	dir := m.backupsDir()
	if dir == "" {
		return []ConfigBackup{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ConfigBackup{}, nil
		}
		return nil, fmt.Errorf("读取备份目录 %s: %w", dir, err)
	}
	prefix := m.backupPrefix()
	out := []ConfigBackup{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := strings.TrimPrefix(e.Name(), prefix)
		out = append(out, ConfigBackup{
			Name:      name,
			File:      filepath.Join(dir, e.Name()),
			Size:      info.Size(),
			CreatedAt: info.ModTime().Unix(),
		})
	}
	// 最新的在前。
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Rollback 把指定备份复制回配置文件原位。
//
// ⚠️ **只改文件，不改运行期**：
// 内存快照与 DB 都不动，所以当前进程里的行为不变——
// 要真正生效必须重启容器。返回值里带上这个说明，
// 调用方必须把它讲给用户听，不能让人以为"回滚完了就生效了"。
//
// ⚠️ 覆盖前先备份当前文件：回滚本身也是一次写入，
// 手滑回滚错了还能再滚回去。
func (m *SettingsManager) Rollback(ctx context.Context, name string) (string, error) {
	if m.yamlPath == "" {
		return "", fmt.Errorf("未配置配置文件路径，无法回滚")
	}
	if name == "" {
		return "", fmt.Errorf("必须指定要回滚到的备份")
	}
	// ⚠️ 名字里不能带路径分隔符——它会被拼进备份目录的路径里。
	//    不拦的话 "../application.yaml" 就能把任意文件读出来。
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("备份名不合法: %q", name)
	}
	dir := m.backupsDir()
	src := filepath.Join(dir, m.backupPrefix()+name)
	// 兜底：也允许直接给完整文件名（列表接口返回的就是它）。
	if _, err := os.Stat(src); os.IsNotExist(err) {
		if _, err2 := os.Stat(filepath.Join(dir, name)); err2 == nil {
			src = filepath.Join(dir, name)
		} else {
			return "", fmt.Errorf("备份不存在: %s", name)
		}
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("读取备份 %s: %w", src, err)
	}
	// 先备份当前文件（回滚错了还能滚回去）。
	if err := m.backupNow(); err != nil {
		return "", fmt.Errorf("回滚前备份当前配置失败: %w", err)
	}
	// 原子写：tmp + rename（**不先删旧文件**——见 internal/fsatomic）。
	if err := fsatomic.Write(m.yamlPath, data, 0o600); err != nil {
		return "", fmt.Errorf("替换配置文件 %s: %w", m.yamlPath, err)
	}
	return m.yamlPath, nil
}

// Export 强制把当前生效配置重新导出到 application.yaml。
//
// 用于"YAML 导出失败"之后的补救：DB 与内存快照是对的，
// 只是文件是旧的——强制重写一遍让三者一致。
func (m *SettingsManager) Export() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.yamlPath == "" {
		return fmt.Errorf("未配置配置文件路径，无法导出")
	}
	// ⚠️ 备份由 exportYAMLDoc 内部完成（写之前必备份），
	//    这里不要再单独调一次——否则会多出一份没意义的重复备份。
	if err := m.exportYAML(m.Snapshot()); err != nil {
		m.exportStale = true
		return fmt.Errorf("导出配置失败: %w", err)
	}
	m.exportStale = false
	m.exportedAt = store.Now()
	return nil
}
