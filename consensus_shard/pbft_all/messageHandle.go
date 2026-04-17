package pbft_all

import (
	"blockEmulator/message"
	"blockEmulator/networks"
	"blockEmulator/params"
	"blockEmulator/shard"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// this func is only invoked by main node
func (p *PbftConsensusNode) Propose() {
	// wait other nodes to start TCPlistening, sleep 5 sec.
	time.Sleep(5 * time.Second)

	nextRoundBeginSignal := make(chan bool)

	go func() {
		// go into the next round
		for {
			time.Sleep(time.Duration(int64(p.pbftChainConfig.BlockInterval)) * time.Millisecond)
			// send a signal to another GO-Routine. It will block until a GO-Routine try to fetch data from this channel.
			for p.pbftStage.Load() != 1 {
				time.Sleep(time.Millisecond * 100)
			}
			nextRoundBeginSignal <- true
		}
	}()

	// >>>>>>>>>> [最终修正版] 宽容型监控协程 >>>>>>>>>>
	go func() {
		// 状态追踪变量
		lastStage := p.pbftStage.Load()
		lastStageChangeTime := time.Now().UnixMilli()

		for {
			time.Sleep(time.Second) // 每秒检查一次

			// 1. 获取当前实时状态
			currentStage := p.pbftStage.Load()
			now := time.Now().UnixMilli()

			// 2. 更新状态计时器
			// 只要状态在变（比如从 PrePrepare -> Prepare -> Commit），就重置计时器
			if currentStage != lastStage {
				lastStage = currentStage
				lastStageChangeTime = now
			}

			// 3. 检查分片信号
			var isPartitionOn bool
			if h, ok := p.ihm.(*CLPAPbftInsideExtraHandleMod); ok {
				isPartitionOn = h.cdm.PartitionOn
			}

			// 4. 智能监控逻辑
			if isPartitionOn {
				// 只有当：(1) 不在空闲状态 (2) 且在该状态卡了超过 30秒 (30000ms)
				// 才认为是真死锁。这足以覆盖 BlockInterval(5s) 和网络延迟。
				if currentStage != 1 {
					// ！！！！这里改成了 30000 ！！！！
					if now-lastStageChangeTime > 30000 {
						fmt.Printf("!!! [DEBUG ACTION] 监测到顽固卡死(Stage=%d, 持续%dms) & 分片信号！执行安全重置...\n", currentStage, now-lastStageChangeTime)

						// A. 重置状态
						p.pbftStage.Store(1)

						// 重置计时器，防止连续触发
						lastStageChangeTime = time.Now().UnixMilli()

						if now-lastStageChangeTime > 30000 {
							fmt.Printf("!!! [DEBUG ACTION] 监测到顽固卡死(Stage=%d, 持续%dms) & 分片信号！执行安全重置...\n", currentStage, now-lastStageChangeTime)

							// A. 重置状态
							p.pbftStage.Store(1)

							// 重置计时器，防止连续触发
							lastStageChangeTime = time.Now().UnixMilli()

							// B. 安全解锁 (⭐ 终极修复点 ⭐)
							// 只有主节点(Leader)在 Propose() 时才加了这把锁。
							// 从节点绝不能去解锁，否则会触发 Go底层的 fatal error！
							if p.NodeID == uint64(p.view.Load()) {
								p.sequenceLock.Unlock()
								fmt.Println("!!! [DEBUG ACTION] Leader 强制释放 sequenceLock 锁成功！")
							}

							// C. 唤醒所有卡住的协程
							p.conditionalVarpbftLock.Broadcast()
						}

						// C. 唤醒
						p.conditionalVarpbftLock.Broadcast()
					} else {
						// 打印心跳，证明它在等待而不是卡死（可选，为了不刷屏可以注释掉）
						// fmt.Printf("!!! [DEBUG MONITOR] 分片共识进行中 (Stage=%d, 已耗时%dms)... 请耐心等待\n", currentStage, now-lastStageChangeTime)
					}
				}
			}

			// 原有的 ViewChange 超时逻辑
			if time.Now().UnixMilli()-p.lastCommitTime.Load() > int64(params.PbftViewChangeTimeOut) {
				p.lastCommitTime.Store(time.Now().UnixMilli())
				go p.viewChangePropose()
			}
		}
	}()
	// <<<<<<<<<< [最终修正版] 结束 <<<<<<<<<<

	for {
		select {
		case <-nextRoundBeginSignal:
			go func() {
				// if this node is not leader, do not propose.
				if uint64(p.view.Load()) != p.NodeID {
					return
				}

				// 👇👇👇【关键修复】在获取 sequenceLock 之前处理分片同步逻辑 👇👇👇
				if h, ok := p.ihm.(*CLPAPbftInsideExtraHandleMod); ok {
					if h.cdm.PartitionOn {
						fmt.Println("!!! [TrafficControl] 检测到 PartitionOn，进入锁外同步阶段...")

						// 1. 锁外等待 (Traffic Control)
						time.Sleep(6 * time.Second)

						// 2. 广播 Ready (调用新加的大写函数)
						fmt.Println("!!! [DEBUG SYNCHRONIZE] 广播 PartitionReady 信号...")
						h.SendPartitionReady()

						// 3. 循环等待 (调用新加的大写函数)
						waitCount := 0
						maxWait := 20
						fmt.Println("!!! [DEBUG SYNCHRONIZE] 开始等待全网 Ready (锁外等待)...")

						for !h.GetPartitionReady() && waitCount < maxWait {
							time.Sleep(1 * time.Second)
							waitCount++
							fmt.Printf("!!! [DEBUG SYNCHRONIZE] 等待中... (%d/%d)\n", waitCount, maxWait)
						}
					}
				}
				// 👆👆👆

				p.sequenceLock.Lock()
				p.pl.Plog.Printf("S%dN%d get sequenceLock locked, now trying to propose...\n", p.ShardID, p.NodeID)
				// propose
				// implement interface to generate propose
				_, r := p.ihm.HandleinPropose()

				digest := getDigest(r)
				p.requestPool[string(digest)] = r
				p.pl.Plog.Printf("S%dN%d put the request into the pool ...\n", p.ShardID, p.NodeID)

				ppmsg := message.PrePrepare{
					RequestMsg: r,
					Digest:     digest,
					SeqID:      p.sequenceID,
				}
				p.height2Digest[p.sequenceID] = string(digest)
				// marshal and broadcast
				ppbyte, err := json.Marshal(ppmsg)
				if err != nil {
					log.Panic()
				}
				msg_send := message.MergeMessage(message.CPrePrepare, ppbyte)
				networks.Broadcast(p.RunningNode.IPaddr, p.getNeighborNodes(), msg_send)
				networks.TcpDial(msg_send, p.RunningNode.IPaddr)
				p.pbftStage.Store(2)
			}()

		case <-p.pStop:
			p.pl.Plog.Printf("S%dN%d get stopSignal in Propose Routine, now stop...\n", p.ShardID, p.NodeID)
			return
		}
	}
}

// Handle pre-prepare messages here.
// If you want to do more operations in the pre-prepare stage, you can implement the interface "ExtraOpInConsensus",
// and call the function: **ExtraOpInConsensus.HandleinPrePrepare**
func (p *PbftConsensusNode) handlePrePrepare(content []byte) {
	p.RunningNode.PrintNode()
	fmt.Println("received the PrePrepare ...")
	// decode the message
	ppmsg := new(message.PrePrepare)
	err := json.Unmarshal(content, ppmsg)
	if err != nil {
		log.Panic(err)
	}

	curView := p.view.Load()
	p.pbftLock.Lock()
	defer p.pbftLock.Unlock()
	for p.pbftStage.Load() < 1 && ppmsg.SeqID >= p.sequenceID && p.view.Load() == curView {
		p.conditionalVarpbftLock.Wait()
	}
	defer p.conditionalVarpbftLock.Broadcast()

	// if this message is out of date, return.
	if ppmsg.SeqID < p.sequenceID || p.view.Load() != curView {
		return
	}

	flag := false
	if digest := getDigest(ppmsg.RequestMsg); string(digest) != string(ppmsg.Digest) {
		p.pl.Plog.Printf("S%dN%d : the digest is not consistent, so refuse to prepare. \n", p.ShardID, p.NodeID)
	} else if p.sequenceID < ppmsg.SeqID {
		p.requestPool[string(getDigest(ppmsg.RequestMsg))] = ppmsg.RequestMsg
		p.height2Digest[ppmsg.SeqID] = string(getDigest(ppmsg.RequestMsg))
		p.pl.Plog.Printf("S%dN%d : the Sequence id is not consistent, so refuse to prepare. \n", p.ShardID, p.NodeID)
	} else {
		// do your operation in this interface
		flag = p.ihm.HandleinPrePrepare(ppmsg)
		p.requestPool[string(getDigest(ppmsg.RequestMsg))] = ppmsg.RequestMsg
		p.height2Digest[ppmsg.SeqID] = string(getDigest(ppmsg.RequestMsg))
	}
	// if the message is true, broadcast the prepare message
	if flag {
		pre := message.Prepare{
			Digest:     ppmsg.Digest,
			SeqID:      ppmsg.SeqID,
			SenderNode: p.RunningNode,
		}
		prepareByte, err := json.Marshal(pre)
		if err != nil {
			log.Panic()
		}
		// broadcast
		msg_send := message.MergeMessage(message.CPrepare, prepareByte)
		networks.Broadcast(p.RunningNode.IPaddr, p.getNeighborNodes(), msg_send)
		networks.TcpDial(msg_send, p.RunningNode.IPaddr)
		p.pl.Plog.Printf("S%dN%d : has broadcast the prepare message \n", p.ShardID, p.NodeID)

		// Pbft stage add 1. It means that this round of pbft goes into the next stage, i.e., Prepare stage.
		p.pbftStage.Add(1)
	}
}

// Handle prepare messages here.
// If you want to do more operations in the prepare stage, you can implement the interface "ExtraOpInConsensus",
// and call the function: **ExtraOpInConsensus.HandleinPrepare**
func (p *PbftConsensusNode) handlePrepare(content []byte) {
	p.pl.Plog.Printf("S%dN%d : received the Prepare ...\n", p.ShardID, p.NodeID)
	// decode the message
	pmsg := new(message.Prepare)
	err := json.Unmarshal(content, pmsg)
	if err != nil {
		log.Panic(err)
	}

	curView := p.view.Load()
	p.pbftLock.Lock()
	defer p.pbftLock.Unlock()
	for p.pbftStage.Load() < 2 && pmsg.SeqID >= p.sequenceID && p.view.Load() == curView {
		p.conditionalVarpbftLock.Wait()
	}
	defer p.conditionalVarpbftLock.Broadcast()

	// if this message is out of date, return.
	if pmsg.SeqID < p.sequenceID || p.view.Load() != curView {
		return
	}

	if _, ok := p.requestPool[string(pmsg.Digest)]; !ok {
		p.pl.Plog.Printf("S%dN%d : doesn't have the digest in the requst pool, refuse to commit\n", p.ShardID, p.NodeID)
	} else if p.sequenceID < pmsg.SeqID {
		p.pl.Plog.Printf("S%dN%d : inconsistent sequence ID, refuse to commit\n", p.ShardID, p.NodeID)
	} else {
		// if needed more operations, implement interfaces
		p.ihm.HandleinPrepare(pmsg)

		p.set2DMap(true, string(pmsg.Digest), pmsg.SenderNode)
		cnt := len(p.cntPrepareConfirm[string(pmsg.Digest)])

		// if the node has received 2f messages (itself included), and it haven't committed, then it commit
		p.lock.Lock()
		defer p.lock.Unlock()
		if uint64(cnt) >= 2*p.malicious_nums+1 && !p.isCommitBordcast[string(pmsg.Digest)] {
			p.pl.Plog.Printf("S%dN%d : is going to commit\n", p.ShardID, p.NodeID)
			// generate commit and broadcast
			c := message.Commit{
				Digest:     pmsg.Digest,
				SeqID:      pmsg.SeqID,
				SenderNode: p.RunningNode,
			}
			commitByte, err := json.Marshal(c)
			if err != nil {
				log.Panic()
			}
			msg_send := message.MergeMessage(message.CCommit, commitByte)
			networks.Broadcast(p.RunningNode.IPaddr, p.getNeighborNodes(), msg_send)
			networks.TcpDial(msg_send, p.RunningNode.IPaddr)
			p.isCommitBordcast[string(pmsg.Digest)] = true
			p.pl.Plog.Printf("S%dN%d : commit is broadcast\n", p.ShardID, p.NodeID)

			p.pbftStage.Add(1)
		}
	}
}

// Handle commit messages here.
// If you want to do more operations in the commit stage, you can implement the interface "ExtraOpInConsensus",
// and call the function: **ExtraOpInConsensus.HandleinCommit**
func (p *PbftConsensusNode) handleCommit(content []byte) {
	// decode the message
	cmsg := new(message.Commit)
	err := json.Unmarshal(content, cmsg)
	if err != nil {
		log.Panic(err)
	}

	curView := p.view.Load()
	p.pbftLock.Lock()
	defer p.pbftLock.Unlock()
	for p.pbftStage.Load() < 3 && cmsg.SeqID >= p.sequenceID && p.view.Load() == curView {
		p.conditionalVarpbftLock.Wait()
	}
	defer p.conditionalVarpbftLock.Broadcast()

	if cmsg.SeqID < p.sequenceID || p.view.Load() != curView {
		return
	}

	p.pl.Plog.Printf("S%dN%d received the Commit from ...%d\n", p.ShardID, p.NodeID, cmsg.SenderNode.NodeID)
	p.set2DMap(false, string(cmsg.Digest), cmsg.SenderNode)
	cnt := len(p.cntCommitConfirm[string(cmsg.Digest)])

	p.lock.Lock()
	defer p.lock.Unlock()

	if uint64(cnt) >= 2*p.malicious_nums+1 && !p.isReply[string(cmsg.Digest)] {
		p.pl.Plog.Printf("S%dN%d : has received 2f + 1 commits ... \n", p.ShardID, p.NodeID)
		// if this node is left behind, so it need to requst blocks
		if _, ok := p.requestPool[string(cmsg.Digest)]; !ok {
			p.isReply[string(cmsg.Digest)] = true
			p.askForLock.Lock()
			// request the block
			sn := &shard.Node{
				NodeID:  uint64(p.view.Load()),
				ShardID: p.ShardID,
				IPaddr:  p.ip_nodeTable[p.ShardID][uint64(p.view.Load())],
			}
			orequest := message.RequestOldMessage{
				SeqStartHeight: p.sequenceID + 1,
				SeqEndHeight:   cmsg.SeqID,
				ServerNode:     sn,
				SenderNode:     p.RunningNode,
			}
			bromyte, err := json.Marshal(orequest)
			if err != nil {
				log.Panic()
			}

			p.pl.Plog.Printf("S%dN%d : is now requesting message (seq %d to %d) ... \n", p.ShardID, p.NodeID, orequest.SeqStartHeight, orequest.SeqEndHeight)
			msg_send := message.MergeMessage(message.CRequestOldrequest, bromyte)
			networks.TcpDial(msg_send, orequest.ServerNode.IPaddr)
		} else {
			// implement interface
			p.ihm.HandleinCommit(cmsg)
			p.isReply[string(cmsg.Digest)] = true
			p.pl.Plog.Printf("S%dN%d: this round of pbft %d is end \n", p.ShardID, p.NodeID, p.sequenceID)
			p.sequenceID += 1
		}

		p.pbftStage.Store(1)
		p.lastCommitTime.Store(time.Now().UnixMilli())

		// if this node is a main node, then unlock the sequencelock
		if p.NodeID == uint64(p.view.Load()) {
			p.sequenceLock.Unlock()
			p.pl.Plog.Printf("S%dN%d get sequenceLock unlocked...\n", p.ShardID, p.NodeID)
		}
	}
}

// this func is only invoked by the main node,
// if the request is correct, the main node will send
// block back to the message sender.
// now this function can send both block and partition
func (p *PbftConsensusNode) handleRequestOldSeq(content []byte) {
	if uint64(p.view.Load()) != p.NodeID {
		return
	}

	rom := new(message.RequestOldMessage)
	err := json.Unmarshal(content, rom)
	if err != nil {
		log.Panic()
	}
	p.pl.Plog.Printf("S%dN%d : received the old message requst from ...", p.ShardID, p.NodeID)
	rom.SenderNode.PrintNode()

	oldR := make([]*message.Request, 0)
	for height := rom.SeqStartHeight; height <= rom.SeqEndHeight; height++ {
		if _, ok := p.height2Digest[height]; !ok {
			p.pl.Plog.Printf("S%dN%d : has no this digest to this height %d\n", p.ShardID, p.NodeID, height)
			break
		}
		if r, ok := p.requestPool[p.height2Digest[height]]; !ok {
			p.pl.Plog.Printf("S%dN%d : has no this message to this digest %d\n", p.ShardID, p.NodeID, height)
			break
		} else {
			oldR = append(oldR, r)
		}
	}
	p.pl.Plog.Printf("S%dN%d : has generated the message to be sent\n", p.ShardID, p.NodeID)

	p.ihm.HandleReqestforOldSeq(rom)

	// send the block back
	sb := message.SendOldMessage{
		SeqStartHeight: rom.SeqStartHeight,
		SeqEndHeight:   rom.SeqEndHeight,
		OldRequest:     oldR,
		SenderNode:     p.RunningNode,
	}
	sbByte, err := json.Marshal(sb)
	if err != nil {
		log.Panic()
	}
	msg_send := message.MergeMessage(message.CSendOldrequest, sbByte)
	networks.TcpDial(msg_send, rom.SenderNode.IPaddr)
	p.pl.Plog.Printf("S%dN%d : send blocks\n", p.ShardID, p.NodeID)
}

// node requst blocks and receive blocks from the main node
func (p *PbftConsensusNode) handleSendOldSeq(content []byte) {
	som := new(message.SendOldMessage)
	err := json.Unmarshal(content, som)
	if err != nil {
		log.Panic()
	}
	p.pl.Plog.Printf("S%dN%d : has received the SendOldMessage message\n", p.ShardID, p.NodeID)

	// implement interface for new consensus
	p.ihm.HandleforSequentialRequest(som)
	beginSeq := som.SeqStartHeight
	for idx, r := range som.OldRequest {
		p.requestPool[string(getDigest(r))] = r
		p.height2Digest[uint64(idx)+beginSeq] = string(getDigest(r))
		p.isReply[string(getDigest(r))] = true
		p.pl.Plog.Printf("this round of pbft %d is end \n", uint64(idx)+beginSeq)
	}
	p.sequenceID = som.SeqEndHeight + 1
	if rDigest, ok1 := p.height2Digest[p.sequenceID]; ok1 {
		if r, ok2 := p.requestPool[rDigest]; ok2 {
			ppmsg := &message.PrePrepare{
				RequestMsg: r,
				SeqID:      p.sequenceID,
				Digest:     getDigest(r),
			}
			flag := false
			flag = p.ihm.HandleinPrePrepare(ppmsg)
			if flag {
				pre := message.Prepare{
					Digest:     ppmsg.Digest,
					SeqID:      ppmsg.SeqID,
					SenderNode: p.RunningNode,
				}
				prepareByte, err := json.Marshal(pre)
				if err != nil {
					log.Panic()
				}
				// broadcast
				msg_send := message.MergeMessage(message.CPrepare, prepareByte)
				networks.Broadcast(p.RunningNode.IPaddr, p.getNeighborNodes(), msg_send)
				p.pl.Plog.Printf("S%dN%d : has broadcast the prepare message \n", p.ShardID, p.NodeID)
			}
		}
	}

	p.askForLock.Unlock()
}
