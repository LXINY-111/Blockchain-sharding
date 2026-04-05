package committee

import (
	"blockEmulator/core"
	"blockEmulator/message"
	"blockEmulator/networks"
	"blockEmulator/params"
	"blockEmulator/partition"
	"blockEmulator/supervisor/signal"
	"blockEmulator/supervisor/supervisor_log"
	"blockEmulator/utils"
	"encoding/csv"
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// CLPA committee operations
type CLPACommitteeModule struct {
	csvPath      string
	dataTotalNum int
	nowDataNum   int
	batchDataNum int

	// additional variants
	curEpoch            int32
	clpaLock            sync.Mutex
	clpaGraph           *partition.CLPAState
	modifiedMap         map[string]uint64
	clpaLastRunningTime time.Time
	clpaFreq            int

	// logger module
	sl *supervisor_log.SupervisorLog

	// control components
	Ss          *signal.StopSignal // to control the stop message sending
	IpNodeTable map[uint64]map[uint64]string
}

func NewCLPACommitteeModule(Ip_nodeTable map[uint64]map[uint64]string, Ss *signal.StopSignal, sl *supervisor_log.SupervisorLog, csvFilePath string, dataNum, batchNum, clpaFrequency int) *CLPACommitteeModule {
	cg := new(partition.CLPAState)
	cg.Init_CLPAState(0.5, 100, params.ShardNum)
	return &CLPACommitteeModule{
		csvPath:             csvFilePath,
		dataTotalNum:        dataNum,
		batchDataNum:        batchNum,
		nowDataNum:          0,
		clpaGraph:           cg,
		modifiedMap:         make(map[string]uint64),
		clpaFreq:            clpaFrequency,
		clpaLastRunningTime: time.Time{},
		IpNodeTable:         Ip_nodeTable,
		Ss:                  Ss,
		sl:                  sl,
		curEpoch:            0,
	}
}

func (ccm *CLPACommitteeModule) HandleOtherMessage([]byte) {}

func (ccm *CLPACommitteeModule) fetchModifiedMap(key string) uint64 {
	if val, ok := ccm.modifiedMap[key]; !ok {
		return uint64(utils.Addr2Shard(key))
	} else {
		return val
	}
}

func (ccm *CLPACommitteeModule) txSending(txlist []*core.Transaction) {
	// the txs will be sent
	sendToShard := make(map[uint64][]*core.Transaction)

	for idx := 0; idx <= len(txlist); idx++ {
		if idx > 0 && (idx%params.InjectSpeed == 0 || idx == len(txlist)) {
			// send to shard
			for sid := uint64(0); sid < uint64(params.ShardNum); sid++ {
				it := message.InjectTxs{
					Txs:       sendToShard[sid],
					ToShardID: sid,
				}
				itByte, err := json.Marshal(it)
				if err != nil {
					log.Panic(err)
				}
				send_msg := message.MergeMessage(message.CInject, itByte)
				go networks.TcpDial(send_msg, ccm.IpNodeTable[sid][0])
			}
			sendToShard = make(map[uint64][]*core.Transaction)
			time.Sleep(time.Second)
		}
		if idx == len(txlist) {
			break
		}
		tx := txlist[idx]
		sendersid := ccm.fetchModifiedMap(tx.Sender)
		sendToShard[sendersid] = append(sendToShard[sendersid], tx)
	}
}

func (ccm *CLPACommitteeModule) MsgSendingControl() {
	txfile, err := os.Open(ccm.csvPath)
	if err != nil {
		log.Panic(err)
	}
	defer txfile.Close()
	reader := csv.NewReader(txfile)

	// ==========================================
	// 🌟 1. 内存预加载阶段：一次性把 10 万条数据吃进内存
	// ==========================================
	allTxs := make([]*core.Transaction, 0)
	tempCount := 0
	for {
		data, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Panic(err)
		}
		if tx, ok := data2tx(data, uint64(tempCount)); ok {
			allTxs = append(allTxs, tx)
			tempCount++
		}
		if tempCount == ccm.dataTotalNum {
			break
		}
	}
	ccm.sl.Slog.Printf("!!! [AERO Loop] 成功将 %d 条交易加载到内存，准备开启无限循环训练 !!!\n", len(allTxs))

	// ==========================================
	// 🌟 2. 无限循环发送阶段
	// ==========================================
	txlist := make([]*core.Transaction, 0)
	clpaCnt := 0
	globalSendCount := 0 // 记录系统总共发了多少条（打破 10 万上限）

	for {
		// 核心逻辑：取模运算，实现首尾相连无限循环
		idx := globalSendCount % len(allTxs)
		originalTx := allTxs[idx]

		// 【关键安全策略】：深度拷贝并修改 Nonce，把旧交易伪装成“新交易”
		// 防止底层交易池 (TxPool) 因为 Hash 查重直接把循环的交易丢弃
		tx := new(core.Transaction)
		*tx = *originalTx
		// 每经历一轮 10 万条，Nonce 加上一个极大值，保证哈希唯一
		tx.Nonce = tx.Nonce + uint64(globalSendCount/len(allTxs))*10000000
		tx.Time = time.Now()

		txlist = append(txlist, tx)
		globalSendCount++
		ccm.nowDataNum = globalSendCount // 欺骗系统，让它以为数据一直在增加

		// --- 批量下发条件 ---
		if len(txlist) == int(ccm.batchDataNum) {
			if ccm.clpaLastRunningTime.IsZero() {
				ccm.clpaLastRunningTime = time.Now()
			}
			ccm.txSending(txlist)

			txlist = make([]*core.Transaction, 0)
			ccm.Ss.StopGap_Reset()
		}

		// --- Epoch 切换与 AI 触发机制 (保持原有 AERO 逻辑不变) ---
		if params.ShardNum > 1 && !ccm.clpaLastRunningTime.IsZero() && time.Since(ccm.clpaLastRunningTime) >= time.Duration(ccm.clpaFreq)*time.Second {
			ccm.clpaLock.Lock()
			clpaCnt++

			// === 触发 Python 端的 PPO 进行训练和决策 ===
			mmap := ccm.clpaGraph.AERO_Partition(clpaCnt)

			ccm.clpaMapSend(mmap)
			for key, val := range mmap {
				ccm.modifiedMap[key] = val
			}
			ccm.clpaReset()
			ccm.clpaLock.Unlock()

			// 阻塞等待所有分片同步状态
			for atomic.LoadInt32(&ccm.curEpoch) != int32(clpaCnt) {
				time.Sleep(time.Second)
			}
			ccm.clpaLastRunningTime = time.Now()
			ccm.sl.Slog.Printf("!!! [AERO Loop] Next CLPA epoch %d begins. Total Tx Sent: %d !!!\n", clpaCnt, globalSendCount)

			// ==========================================
			// 🛑 新增：安全刹车机制，防止 Windows 端口耗尽
			// ==========================================
			if clpaCnt >= 100 {
				ccm.sl.Slog.Println(">>> [Safe Stop] 已达到 100 Epoch，PPO 已充分收敛！准备安全退出并生成 CSV 数据...")
				return // 直接退出 MsgSendingControl 函数，触发上层生成 csv 并平滑关闭
			}
			// ==========================================
		}
	}
}

// 作用：负责将分片变更信息（Map）打包并广播给所有 Shard 节点。
func (ccm *CLPACommitteeModule) clpaMapSend(m map[string]uint64) {

	// 👇👇👇 【新增日志 1】 确认发送前的原始数据 👇👇👇
	ccm.sl.Slog.Printf("!!! [DEBUG Supervisor] 准备发送分片映射表 (Epoch: %d), 变更数量: %d\n", atomic.LoadInt32(&ccm.curEpoch)+1, len(m))
	for k, v := range m {
		// 只打印前3个，防止刷屏
		ccm.sl.Slog.Printf("!!! [DEBUG Supervisor] 变更详情示例: Addr: %s... -> Shard: %d\n", k[:10], v)
		break
	}
	// 👆👆👆

	// send partition modified Map message
	pm := message.PartitionModifiedMap{
		PartitionModified: m,
	}
	pmByte, err := json.Marshal(pm)
	if err != nil {
		log.Panic()
	}
	send_msg := message.MergeMessage(message.CPartitionMsg, pmByte)

	// === 修改开始 ===
	// 遍历所有分片
	for i := uint64(0); i < uint64(params.ShardNum); i++ {
		// 遍历分片内的所有节点
		for nodeID, addr := range ccm.IpNodeTable[i] {
			go networks.TcpDial(send_msg, addr)

			// 👇👇👇 【新增日志 2】 确认网络发送调用成功 (必须在内层循环里) 👇👇👇
			ccm.sl.Slog.Printf("!!! [DEBUG Supervisor] 已向 Shard %d Node %d (%s) 发送消息\n", i, nodeID, addr)
			// 👆👆👆
		} // <--- 1. 结束内层循环 (for nodeID, addr ...)
	} // <--- 2. 结束外层循环 (for i ...)

	ccm.sl.Slog.Println("Supervisor: all partition map message has been sent. ")
} // <--- 3. 结束函数 (func clpaMapSend)

func (ccm *CLPACommitteeModule) clpaReset() {
	ccm.clpaGraph = new(partition.CLPAState)
	ccm.clpaGraph.Init_CLPAState(0.5, 100, params.ShardNum)
	for key, val := range ccm.modifiedMap {
		ccm.clpaGraph.PartitionMap[partition.Vertex{Addr: key}] = int(val)
	}
}

func (ccm *CLPACommitteeModule) HandleBlockInfo(b *message.BlockInfoMsg) {
	ccm.sl.Slog.Printf("Supervisor: received from shard %d in epoch %d.\n", b.SenderShardID, b.Epoch)
	if atomic.CompareAndSwapInt32(&ccm.curEpoch, int32(b.Epoch-1), int32(b.Epoch)) {
		ccm.sl.Slog.Println("this curEpoch is updated", b.Epoch)
	}
	if b.BlockBodyLength == 0 {
		return
	}
	ccm.clpaLock.Lock()
	for _, tx := range b.InnerShardTxs {
		ccm.clpaGraph.AddEdge(partition.Vertex{Addr: tx.Sender}, partition.Vertex{Addr: tx.Recipient})
	}
	for _, r2tx := range b.Relay2Txs {
		ccm.clpaGraph.AddEdge(partition.Vertex{Addr: r2tx.Sender}, partition.Vertex{Addr: r2tx.Recipient})
	}
	ccm.clpaLock.Unlock()
}
