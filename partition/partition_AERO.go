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
	// ✨ 新增：图表示特征，记录当前前缀与每个分片的交易边数量
	EdgesToShard []int `json:"edges_to_shard"`
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
				EdgesToShard: make([]int, cs.ShardNum),
			}
		}
		stat := prefixMap[p]

		// 统计交易量和跨分片交易
		for _, u := range neighbors {
			stat.TXVolume++
			// 找到邻居所在的分片
			neighborShard := cs.PartitionMap[u]
			// ✨ 新增：累加到对应的分片引力槽中
			stat.EdgesToShard[neighborShard]++
			if cs.PartitionMap[v] != cs.PartitionMap[u] {
				stat.CSTXVolume++
			}
		}
	}

	// =========================================================
	// 1.2 (修正版) 根据前缀统计精准计算每个分片的跨片率
	// =========================================================
	shardTotalTx := make([]int, cs.ShardNum)
	shardCSTx := make([]int, cs.ShardNum)

	// 将所有前缀的交易量，按其所在的分片进行累加
	for _, stat := range prefixMap {
		shardTotalTx[stat.CurrentShard] += stat.TXVolume
		shardCSTx[stat.CurrentShard] += stat.CSTXVolume
	}

	// 计算每个分片的真实跨片率
	for i := 0; i < cs.ShardNum; i++ {
		if shardTotalTx[i] > 0 {
			payload.ShardCSTXRatios[i] = float64(shardCSTx[i]) / float64(shardTotalTx[i])
		} else {
			payload.ShardCSTXRatios[i] = 0.0 // 如果该分片没有交易，则跨片率为 0
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
	//triggerThreshold := 0.62 // 设定阈值，你可以根据基线调整为 0.60 或 0.62

	fmt.Printf(">>> [AERO-Micro] 🚨 触发警报！监控点 %d: 跨片率飙升至 %.2f%%！立即唤醒 PPO 智能体进行微迁移！\n", epoch, currentRatio*100)
	// =========================================================
	// ✨✨✨ 插入部分结束 ✨✨✨
	// =========================================================

	// 筛选有跨分片行为的前缀
	for _, stat := range prefixMap {
		if stat.CSTXVolume > 0 {
			payload.CandidatePrefixes = append(payload.CandidatePrefixes, *stat)
		}
	}

	//请务必加上这一行！👇👇👇
	fmt.Printf("!!! [DEBUG CHECK] 发送候选前缀数: %d\n", len(payload.CandidatePrefixes))

	// =======================================================
	// 🚑🚑🚑 【新增】救护车代码：防止死锁 🚑🚑🚑
	// 如果列表为空，Python 的保底逻辑无法触发。
	// 我们强制塞一个“虚拟前缀”进去，充当心跳包。
	//if len(payload.CandidatePrefixes) == 0 {
	//fmt.Println("!!! [DEBUG] 列表为空！正在注入虚拟前缀以激活系统...")
	//payload.CandidatePrefixes = append(payload.CandidatePrefixes, PrefixStat{
	//Prefix:       "00", // 一个假的十六进制前缀
	//CurrentShard: 0,    // 假装在分片0
	//TXVolume:     1,
	//CSTXVolume:   1,
	//})
	//}
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

	// =========================================================
	// 🚑🚑🚑【防死锁心跳包机制】🚑🚑🚑
	// 如果 PPO 决定“按兵不动”（或者没有任何前缀满足要求），导致迁移列表为空
	// 为了防止底层 PBFT 共识引擎因为空提案而陷入死锁（卡在当前 Epoch 不走）
	// 我们必须注入一个“假动作”。这个假动作绝对不能影响真实的图结构和跨片率！
	if len(res) == 0 {
		fmt.Println("!!! [DEBUG] PPO 决定不迁移 (或无候选)。为了防止 PBFT 死锁，正在注入【虚拟账户】心跳包...")

		// 使用一个绝对不可能在真实以太坊数据中存在的、没有任何交易记录的假地址
		fakeAddr := "000000000000000000000000000000000000dead"

		// 将其强行指派给分片 0（或者任何分片都行，因为它没有邻居，不会产生任何真实的跨片边）
		res[fakeAddr] = 0
	}
	// =========================================================

	// 重新计算边界统计
	cs.ComputeEdges2Shard()
	return res
}
