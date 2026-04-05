package pbft_all

import (
	"blockEmulator/consensus_shard/pbft_all/dataSupport"
	"blockEmulator/core"
	"blockEmulator/message"
	"blockEmulator/networks"
	"blockEmulator/params"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"
)

type CLPAPbftInsideExtraHandleMod struct {
	cdm      *dataSupport.Data_supportCLPA
	pbftNode *PbftConsensusNode
}

// propose request with different types
// 作用：Leader 节点在每一轮挖矿开始时，检查是不是该进行分片重构了
func (cphm *CLPAPbftInsideExtraHandleMod) HandleinPropose() (bool, *message.Request) {

	// 👇👇👇【调试日志】保持开启，用于确认心跳正常 👇👇👇
	cphm.pbftNode.pl.Plog.Println("!!! [DEBUG HEARTBEAT] 正在检查提案条件...")
	// 👆👆👆

	// 👇👇👇 【状态监控】 监控共识循环是否检测到了信号 👇👇👇
	if cphm.cdm.PartitionOn {
		cphm.pbftNode.pl.Plog.Println("!!! [DEBUG Propose] 进入 HandleinPropose (已持有锁), 准备发起分片重构提案...")

		// =======================================================
		// ❌ 删除：原来的 Sleep(6s)
		// ❌ 删除：cphm.sendPartitionReady()
		// ❌ 删除：for !cphm.getPartitionReady() ... 循环
		//
		// ✅ 说明：这些逻辑已经全部移到了 messageHandle.go 的 Lock() 之前！
		// =======================================================

		// 依然保留发送账户和交易数据的逻辑 (这个操作通常很快，或者由后台协程处理)
		cphm.sendAccounts_and_Txs()

		cphm.pbftNode.pl.Plog.Println("!!! [DEBUG] 数据发送完毕，立即执行 ProposePartition...")
		return cphm.proposePartition()
	} // <--- 注意：if 语句在这里结束

	// 👇👇👇 以下是普通区块的逻辑（只有 if 不成立时才会执行到这里） 👇👇👇
	block := cphm.pbftNode.CurChain.GenerateBlock(int32(cphm.pbftNode.NodeID))
	r := &message.Request{
		RequestType: message.BlockRequest,
		ReqTime:     time.Now(),
	}
	r.Msg.Content = block.Encode()
	return true, r
}

// the diy operation in preprepare
func (cphm *CLPAPbftInsideExtraHandleMod) HandleinPrePrepare(ppmsg *message.PrePrepare) bool {
	// 👇👇👇【新增探针 1】函数入口强制打印 👇👇👇
	cphm.pbftNode.pl.Plog.Printf("!!! [DEBUG TRAP] S%dN%d 进入 HandleinPrePrepare! 收到消息类型: %s\n",
		cphm.pbftNode.ShardID, cphm.pbftNode.NodeID, ppmsg.RequestMsg.RequestType)
	// 👆👆👆

	// judge whether it is a partitionRequest or not
	isPartitionReq := ppmsg.RequestMsg.RequestType == message.PartitionReq

	// 👇👇👇【新增探针 2】打印判断结果 👇👇👇
	cphm.pbftNode.pl.Plog.Printf("!!! [DEBUG TRAP] 是否为分片请求: %v (本地 PartitionReq字符串: %s)\n", isPartitionReq, message.PartitionReq)
	// 👆👆👆

	if isPartitionReq {
		// 👇👇👇 【关键修复】 必须检查本地是否已收到 Supervisor 的 Map (PartitionOn) 👇👇👇
		if !cphm.cdm.PartitionOn {
			cphm.pbftNode.pl.Plog.Printf("S%dN%d : [REJECT] 收到 PartitionReq，但 PartitionOn 为 false (尚未收到映射表)。拒绝处理，等待重试。\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
			return false // <--- 拒绝请求，不占用 SequenceID
		}
		// 👆👆👆

		cphm.pbftNode.pl.Plog.Printf("S%dN%d : a partition block\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
		cphm.pbftNode.pl.Plog.Printf("S%dN%d : partition request detected, skipping validation and adding to pool.\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
		cphm.pbftNode.requestPool[string(ppmsg.Digest)] = ppmsg.RequestMsg
		return true
	} else {
		// the request is a block
		if cphm.pbftNode.CurChain.IsValidBlock(core.DecodeB(ppmsg.RequestMsg.Msg.Content)) != nil {
			cphm.pbftNode.pl.Plog.Printf("S%dN%d : not a valid block\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
			return false
		}
	}
	cphm.pbftNode.pl.Plog.Printf("S%dN%d : the pre-prepare message is correct, putting it into the RequestPool. \n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
	cphm.pbftNode.requestPool[string(ppmsg.Digest)] = ppmsg.RequestMsg
	// merge to be a prepare message
	return true
}

// the operation in prepare...
func (cphm *CLPAPbftInsideExtraHandleMod) HandleinPrepare(pmsg *message.Prepare) bool {
	// 👇👇👇 【新增日志】 在正确的地方打印收票情况 👇👇👇
	// 只要看到这个日志，就说明 Leader 收到了来自 S0N1/N2/N3 的投票
	cphm.pbftNode.pl.Plog.Printf("!!! [DEBUG VOTE] InsideMod 确认收到 Prepare 投票 (SeqID: %d)\n", pmsg.SeqID)
	// 👆👆👆

	// 👇👇👇 【必须保持 false】 👇👇👇
	// 返回 false，PBFT 主流程才会继续计票
	return false
}

// the operation in commit.
func (cphm *CLPAPbftInsideExtraHandleMod) HandleinCommit(cmsg *message.Commit) bool {
	r := cphm.pbftNode.requestPool[string(cmsg.Digest)]
	// requestType ...
	if r.RequestType == message.PartitionReq {
		// if a partition Requst ...
		atm := message.DecodeAccountTransferMsg(r.Msg.Content)
		cphm.accountTransfer_do(atm)
		return true
	}
	// if a block request ...
	block := core.DecodeB(r.Msg.Content)
	cphm.pbftNode.pl.Plog.Printf("S%dN%d : adding the block %d...now height = %d \n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID, block.Header.Number, cphm.pbftNode.CurChain.CurrentBlock.Header.Number)
	cphm.pbftNode.CurChain.AddBlock(block)
	cphm.pbftNode.pl.Plog.Printf("S%dN%d : added the block %d... \n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID, block.Header.Number)
	cphm.pbftNode.CurChain.PrintBlockChain()

	// now try to relay txs to other shards (for main nodes)
	if cphm.pbftNode.NodeID == uint64(cphm.pbftNode.view.Load()) {
		cphm.pbftNode.pl.Plog.Printf("S%dN%d : main node is trying to send relay txs at height = %d \n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID, block.Header.Number)
		// generate relay pool and collect txs excuted
		cphm.pbftNode.CurChain.Txpool.RelayPool = make(map[uint64][]*core.Transaction)
		interShardTxs := make([]*core.Transaction, 0)
		relay1Txs := make([]*core.Transaction, 0)
		relay2Txs := make([]*core.Transaction, 0)

		for _, tx := range block.Body {
			ssid := cphm.pbftNode.CurChain.Get_PartitionMap(tx.Sender)
			rsid := cphm.pbftNode.CurChain.Get_PartitionMap(tx.Recipient)
			if !tx.Relayed && ssid != cphm.pbftNode.ShardID {
				log.Panic("incorrect tx")
			}
			if tx.Relayed && rsid != cphm.pbftNode.ShardID {
				log.Panic("incorrect tx")
			}
			if rsid != cphm.pbftNode.ShardID {
				relay1Txs = append(relay1Txs, tx)
				tx.Relayed = true
				cphm.pbftNode.CurChain.Txpool.AddRelayTx(tx, rsid)
			} else {
				if tx.Relayed {
					relay2Txs = append(relay2Txs, tx)
				} else {
					interShardTxs = append(interShardTxs, tx)
				}
			}
		}

		// send relay txs
		if params.RelayWithMerkleProof == 1 {
			cphm.pbftNode.RelayWithProofSend(block)
		} else {
			cphm.pbftNode.RelayMsgSend()
		}

		// send txs excuted in this block to the listener
		// add more message to measure more metrics
		bim := message.BlockInfoMsg{
			BlockBodyLength: len(block.Body),
			InnerShardTxs:   interShardTxs,
			Epoch:           int(cphm.cdm.AccountTransferRound),

			Relay1Txs: relay1Txs,
			Relay2Txs: relay2Txs,

			SenderShardID: cphm.pbftNode.ShardID,
			ProposeTime:   r.ReqTime,
			CommitTime:    time.Now(),
		}
		bByte, err := json.Marshal(bim)
		if err != nil {
			log.Panic()
		}
		msg_send := message.MergeMessage(message.CBlockInfo, bByte)
		go networks.TcpDial(msg_send, cphm.pbftNode.ip_nodeTable[params.SupervisorShard][0])
		cphm.pbftNode.pl.Plog.Printf("S%dN%d : sended excuted txs\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)

		cphm.pbftNode.CurChain.Txpool.GetLocked()

		metricName := []string{
			"Block Height",
			"EpochID of this block",
			"TxPool Size",
			"# of all Txs in this block",
			"# of Relay1 Txs in this block",
			"# of Relay2 Txs in this block",
			"TimeStamp - Propose (unixMill)",
			"TimeStamp - Commit (unixMill)",

			"SUM of confirm latency (ms, All Txs)",
			"SUM of confirm latency (ms, Relay1 Txs) (Duration: Relay1 proposed -> Relay1 Commit)",
			"SUM of confirm latency (ms, Relay2 Txs) (Duration: Relay1 proposed -> Relay2 Commit)",
		}
		metricVal := []string{
			strconv.Itoa(int(block.Header.Number)),
			strconv.Itoa(bim.Epoch),
			strconv.Itoa(len(cphm.pbftNode.CurChain.Txpool.TxQueue)),
			strconv.Itoa(len(block.Body)),
			strconv.Itoa(len(relay1Txs)),
			strconv.Itoa(len(relay2Txs)),
			strconv.FormatInt(bim.ProposeTime.UnixMilli(), 10),
			strconv.FormatInt(bim.CommitTime.UnixMilli(), 10),

			strconv.FormatInt(computeTCL(block.Body, bim.CommitTime), 10),
			strconv.FormatInt(computeTCL(relay1Txs, bim.CommitTime), 10),
			strconv.FormatInt(computeTCL(relay2Txs, bim.CommitTime), 10),
		}
		cphm.pbftNode.writeCSVline(metricName, metricVal)
		cphm.pbftNode.CurChain.Txpool.GetUnlocked()
	}
	return true
}

func (cphm *CLPAPbftInsideExtraHandleMod) HandleReqestforOldSeq(*message.RequestOldMessage) bool {
	fmt.Println("No operations are performed in Extra handle mod")
	return true
}

// the operation for sequential requests
func (cphm *CLPAPbftInsideExtraHandleMod) HandleforSequentialRequest(som *message.SendOldMessage) bool {
	if int(som.SeqEndHeight-som.SeqStartHeight+1) != len(som.OldRequest) {
		cphm.pbftNode.pl.Plog.Printf("S%dN%d : the SendOldMessage message is not enough\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
	} else { // add the block into the node pbft blockchain
		for height := som.SeqStartHeight; height <= som.SeqEndHeight; height++ {
			r := som.OldRequest[height-som.SeqStartHeight]
			if r.RequestType == message.BlockRequest {
				b := core.DecodeB(r.Msg.Content)
				cphm.pbftNode.CurChain.AddBlock(b)
			} else {
				atm := message.DecodeAccountTransferMsg(r.Msg.Content)
				cphm.accountTransfer_do(atm)
			}
		}
		cphm.pbftNode.sequenceID = som.SeqEndHeight + 1
		cphm.pbftNode.CurChain.PrintBlockChain()
	}
	return true
}

// =================================================================
// 👇👇👇【新增/重构】公开的辅助函数 (Moved & Exported) 👇👇👇
// =================================================================

// SendPartitionReady 广播“分片就绪”信号
func (cphm *CLPAPbftInsideExtraHandleMod) SendPartitionReady() {
	pr := message.PartitionReady{
		FromShard: cphm.pbftNode.ShardID,
		NowSeqID:  cphm.pbftNode.sequenceID,
	}
	prByte, err := json.Marshal(pr)
	if err != nil {
		log.Panic(err)
	}
	msg_send := message.MergeMessage(message.CPartitionReady, prByte)
	// 广播给其他分片的节点（通常是广播给所有已知节点，由接收方过滤）
	networks.Broadcast(cphm.pbftNode.RunningNode.IPaddr, cphm.pbftNode.getNeighborNodes(), msg_send)
	cphm.pbftNode.pl.Plog.Printf("!!! [DEBUG SYNCHRONIZE] S%dN%d 已广播 PartitionReady 信号\n", cphm.pbftNode.ShardID, cphm.pbftNode.NodeID)
}

// GetPartitionReady 检查是否收到足够多的就绪信号
func (cphm *CLPAPbftInsideExtraHandleMod) GetPartitionReady() bool {
	cphm.cdm.P_ReadyLock.Lock()
	defer cphm.cdm.P_ReadyLock.Unlock()

	// 目标：收到来自所有其他分片的 Ready 信号
	// 总分片数 - 1 (自己)
	targetNum := int(cphm.pbftNode.pbftChainConfig.ShardNums) - 1
	currentReady := len(cphm.cdm.PartitionReady)

	if currentReady >= targetNum {
		return true
	}
	return false
}
