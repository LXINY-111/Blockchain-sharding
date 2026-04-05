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
LR = 0.002
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

def compute_reward(cstx_ratios):
    # 🌟 新增：计算奖励
    # 目标是降低跨片率，所以当前跨片率均值的负数就是奖励（跨片率越高，惩罚越狠）
    avg_cstx = sum(cstx_ratios) / len(cstx_ratios) if cstx_ratios else 0.75
    return -avg_cstx * 10.0 # 放大信号，加速收敛

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
    max_load = max(loads) + 1e-5
    norm_loads = [x / max_load for x in loads]
    cstx_ratios = data.get("shard_cstx_ratios", [])
    global_state = norm_loads + cstx_ratios
    num_shards = len(loads) if loads else 4
    
    state_dim = len(global_state) + num_shards + 1
    action_dim = num_shards 
    
    model, optimizer = load_model(state_dim, action_dim)
    
    # 🌟 1. 加载持久化经验池
    if os.path.exists(MEMORY_PATH):
        try:
            memory = torch.load(MEMORY_PATH)
        except:
            memory = {'states': [], 'actions': [], 'logprobs': [], 'rewards': []}
    else:
        memory = {'states': [], 'actions': [], 'logprobs': [], 'rewards': []}

    # 🌟 2. 结算上一轮的 Reward (延迟反馈)
    # 当前拿到的 state 其实是上一轮 action 执行后的结果
    unrewarded_count = len(memory['states']) - len(memory['rewards'])
    if unrewarded_count > 0:
        step_reward = compute_reward(cstx_ratios)
        memory['rewards'].extend([step_reward] * unrewarded_count)
        print(f"[AERO PPO] 结算 Epoch {args.epoch-1} 的行动得分: Reward = {step_reward:.4f}")

    # 🌟 3. 触发 PPO 训练 (攒够一定经验后更新网络)
    if len(memory['rewards']) >= 60:  # 约经历 3~4 个 Epoch
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
    sorted_candidates = sorted(candidates, key=lambda x: x['cstx_volume'], reverse=True)
    
    # =========================================================
    # ✨✨✨ 核心创新点 2.0：自适应动态步长 (Adaptive Step-Size) ✨✨✨
    # 计算当前的全局平均跨片率
    current_avg_cstx = sum(cstx_ratios) / len(cstx_ratios) if len(cstx_ratios) > 0 else 0.75
    
    # 动态决定迁移规模 (重病下猛药，轻症微调)
    if current_avg_cstx > 0.68:
        dynamic_limit = 20  # 早期混乱阶段：放开限制，加速收敛
        print(f"[AERO 策略切换] 当前跨片率 {current_avg_cstx*100:.2f}% > 68% | 启用【宏观大迁徙】(Limit: 20)")
    elif current_avg_cstx > 0.63:
        dynamic_limit = 8   # 中期过渡阶段：中度调整
        print(f"[AERO 策略切换] 当前跨片率 {current_avg_cstx*100:.2f}% | 启用【平滑过渡】(Limit: 8)")
    else:
        dynamic_limit = 3   # 逼近基线：微创手术，防止震荡
        print(f"[AERO 策略切换] 当前跨片率 {current_avg_cstx*100:.2f}% 逼近红线 | 启用【微创手术】(Limit: 3)")
        
    for prefix_data in sorted_candidates[:dynamic_limit]:
    # =========================================================
    
        current_shard_onehot = [0] * num_shards
        if prefix_data['current_shard'] < num_shards:
            current_shard_onehot[prefix_data['current_shard']] = 1
            
        tx_vol_norm = [prefix_data['tx_volume'] / 1000.0] 
        input_vec = global_state + current_shard_onehot + tx_vol_norm
        state_tensor = torch.FloatTensor(input_vec).to("cpu")
        
        # 采样动作
        with torch.no_grad():
            action_probs = torch.nn.functional.softmax(model.actor(state_tensor), dim=-1)
            dist = torch.distributions.Categorical(action_probs)
            action = dist.sample()
            logprob = dist.log_prob(action)
        
        target_shard = action.item()

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
    if len(migrations) == 0 and len(candidates) > 0:
        import random
        victim = random.choice(candidates)
        forced_shard = (victim['current_shard'] + 1) % num_shards
        migrations.append({"prefix": victim['prefix'], "to_shard": forced_shard})

    # 🌟 将经验池写回硬盘，供下一次进程唤醒时使用
    torch.save(memory, MEMORY_PATH)

    output_path = f"aero_io/action_{args.epoch}.json"
    with open(output_path, 'w') as f:
        json.dump({"migrations": migrations}, f, indent=2)

    print("[DEBUG] Python script finished writing file. Exiting now...")

if __name__ == "__main__":
    main()