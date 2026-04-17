import json
import argparse
import os
import torch
import torch.nn as nn
import torch.optim as optim
import numpy as np
from ppo import ActorCritic
import sys

MODEL_PATH = "aero/aero_model.pth"
MEMORY_PATH = "aero_io/ppo_memory.pt" # 新增：用于存储跨 Epoch 的经验池

# --- PPO 核心超参数 ---
LR = 0.0001  # 从 0.002 降低至 0.0001
GAMMA = 0.99
K_EPOCHS = 4
EPS_CLIP = 0.2

def load_model(state_dim, action_dim):
    device = torch.device("cpu") 
    model = ActorCritic(state_dim, action_dim).to(device)
    
    # 🌟 新增：优化器
    optimizer = optim.Adam([
        {'params': model.actor.parameters(), 'lr': LR},
        {'params': model.critic.parameters(), 'lr': LR}
    ])
    
    if os.path.exists(MODEL_PATH):
        try:
            checkpoint = torch.load(MODEL_PATH, map_location=device)
            model.load_state_dict(checkpoint['model_state_dict'])
            optimizer.load_state_dict(checkpoint['optimizer_state_dict'])
        except Exception as e:
            print(f"[AERO ERROR] 模型加载失败, 使用随机初始化: {e}")
    return model, optimizer

# 计算奖励，增加 norm_loads 参数
def compute_reward(global_cstx_ratio, norm_loads):
    global_cstx = global_cstx_ratio if global_cstx_ratio is not None else 0.75
    # 🌟 修复 1：计算真实的“全局负载占比” (每个分片占全网总交易的比例)
    sum_nl = sum(norm_loads) + 1e-5
    load_proportions = [nl / sum_nl for nl in norm_loads]

    # 计算基于真实占比的负载方差
    load_variance = np.var(load_proportions) if load_proportions else 0.0
    
    # =========================================================
    # 终极 Baseline 调参：权重反转！
    # 既然有 40% 的死亡红线保底，我们大幅降低方差惩罚 (-10.0)
    # 并将跨片率的惩罚激增至 -50.0！
    # 逼迫 AI 在“不触发死亡红线”的前提下，拼命去降低跨片率。
    # =========================================================
    reward = -50.0 * global_cstx - 10.0 * load_variance

    # 🌟 修复 2：用真实的占比去触发 40% 的“死亡惩罚”红线
    if len(load_proportions) > 0 and max(load_proportions) > 0.4:
        #加一个更平滑的轻惩罚
        reward -= 5.0 * max(load_proportions)
        #reward -= 50.0  
        print(f"[AERO PENALTY] 🚨 触发死亡惩罚！当前最大负载占比已超标: {max(load_proportions):.2f}")
        
    # 🌟 修复 3：绝对不能漏了 return！！否则 PPO 拿不到奖励会直接闪退！
    return reward

def update_ppo(model, optimizer, memory):
    if len(memory['states']) == 0: return

    old_states = torch.stack(memory['states']).detach()
    old_actions = torch.stack(memory['actions']).detach()
    old_logprobs = torch.stack(memory['logprobs']).detach()
    rewards = memory['rewards']

    # 奖励归一化技巧，稳定训练
    rewards = torch.tensor(rewards, dtype=torch.float32)
    rewards = (rewards - rewards.mean()) / (rewards.std() + 1e-7)

    for _ in range(K_EPOCHS):
        # 使用 ppo.py 中的 evaluate 评估当前状态
        logprobs, state_values, dist_entropy = model.evaluate(old_states, old_actions)
        state_values = torch.squeeze(state_values)
        
        ratios = torch.exp(logprobs - old_logprobs)
        advantages = rewards - state_values.detach()   
        
        surr1 = ratios * advantages
        surr2 = torch.clamp(ratios, 1 - EPS_CLIP, 1 + EPS_CLIP) * advantages

        loss = -torch.min(surr1, surr2) + 0.5 * nn.MSELoss()(state_values, rewards) - 0.01 * dist_entropy
        
        optimizer.zero_grad()
        loss.mean().backward()
        optimizer.step()
        
    print(f"🚀 [AERO PPO TRAIN] 神经网络完成了一次进化! 当前 Loss: {loss.mean().item():.4f}")

def main():
    #加一个总开关，是否用图的边中最大的数
    USE_HEURISTIC_BASELINE = False

    parser = argparse.ArgumentParser()
    parser.add_argument("--epoch", type=int, required=True)
    args = parser.parse_args()

    # ⚠️ 删除了原来的 random seed 固定逻辑！
    # 强化学习需要探索，如果每个 epoch 都重置同样的种子，模型就成了只会按剧本走的死板机器。

    state_file = f"aero_io/state_{args.epoch}.json"
    if not os.path.exists(state_file):
        print(f"Error: {state_file} not found")
        return
    
    with open(state_file, 'r') as f:
        data = json.load(f)

    loads = data.get("shard_loads", [])
    num_shards = len(loads) if loads else 4
    max_load = max(loads) + 1e-5 if loads else 1.0
    norm_loads = [x / max_load for x in loads]
    cstx_ratios = data.get("shard_cstx_ratios", [])
    
    # ✨ 新增：读取上一个 Epoch 的状态，构建时序依赖 ✨
    prev_state_file = f"aero_io/state_{args.epoch-1}.json"
    if os.path.exists(prev_state_file):
        with open(prev_state_file, 'r') as f:
            prev_data = json.load(f)
        prev_loads = prev_data.get("shard_loads", [])
        if prev_loads:
            prev_max_load = max(prev_loads) + 1e-5
            prev_norm_loads = [x / prev_max_load for x in prev_loads]
        else:
            prev_norm_loads = [0] * num_shards
            
        prev_cstx = prev_data.get("shard_cstx_ratios", [])
        if not prev_cstx:
            prev_cstx = [0] * num_shards
    else:
        # 如果是 Epoch 0，或者上一轮状态文件丢失，用零填充
        prev_norm_loads = [0] * num_shards
        prev_cstx = [0] * num_shards
        
    # 将当前的 global_state 扩大，融入历史信息 (长度翻倍)
    global_state = norm_loads + cstx_ratios + prev_norm_loads + prev_cstx
    
    # 这里的 state_dim 会自动根据变长后的 global_state 计算出正确的输入维度
    state_dim = len(global_state) + num_shards + 1 + num_shards
    action_dim = num_shards 
    
    model, optimizer = load_model(state_dim, action_dim)
    
    # 🌟 1. 加载持久化经验池
    if os.path.exists(MEMORY_PATH):
        try:
            # 【修复 2】：关闭 weights_only 限制
            memory = torch.load(MEMORY_PATH, weights_only=False)
        except Exception as e:
            # 【修复 3】：绝对不要用空的 except 吞掉报错！
            print(f"[FATAL ERROR] 经验池加载失败，被清空！原因: {e}")
            memory = {'states': [], 'actions': [], 'logprobs': [], 'rewards': []}
    else:
        memory = {'states': [], 'actions': [], 'logprobs': [], 'rewards': []}

    # 🌟 2. 结算上一轮的 Reward (延迟反馈)
    # 当前拿到的 state 其实是上一轮 action 执行后的结果
    global_cstx_ratio = data.get("global_cstx_ratio", None)
    unrewarded_count = len(memory['states']) - len(memory['rewards'])
    #在 reward 结算前打印
    avg_shard_cstx = sum(cstx_ratios) / len(cstx_ratios) if cstx_ratios else 0.0
    print(f"[AERO REWARD] global_cstx={float(global_cstx_ratio or 0.0):.4f}, avg_shard_cstx={avg_shard_cstx:.4f}, loads={norm_loads}")

    if unrewarded_count > 0:
        step_reward = compute_reward(global_cstx_ratio,norm_loads)
        memory['rewards'].extend([step_reward] * unrewarded_count)
        print(f"[AERO PPO] 结算 Epoch {args.epoch-1} 的行动得分: Reward = {step_reward:.4f}")

    # 🌟 3. 触发 PPO 训练 (攒够一定经验后更新网络)60 修改为 50，对应经验池的更新阈值
    if len(memory['rewards']) >= 50:  # 约经历 3~4 个 Epoch
        update_ppo(model, optimizer, memory)
        memory = {'states': [], 'actions': [], 'logprobs': [], 'rewards': []} # 清空池子
        
        # 保存真正被训练过的模型！
        torch.save({
            'model_state_dict': model.state_dict(),
            'optimizer_state_dict': optimizer.state_dict()
        }, MODEL_PATH)

    

    # 4. 开始为当前 Epoch 制定决策
    candidates = data.get("candidate_prefixes", [])
    migrations = []
    # 依然按跨片交易量排序，把最需要迁移的账户排在前面
    sorted_candidates = sorted(candidates, key=lambda x: x['cstx_volume'], reverse=True)
    
    # =========================================================
    # 严格对齐论文：移除人为干预的动态步长，启用恒定的防震荡限流阀
    # 让 PPO 在固定的动作空间边界内自己摸索最优解
    MAX_SEQ_LEN = 3
    
    for prefix_data in sorted_candidates[:MAX_SEQ_LEN]:
    # =========================================================
    
        current_shard_onehot = [0] * num_shards
        if prefix_data['current_shard'] < num_shards:
            current_shard_onehot[prefix_data['current_shard']] = 1
            
        tx_vol_norm = [prefix_data['tx_volume'] / 1000.0] 
        # ✨ 新增：提取并归一化图表示特征 (引力分布)
        raw_edges = prefix_data.get('edges_to_shard', [0]*num_shards)
        total_edges = sum(raw_edges)
        # 归一化：转换为去往各个分片的概率分布，如果没有边则全是0
        edges_distribution = [e / total_edges for e in raw_edges] if total_edges > 0 else [0.0]*num_shards
        # heuristic baseline：边最多的 shard 作为“最佳目标”
        best_shard = int(np.argmax(raw_edges)) if sum(raw_edges) > 0 else prefix_data['current_shard']
        
        # ✨ 将图特征 edges_distribution 拼接到输入向量中
        input_vec = global_state + current_shard_onehot + tx_vol_norm + edges_distribution
        state_tensor = torch.FloatTensor(input_vec).to("cpu")
        
        # 采样动作
        with torch.no_grad():
            action_probs = torch.nn.functional.softmax(model.actor(state_tensor), dim=-1)
            dist = torch.distributions.Categorical(action_probs)
            action = dist.sample()
            logprob = dist.log_prob(action)
        
        #target_shard = action.item()
        rl_target_shard = action.item()
        # True = 用 heuristic 直接接管动作
        # False = 仍然用 MLP 动作，只把 heuristic 当对照
        target_shard = best_shard if USE_HEURISTIC_BASELINE else rl_target_shard
        # 动作正确性诊断
        is_correct = (target_shard == best_shard)
        print(
            f"[AERO CHECK] prefix={prefix_data['prefix']} "
            f"cur={prefix_data['current_shard']} "
            f"choose={target_shard} "
            f"best={best_shard} "
            f"edges={raw_edges} "
            f"{'✅' if is_correct else '❌'}"
        )

        chosen_gain = raw_edges[target_shard] if target_shard < len(raw_edges) else -1
        best_gain = raw_edges[best_shard] if best_shard < len(raw_edges) else -1
        print(
            f"[AERO GAIN] prefix={prefix_data['prefix']} "
            f"chosen_gain={chosen_gain} best_gain={best_gain}"
        )

        # 🌟 记录动作到经验池（等下个 Epoch 再发工资）
        memory['states'].append(state_tensor)
        memory['actions'].append(action)
        memory['logprobs'].append(logprob)

        if target_shard != prefix_data['current_shard']:
            migrations.append({"prefix": prefix_data['prefix'], "to_shard": target_shard})
            print(f"[AERO PPO] Prefix {prefix_data['prefix']} ({prefix_data['current_shard']}) -> {target_shard}")

    # =========================================================
    # 👇 下方的保底机制和写入逻辑保持原样即可 👇
    # =========================================================
    #if len(migrations) == 0 and len(candidates) > 0:
        #import random
        #victim = random.choice(candidates)
        #forced_shard = (victim['current_shard'] + 1) % num_shards
        #migrations.append({"prefix": victim['prefix'], "to_shard": forced_shard})

    # 🌟 将经验池写回硬盘，供下一次进程唤醒时使用
    torch.save(memory, MEMORY_PATH)

    output_path = f"aero_io/action_{args.epoch}.json"
    with open(output_path, 'w') as f:
        json.dump({"migrations": migrations}, f, indent=2)

    print("[DEBUG] Python script finished writing file. Exiting now...")

if __name__ == "__main__":
    main()