package partition

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strconv"
)

// AEROStatePayload 定义传给 Python 的数据结构
type AEROStatePayload struct {
	Epoch             int          `json:"epoch"`
	ShardLoads        []int        `json:"shard_loads"`        // 分片负载
	ShardCSTXRatios   []float64    `json:"shard_cstx_ratios"`  // 分片跨分片交易比例
	CandidatePrefixes []PrefixStat `json:"candidate_prefixes"` // 候选迁移前缀
}

// PrefixStat 对应论文中的前缀组统计
type PrefixStat struct {
	Prefix       string `json:"prefix"`
	CurrentShard int    `json:"current_shard"`
	TXVolume     int    `json:"tx_volume"`
	CSTXVolume   int    `json:"cstx_volume"`
}

// AEROActionResponse 定义 Python 返回的动作
type AEROActionResponse struct {
	Migrations []struct {
		Prefix  string `json:"prefix"`
		ToShard int    `json:"to_shard"`
	} `json:"migrations"`
}

// AERO_Partition 是 AERO 算法的主入口
func (cs *CLPAState) AERO_Partition(epoch int) map[string]uint64 {
	fmt.Printf(">>> [AERO] Epoch %d: 开始状态收集...\n", epoch)

	// 1. 更新基础统计数据
	cs.ComputeEdges2Shard()

	payload := AEROStatePayload{
		Epoch:             epoch,
		ShardLoads:        make([]int, cs.ShardNum),
		ShardCSTXRatios:   make([]float64, cs.ShardNum),
		CandidatePrefixes: make([]PrefixStat, 0),
	}

	// 1.1 填充负载 (使用节点数近似)
	copy(payload.ShardLoads, cs.VertexsNumInShard)

	// 1.2 填充跨分片比例 (简单估算)
	for i := 0; i < cs.ShardNum; i++ {
		total := cs.Edges2Shard[i] + 1 // 防止除以0
		payload.ShardCSTXRatios[i] = float64(cs.Edges2Shard[i]) / float64(total)
	}

	// 1.3 核心：按前缀聚合统计
	// 假设前缀长度为2 (对应以太坊地址前几位，如 "0x1a")
	prefixMap := make(map[string]*PrefixStat)
	prefixLen := 2

	for v, neighbors := range cs.NetGraph.EdgeSet {
		// 确保地址长度足够
		if len(v.Addr) < prefixLen {
			continue
		}
		p := v.Addr[:prefixLen]

		if _, ok := prefixMap[p]; !ok {
			prefixMap[p] = &PrefixStat{
				Prefix:       p,
				CurrentShard: cs.PartitionMap[v],
				TXVolume:     0,
				CSTXVolume:   0,
			}
		}
		stat := prefixMap[p]

		// 统计交易量和跨分片交易
		for _, u := range neighbors {
			stat.TXVolume++
			if cs.PartitionMap[v] != cs.PartitionMap[u] {
				stat.CSTXVolume++
			}
		}
	}

	// =========================================================
	// ✨✨✨ 新增：在这里插入“阈值拦截闸门”代码 ✨✨✨
	// =========================================================

	// 计算实时全局跨片率
	totalCSTX := 0
	totalTX := 0
	for _, stat := range prefixMap {
		totalCSTX += stat.CSTXVolume
		totalTX += stat.TXVolume
	}

	currentRatio := 0.0
	if totalTX > 0 {
		currentRatio = float64(totalCSTX) / float64(totalTX)
	}

	// 核心创新点：全阶段阈值触发微迁移
	triggerThreshold := 0.62 // 设定阈值，你可以根据基线调整为 0.60 或 0.62

	// 如果图中的交易量太少（例如刚启动时），或者跨片率低于阈值，则直接拦截，不触发迁移
	if totalTX < 100 || currentRatio <= triggerThreshold {
		fmt.Printf(">>> [AERO-Micro] 监控点 %d: 当前跨片率 %.2f%% (低于阈值 %.2f%%)，无需唤醒 PPO，平滑运行中...\n", epoch, currentRatio*100, triggerThreshold*100)
		// 重新计算边界统计以保持状态连贯
		cs.ComputeEdges2Shard()
		return nil // 👈 这里 return nil 后，下面的 Python 调用和 JSON 生成就全被跳过了！
	}

	fmt.Printf(">>> [AERO-Micro] 🚨 触发警报！监控点 %d: 跨片率飙升至 %.2f%%！立即唤醒 PPO 智能体进行微迁移！\n", epoch, currentRatio*100)
	// =========================================================
	// ✨✨✨ 插入部分结束 ✨✨✨
	// =========================================================

	// 筛选有跨分片行为的前缀
	for _, stat := range prefixMap {
		//if stat.CSTXVolume > 0 {
		payload.CandidatePrefixes = append(payload.CandidatePrefixes, *stat)
		//}
	}

	//请务必加上这一行！👇👇👇
	fmt.Printf("!!! [DEBUG CHECK] 发送候选前缀数: %d\n", len(payload.CandidatePrefixes))

	// =======================================================
	// 🚑🚑🚑 【新增】救护车代码：防止死锁 🚑🚑🚑
	// 如果列表为空，Python 的保底逻辑无法触发。
	// 我们强制塞一个“虚拟前缀”进去，充当心跳包。
	if len(payload.CandidatePrefixes) == 0 {
		fmt.Println("!!! [DEBUG] 列表为空！正在注入虚拟前缀以激活系统...")
		payload.CandidatePrefixes = append(payload.CandidatePrefixes, PrefixStat{
			Prefix:       "00", // 一个假的十六进制前缀
			CurrentShard: 0,    // 假装在分片0
			TXVolume:     1,
			CSTXVolume:   1,
		})
	}
	// =======================================================

	// 2. 导出 JSON 并调用 Python
	_ = os.Mkdir("aero_io", 0755)
	jsonBytes, _ := json.MarshalIndent(payload, "", " ")
	_ = ioutil.WriteFile(fmt.Sprintf("aero_io/state_%d.json", epoch), jsonBytes, 0644)

	fmt.Println(">>> [AERO] 调用 Python Agent...")
	// 假设你的 python 脚本在根目录的 aero 文件夹下
	cmd := exec.Command("python", "aero/aero_agent.py", "--epoch", strconv.Itoa(epoch))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("!!! [AERO] Python 运行失败: %v (将跳过本轮迁移)\n", err)
		return nil
	}

	// 3. 读取并执行动作
	actionBytes, err := ioutil.ReadFile(fmt.Sprintf("aero_io/action_%d.json", epoch))
	if err != nil {
		log.Printf("!!! [AERO] 无法读取动作文件: %v\n", err)
		return nil
	}

	var action AEROActionResponse
	json.Unmarshal(actionBytes, &action)

	res := make(map[string]uint64)
	fmt.Printf(">>> [AERO] 执行 %d 个迁移指令\n", len(action.Migrations))

	for _, mig := range action.Migrations {
		// 调用辅助函数更新状态
		cs.UpdatePrefixShard(mig.Prefix, mig.ToShard)

		// 记录具体变动的账户 (用于返回给 Supervisor)
		for v := range cs.NetGraph.VertexSet {
			if len(v.Addr) >= prefixLen && v.Addr[:prefixLen] == mig.Prefix {
				res[v.Addr] = uint64(mig.ToShard)
			}
		}
	}

	// 🚑🚑🚑【终极混合保底】🚑🚑🚑
	if len(res) == 0 {
		fmt.Println("!!! [DEBUG] 正在寻找【真实节点】以强制切换 Epoch...")
		foundReal := false

		// 1. 先尝试抓真实节点
		for v := range cs.NetGraph.VertexSet {
			if len(v.Addr) > 2 {
				targetShard := (cs.PartitionMap[v] + 1) % cs.ShardNum
				res[v.Addr] = uint64(targetShard)
				fmt.Printf("!!! [DEBUG] 成功强制迁移真实节点: %s (%d -> %d)\n", v.Addr, cs.PartitionMap[v], targetShard)
				foundReal = true
				break
			}
		}

		// 2. 如果真实节点没抓到（比如刚启动图是空的），就用假地址兜底
		if !foundReal {
			fmt.Println("!!! [DEBUG] 真实图为空，启用【虚拟账户】兜底...")
			fakeAddr := "0000000000000000000000000000000000000000"
			res[fakeAddr] = 1
		}
	}
	// ---------------------------------------------------------

	// 重新计算边界统计
	cs.ComputeEdges2Shard()
	return res
}
