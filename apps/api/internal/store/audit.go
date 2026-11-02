package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// 审计写入
//
// 审计（audit_logs）是"谁在什么时候做了什么"的唯一凭据。
// 它写不进去时，调用方必须把失败**暴露出去**——
// 报 200 却没留下记录，等于告诉调用方"记下了"，那是假成功。
//
// 本文件提供三层保障：
//
//  1. 重试：审计写失败多半是瞬时的（连接池打满、SQLite 写锁竞争、数据库抖动）。
//  2. 计数：累计失败次数，供运维在 /api/v1/admin/audit 里一眼看出"审计链上有洞、几个洞"。
//  3. 事务内写入：需要"业务写 + 审计写"原子生效时（创建项目），走 Tx.WriteAudit。

// auditWriteFailures 审计写失败的累计次数。
//
// 为什么用进程内计数而不是指标系统：本仓库没有 metrics 通道，
// 计数挂在管理端审计查询的响应里是最短的可观测路径。
var auditWriteFailures atomic.Int64

// AuditWriteFailures 返回审计写失败的累计次数。
func AuditWriteFailures() int64 { return auditWriteFailures.Load() }

// auditRetryTimes 审计写失败后的重试次数（不含首次）。
//
// 重试一次覆盖了绝大多数瞬时故障（锁竞争、连接池抖动），
// 又不会把一次真故障拖成多次等待。
const auditRetryTimes = 1

// WriteAudit 写一条审计记录。
//
// 失败会重试 auditRetryTimes 次；仍失败则累计失败计数并返回错误。
// ⚠️ 返回的 error **必须**被调用方处理：吞掉它等于让这次操作在审计链上消失。
func (d *DB) WriteAudit(ctx context.Context, actor, action, target, scope, ip string, success bool, detail map[string]any) error {
	raw, err := marshalAuditDetail(detail)
	if err != nil {
		// 明细序列化失败重试也没用——不是瞬时故障。
		auditWriteFailures.Add(1)
		return fmt.Errorf("序列化审计明细 %s: %w", action, err)
	}
	s := 0
	if success {
		s = 1
	}
	// ⚠️ 极性：success 列 **1 = 成功**。
	//    建表的默认值是 DEFAULT 1，ListAudit 把它直接 Scan 进 bool，
	//    两头都按"1 = 成功"读——写的时候必须跟它们一致。
	//    写成反的会让审计页把"登录失败"显示成成功、把成功显示成失败。
	var lastErr error
	for i := 0; i <= auditRetryTimes; i++ {
		_, lastErr = d.Exec(ctx, `INSERT INTO audit_logs
			(ts, actor, action, target, scope, client_ip, success, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, Now(), actor, action, target, scope, ip, s, raw)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			// 上下文已取消（客户端断开 / 超时）——重试没有意义。
			break
		}
	}
	auditWriteFailures.Add(1)
	return fmt.Errorf("写审计记录 %s: %w", action, lastErr)
}

// WriteAudit 在事务里写一条审计记录。
//
// 用在"业务写 + 审计写必须同时生效"的地方：
// 事务回滚时审计也一起没了，不会留下"操作没发生但审计说有"的记录。
//
// ⚠️ 失败计数与 DB.WriteAudit 一致：事务内审计写失败同样是"审计链上的洞"，
//
//	管理端靠这个计数一眼看出少了记录——漏计会让它显示"审计链完整"。
//	重试策略不必统一（事务里重试没有意义：第一次失败后事务多半已 doomed）。
func (t *Tx) WriteAudit(ctx context.Context, actor, action, target, scope, ip string, success bool, detail map[string]any) error {
	raw, err := marshalAuditDetail(detail)
	if err != nil {
		auditWriteFailures.Add(1)
		return fmt.Errorf("序列化审计明细 %s: %w", action, err)
	}
	s := 0
	if success {
		s = 1
	}
	if _, err := t.Exec(ctx, `INSERT INTO audit_logs
		(ts, actor, action, target, scope, client_ip, success, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, Now(), actor, action, target, scope, ip, s, raw); err != nil {
		auditWriteFailures.Add(1)
		return fmt.Errorf("写审计记录 %s: %w", action, err)
	}
	return nil
}

// InsertAndGetID 在事务里插入一行并取回它的自增主键。
//
// 方言处理与 DB.insertAndGetID 一致：postgres / sqlite 走 RETURNING id，
// MySQL 走驱动的 LastInsertId（MySQL 不支持 RETURNING）。
func (t *Tx) InsertAndGetID(ctx context.Context, insert string, args ...any) (int64, error) {
	return insertAndGetID(ctx, t.Dialect, t.QueryRow, t.Exec, insert, args...)
}

// marshalAuditDetail 把审计明细序列化成 JSON 文本。
func marshalAuditDetail(detail map[string]any) (string, error) {
	if len(detail) == 0 {
		return "", nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
