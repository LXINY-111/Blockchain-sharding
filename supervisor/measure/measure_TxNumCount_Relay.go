package measure

import (
	"blockEmulator/message"
	"strconv"
)

// to test Tx number
type TestTxNumCount_Relay struct {
	epochID int
	txNum   []float64

	normalTxNum []int
	relay1TxNum []int
	relay2TxNum []int
}

func NewTestTxNumCount_Relay() *TestTxNumCount_Relay {
	return &TestTxNumCount_Relay{
		epochID: -1,
		txNum:   make([]float64, 0),

		normalTxNum: make([]int, 0),
		relay1TxNum: make([]int, 0),
		relay2TxNum: make([]int, 0),
	}
}

func (ttnc *TestTxNumCount_Relay) OutputMetricName() string {
	return "Tx_number"
}

func (ttnc *TestTxNumCount_Relay) UpdateMeasureRecord(b *message.BlockInfoMsg) {
	if b.BlockBodyLength == 0 { // empty block
		return
	}
	epochid := b.Epoch
	r1TxNum := len(b.Relay1Txs)
	r2TxNum := len(b.Relay2Txs)
	// extend
	for ttnc.epochID < epochid {
		ttnc.txNum = append(ttnc.txNum, 0)
		ttnc.relay1TxNum = append(ttnc.relay1TxNum, 0)
		ttnc.relay2TxNum = append(ttnc.relay2TxNum, 0)
		ttnc.normalTxNum = append(ttnc.normalTxNum, 0)

		ttnc.epochID++
	}

	ttnc.normalTxNum[epochid] += len(b.InnerShardTxs)
	ttnc.relay1TxNum[epochid] += r1TxNum
	ttnc.relay2TxNum[epochid] += r2TxNum
	ttnc.txNum[epochid] += float64(len(b.InnerShardTxs)) + float64(len(b.Relay1Txs)+len(b.Relay2Txs))/2

	// 🔥🔥🔥【简单修改版】🔥🔥🔥
	// 只有当 epochid 发生变化（新 Epoch 开始了），才把上一个 Epoch 的数据存下来
	// 或者利用随机数/取模来降低频率
	// 例如：只有当这一轮交易数量累积到一定程度才写

	// 推荐：用简单的取模，每处理 100 个 Block 消息写一次
	// 这样既不会卡死，即使崩溃也最多丢 100 个块的数据
	// (需要引入 "math/rand" 或者简单地利用一些变动的值，这里没法直接存状态，建议用上面改结构体的方法最稳妥)

	// 如果不想改结构体，可以用这个“低频写入”策略：
	if ttnc.normalTxNum[epochid] > 0 && ttnc.normalTxNum[epochid]%50 == 0 {
		ttnc.writeToCSV()
	}
}

func (ttnc *TestTxNumCount_Relay) HandleExtraMessage([]byte) {}

func (ttnc *TestTxNumCount_Relay) OutputRecord() (perEpochCTXs []float64, totTxNum float64) {
	ttnc.writeToCSV()

	// calculate the simple result
	perEpochCTXs = make([]float64, 0)
	totTxNum = 0.0
	for _, tn := range ttnc.txNum {
		perEpochCTXs = append(perEpochCTXs, tn)
		totTxNum += tn
	}
	return perEpochCTXs, totTxNum
}

func (ttnc *TestTxNumCount_Relay) writeToCSV() {
	fileName := ttnc.OutputMetricName()
	// Header 只有文件为空时会自动写入，这里不用担心
	measureName := []string{"EpochID", "Total tx # in this epoch", "Normal tx # in this epoch", "Relay1 tx # in this epoch", "Relay2 tx # in this epoch"}
	measureVals := make([][]string, 0)

	// 🔥 核心修改：只获取最新产生的一个 Epoch 数据 🔥
	if len(ttnc.txNum) == 0 {
		return
	}
	// 获取最后一个索引
	eid := len(ttnc.txNum) - 1

	csvLine := []string{
		strconv.Itoa(eid),
		// 格式化浮点数，保留小数点后8位
		strconv.FormatFloat(ttnc.txNum[eid], 'f', '8', 64),
		strconv.Itoa(ttnc.normalTxNum[eid]),
		strconv.Itoa(ttnc.relay1TxNum[eid]),
		strconv.Itoa(ttnc.relay2TxNum[eid]),
	}

	// 将这一行加入待写入列表
	measureVals = append(measureVals, csvLine)

	// 调用工具函数追加写入
	WriteMetricsToCSV(fileName, measureName, measureVals)
}
