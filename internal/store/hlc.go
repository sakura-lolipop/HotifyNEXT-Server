// HLC（Hybrid Logical Clock）纯函数：BBolt 和 Memory 共享同一份，测试覆盖统一。
// 从 store.go 拆出（TD-1，自洽单元零依赖，零风险平移）。
//
// 位布局：pt 高 48 位（ns，~287 年到 ~2257）+ counter 低 16 位（0..65535）。
// 否决 counter 低 2 位（NTP 回退几秒就溢出破坏单调）；16 位=65536/秒容错，单用户永不到。
// big-endian 编码成 8 字节后，字节字典序 == 数值序 → bbolt Cursor.First() 取最小（最老）。
package store

import "log"

const (
	hlcPtMask  uint64 = (1 << 48) - 1 // pt 低 48 位掩码（截断防溢出到 counter 区）
	hlcCtrMask uint64 = (1 << 16) - 1 // counter 低 16 位掩码
	hlcCtrMax  uint64 = 0xFFFF
)

// nextHLC 生成下一个 HLC 的 (pt, counter)（docs/NEXT-Server.md §7 算法）。
// 调用方须持锁（BBolt 在 db.Update 事务内 + hlcState.mu；Memory 在 mu）保证 lastPt/lastCtr 读改写互斥。
// nowNs 传参（不内部 time.Now）便于测试注入固定时钟验单调。
func nextHLC(lastPt, lastCtr, nowNs uint64) (newPt, newCtr uint64) {
	nowNs &= hlcPtMask // 48 位截断
	if nowNs > lastPt {
		return nowNs, 0 // 时钟前进：用新 pt，counter 归零
	}
	if lastCtr >= hlcCtrMax {
		// counter 溢出（理论 65536/秒 不触发）：pt 前进 1 让出 counter 空间 + log warn 留痕。
		// 注：此后 HLC.pt 偏离物理时钟（虚假前进），但展示用 Message.TS（物理 ns）不用 HLC.pt → 展示不失真；
		// 补漏靠 HLC 单调（pt+1 仍 > 旧 HLC）不受影响。
		log.Printf("[WARN] hlc counter overflow at pt=%d, forcing pt+1 (single-user load should never hit this)", lastPt)
		return lastPt + 1, 0
	}
	return lastPt, lastCtr + 1 // 停滞/回退：pt 不动，counter+1（保单调）
}

// packHLC 把 (pt, counter) 编码成单 uint64（pt 占高 48 位，counter 低 16 位）。
func packHLC(pt, ctr uint64) uint64 { return (pt << 16) | (ctr & hlcCtrMask) }

// unpackHLC 反解。
func unpackHLC(hlc uint64) (pt, ctr uint64) { return hlc >> 16, hlc & hlcCtrMask }
