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
MEMORY_PATH = "aero_io/ppo_memory.pt"  # 用于存储跨 Epoch 的经验池

# --- PPO 核心超参数 ---
LR = 0.00005
GAMMA = 0.99
K_EPOCHS = 2
EPS_CLIP = 0.2

# --- local reward shaping 超参数 ---
# 目标：
# 1. 保留论文的全局目标（global CSTX + load variance）
# 2. 给每个 prefix 的动作补一个局部引导信号
# 3. 不改 PPO 结构，只改 aero_agent.py
EPS = 1e-8
LOCAL_REWARD_ALPHA = 5.0
LOCAL_ALIGN_W = 1.6
LOCAL_IMPROVE_W = 0.8
LOCAL_ZERO_EDGE_W = 0.8
NEAR_BEST_RATIO = 0.5


def load_model(state_dim, action_dim):
    device = torch.device("cpu")
    model = ActorCritic(state_dim, action_dim).to(device)

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


def compute_global_reward(global_cstx_ratio, norm_loads):
    """
    全局奖励：
    保留你当前 baseline 的目标不动：
        reward = -50 * global_cstx - 10 * load_variance
    并保留 40% 负载红线惩罚
    """
    global_cstx = global_cstx_ratio if global_cstx_ratio is not None else 0.75

    # 计算真实的“全局负载占比”
    sum_nl = sum(norm_loads) + 1e-5
    load_proportions = [nl / sum_nl for nl in norm_loads]

    # 基于真实占比的方差
    load_variance = np.var(load_proportions) if load_proportions else 0.0

    # 保留你当前的全局奖励形式
    reward = -50.0 * global_cstx - 10.0 * load_variance

    # 40% “死亡红线”轻惩罚
    if len(load_proportions) > 0 and max(load_proportions) > 0.4:
        reward -= 5.0 * max(load_proportions)
        print(f"[AERO PENALTY] 触发死亡惩罚！当前最大负载占比已超标: {max(load_proportions):.2f}")

    return reward


def compute_local_prefix_reward(raw_edges, current_shard, target_shard):
    """
    折中版 local reward：
    - best / tie-best: 强正奖励
    - 0-edge 且存在正边目标: 强负奖励
    - 次优但接近 best: 轻微负分或接近 0
    - 明显比 current 更差: 中等负分
    目标：
    既不回到“错动作也奖励”，也避免“几乎所有非 best 都重罚”。
    """
    edges = np.asarray(raw_edges, dtype=np.float32)
    if edges.size == 0:
        return 0.0

    max_edge = float(np.max(edges))
    if max_edge <= 0.0:
        return 0.0

    best_shards = [i for i, e in enumerate(edges) if e == max_edge]

    chosen_edges = float(edges[target_shard]) if 0 <= target_shard < len(edges) else 0.0
    current_edges = float(edges[current_shard]) if 0 <= current_shard < len(edges) else 0.0

    # 1) 选到最优（含并列最优）
    if target_shard in best_shards:
        return 1.2 if current_shard in best_shards else 2.0

    # 2) 明明有正收益目标，却选到 0-edge
    if chosen_edges <= 0.0 and max_edge > 0.0:
        return -2.0

    # 3) 不是 best，但“接近 best”
    ratio = chosen_edges / (max_edge + 1e-8)

    # 接近 best（>=85%）: 只给很轻的负分
    if ratio >= 0.85:
        return -0.1

    # 中度接近（>=65%）: 轻负分
    if ratio >= 0.65:
        return -0.3

    # 4) 比当前更好，但仍离 best 较远
    if chosen_edges > current_edges:
        return -0.2

    # 5) 和当前一样
    if chosen_edges == current_edges:
        return -0.4

    # 6) 比当前更差
    gap = (current_edges - chosen_edges) / (max_edge + 1e-8)
    return float(np.clip(-0.8 - 0.8 * gap, -1.6, -0.8))


def build_action_mask(raw_edges, num_shards, near_best_ratio=NEAR_BEST_RATIO):
    """
    构造动作 mask：
    1) 先禁止 0-edge shard
    2) 再做“近优动作裁剪”：
       只保留 edges >= near_best_ratio * max_edge 的 shard
    3) 如果裁剪后意外全空，则退回到“只禁止 0-edge”
    4) 如果所有 shard 的 edges 都是 0，则放开所有动作，避免 mask 全 0
    """
    edges = np.asarray(raw_edges, dtype=np.float32)

    if edges.size == 0:
        return torch.ones(num_shards, dtype=torch.float32)

    max_edge = float(np.max(edges))
    if max_edge <= 0.0:
        return torch.ones(num_shards, dtype=torch.float32)

    # 先只保留正边
    positive_mask = (edges > 0)

    # 再做近优裁剪：只保留“接近最优”的正边 shard
    clipped_mask = positive_mask & (edges >= near_best_ratio * max_edge)

    if np.any(clipped_mask):
        return torch.tensor(clipped_mask.astype(np.float32), dtype=torch.float32)

    # 如果裁剪过严导致空了，退回到“只禁止 0-edge”
    if np.any(positive_mask):
        return torch.tensor(positive_mask.astype(np.float32), dtype=torch.float32)

    return torch.ones(num_shards, dtype=torch.float32)


def masked_action_distribution(logits, action_mask):
    """
    对 actor 输出加 mask，屏蔽非法动作。
    logits: shape [action_dim]
    action_mask: shape [action_dim], 0/1
    """
    # 安全兜底：如果 mask 意外全 0，则放开全部动作
    if torch.sum(action_mask) <= 0:
        action_mask = torch.ones_like(action_mask)

    masked_logits = logits.clone()
    masked_logits[action_mask <= 0] = -1e9

    action_probs = torch.softmax(masked_logits, dim=-1)
    dist = torch.distributions.Categorical(action_probs)
    return dist, action_probs


def update_ppo(model, optimizer, memory):
    if len(memory['states']) == 0:
        return

    old_states = torch.stack(memory['states']).detach()
    old_actions = torch.stack(memory['actions']).detach()
    old_logprobs = torch.stack(memory['logprobs']).detach()
    old_action_masks = torch.stack(memory['action_masks']).detach()
    rewards = memory['rewards']

    # 奖励归一化，稳定训练
    rewards = torch.tensor(rewards, dtype=torch.float32)
    rewards = (rewards - rewards.mean()) / (rewards.std() + 1e-7)

    for _ in range(K_EPOCHS):
        # ===== 用 masked policy 重新计算 logprob / entropy =====
        logits = model.actor(old_states)  # [B, action_dim]

        masked_logits = logits.clone()
        masked_logits[old_action_masks <= 0] = -1e9

        action_probs = torch.softmax(masked_logits, dim=-1)
        dist = torch.distributions.Categorical(action_probs)

        logprobs = dist.log_prob(old_actions)
        dist_entropy = dist.entropy()

        state_values = torch.squeeze(model.critic(old_states))

        ratios = torch.exp(logprobs - old_logprobs)
        advantages = rewards - state_values.detach()

        surr1 = ratios * advantages
        surr2 = torch.clamp(ratios, 1 - EPS_CLIP, 1 + EPS_CLIP) * advantages

        loss = (
            -torch.min(surr1, surr2)
            + 0.5 * nn.MSELoss()(state_values, rewards)
            - 0.003 * dist_entropy
        )

        optimizer.zero_grad()
        loss.mean().backward()
        optimizer.step()

    print(f"[AERO PPO TRAIN] 神经网络完成了一次进化! 当前 Loss: {loss.mean().item():.4f}")


def main():
    # False：训练阶段必须用 RL 自己的动作
    # True：只适合做 heuristic 对照，不适合真正训练
    USE_HEURISTIC_BASELINE = False

    parser = argparse.ArgumentParser()
    parser.add_argument("--epoch", type=int, required=True)
    args = parser.parse_args()

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

    # 读取上一个 Epoch 的状态，构建简单时序依赖
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
        # Epoch 0 或上一轮状态缺失
        prev_norm_loads = [0] * num_shards
        prev_cstx = [0] * num_shards

    # 当前全局状态 = 当前负载 + 当前 cstx + 上一轮负载 + 上一轮 cstx
    global_state = norm_loads + cstx_ratios + prev_norm_loads + prev_cstx

    # 输入维度：
    # global_state
    # + current_shard_onehot(num_shards)
    # + tx_volume(1)
    # + edges_distribution(num_shards)
    state_dim = len(global_state) + num_shards + 1 + num_shards
    action_dim = num_shards

    model, optimizer = load_model(state_dim, action_dim)

    # 1) 加载持久化经验池
    if os.path.exists(MEMORY_PATH):
        try:
            memory = torch.load(MEMORY_PATH, weights_only=False)
        except Exception as e:
            print(f"[FATAL ERROR] 经验池加载失败，被清空！原因: {e}")
            memory = {
                'states': [],
                'actions': [],
                'logprobs': [],
                'rewards': [],
                'local_rewards': [],
                'action_masks': []
            }
    else:
        memory = {
            'states': [],
            'actions': [],
            'logprobs': [],
            'rewards': [],
            'local_rewards': [],
            'action_masks': []
        }

    # 兼容旧版本 memory：如果没有字段，补上
    memory.setdefault('local_rewards', [])
    memory.setdefault('action_masks', [])

    # 兼容旧 memory 文件，保证长度不炸
    if len(memory['local_rewards']) < len(memory['states']):
        memory['local_rewards'].extend([0.0] * (len(memory['states']) - len(memory['local_rewards'])))
    elif len(memory['local_rewards']) > len(memory['states']):
        memory['local_rewards'] = memory['local_rewards'][:len(memory['states'])]

    if len(memory['action_masks']) < len(memory['states']):
        for _ in range(len(memory['states']) - len(memory['action_masks'])):
            memory['action_masks'].append(torch.ones(action_dim, dtype=torch.float32))
    elif len(memory['action_masks']) > len(memory['states']):
        memory['action_masks'] = memory['action_masks'][:len(memory['states'])]

    # 保证 action_masks 内部元素都是 tensor
    normalized_masks = []
    for mask in memory['action_masks']:
        if isinstance(mask, torch.Tensor):
            normalized_masks.append(mask.float())
        else:
            normalized_masks.append(torch.tensor(mask, dtype=torch.float32))
    memory['action_masks'] = normalized_masks

    # 2) 结算上一轮的 Reward（延迟反馈）
    # 当前拿到的 state，是上一轮 action 执行后的结果
    global_cstx_ratio = data.get("global_cstx_ratio", None)
    unrewarded_count = len(memory['states']) - len(memory['rewards'])

    avg_shard_cstx = sum(cstx_ratios) / len(cstx_ratios) if cstx_ratios else 0.0
    print(f"[AERO REWARD] global_cstx={float(global_cstx_ratio or 0.0):.4f}, avg_shard_cstx={avg_shard_cstx:.4f}, loads={norm_loads}")

    if unrewarded_count > 0:
        global_reward = compute_global_reward(global_cstx_ratio, norm_loads)

        # 只给“上一轮还没发工资”的那些动作结算 reward
        start_idx = len(memory['rewards'])
        pending_local_rewards = memory['local_rewards'][start_idx:start_idx + unrewarded_count]

        # 每个 prefix 单独获得
        # final_reward = global_reward + alpha * local_reward
        step_rewards = [
            global_reward + LOCAL_REWARD_ALPHA * float(local_r)
            for local_r in pending_local_rewards
        ]

        memory['rewards'].extend(step_rewards)

        local_mean = float(np.mean(pending_local_rewards)) if pending_local_rewards else 0.0
        final_mean = float(np.mean(step_rewards)) if step_rewards else global_reward

        print(
            f"[AERO PPO] 结算 Epoch {args.epoch-1} 的行动得分: "
            f"global={global_reward:.4f}, "
            f"local_mean={local_mean:.4f}, "
            f"final_mean={final_mean:.4f}"
        )

    # 3) 触发 PPO 训练
    # MAX_SEQ_LEN=5 时，40 条经验大约对应 8 个 epoch 再更新一次
    if len(memory['rewards']) >= 40:
        update_ppo(model, optimizer, memory)

        memory = {
            'states': [],
            'actions': [],
            'logprobs': [],
            'rewards': [],
            'local_rewards': [],
            'action_masks': []
        }

        torch.save({
            'model_state_dict': model.state_dict(),
            'optimizer_state_dict': optimizer.state_dict()
        }, MODEL_PATH)

    # 4) 开始为当前 Epoch 制定决策
    candidates = data.get("candidate_prefixes", [])
    migrations = []

    # 仍按跨片交易量排序，优先考虑最需要迁移的 prefix
    sorted_candidates = sorted(candidates, key=lambda x: x['cstx_volume'], reverse=True)

    # 固定动作序列长度，不引入 STOP / decoder
    MAX_SEQ_LEN = 5

    for prefix_data in sorted_candidates[:MAX_SEQ_LEN]:
        current_shard_onehot = [0] * num_shards
        if prefix_data['current_shard'] < num_shards:
            current_shard_onehot[prefix_data['current_shard']] = 1

        tx_vol_norm = [prefix_data['tx_volume'] / 1000.0]

        # 提取并归一化图表示特征
        raw_edges = prefix_data.get('edges_to_shard', [0] * num_shards)
        total_edges = sum(raw_edges)
        edges_distribution = [e / total_edges for e in raw_edges] if total_edges > 0 else [0.0] * num_shards

        # heuristic baseline：边最多的 shard
        best_shard = int(np.argmax(raw_edges)) if sum(raw_edges) > 0 else prefix_data['current_shard']

        # 输入状态向量
        input_vec = global_state + current_shard_onehot + tx_vol_norm + edges_distribution
        state_tensor = torch.FloatTensor(input_vec).to("cpu")

        # 构造 action mask：
        # 1) 禁止 0-edge
        # 2) 只保留“接近 best”的合法动作
        action_mask = build_action_mask(raw_edges, num_shards)

        # ===== 单正边直接选 =====
        # 注意：这里看的是“原始正边数”，不是裁剪后的 mask
        # 如果 raw_edges 里只有一个 shard 的 edges > 0，那么这个动作没有探索必要，直接选它
        positive_indices = [i for i, e in enumerate(raw_edges) if e > 0]

        with torch.no_grad():
            if len(positive_indices) == 1:
                forced_shard = positive_indices[0]

                action_probs = torch.zeros(num_shards, dtype=torch.float32)
                action_probs[forced_shard] = 1.0

                action = torch.tensor(forced_shard, dtype=torch.long)
                logprob = torch.tensor(0.0, dtype=torch.float32)

                print(
                    f"[AERO FORCE] prefix={prefix_data['prefix']} "
                    f"edges={raw_edges} "
                    f"forced_shard={forced_shard}"
                )
            else:
                logits = model.actor(state_tensor)
                dist, action_probs = masked_action_distribution(
                    logits=logits,
                    action_mask=action_mask.to(logits.device)
                    )
                action = dist.sample()
                logprob = dist.log_prob(action)

        print(
            f"[AERO MASK] prefix={prefix_data['prefix']} "
            f"edges={raw_edges} "
            f"mask={action_mask.tolist()} "
            f"probs={action_probs.tolist()}"
        )

        rl_target_shard = action.item()

        # True = heuristic 直接接管动作
        # False = 用 RL 动作训练
        target_shard = best_shard if USE_HEURISTIC_BASELINE else rl_target_shard

        # 计算 prefix 级局部 reward
        local_reward = compute_local_prefix_reward(
            raw_edges=raw_edges,
            current_shard=prefix_data['current_shard'],
            target_shard=target_shard
        )

        # 动作正确性诊断
        # 注意：可能存在多个并列最优 shard，不能只用 argmax 的第一个位置判断对错
        if len(raw_edges) > 0:
            max_edge = max(raw_edges)
            best_shards = [i for i, e in enumerate(raw_edges) if e == max_edge]
        else:
            max_edge = 0
            best_shards = []

        is_correct = (target_shard in best_shards)

        print(
            f"[AERO CHECK] prefix={prefix_data['prefix']} "
            f"cur={prefix_data['current_shard']} "
            f"choose={target_shard} "
            f"best={best_shards} "
            f"edges={raw_edges} "
            f"local_r={local_reward:.3f} "
            f"{'✅' if is_correct else '❌'}"
        )

        chosen_gain = raw_edges[target_shard] if target_shard < len(raw_edges) else -1
        best_gain = raw_edges[best_shard] if best_shard < len(raw_edges) else -1
        print(
            f"[AERO GAIN] prefix={prefix_data['prefix']} "
            f"chosen_gain={chosen_gain} best_gain={best_gain}"
        )

        # 记录动作到经验池（奖励在下一轮到账）
        memory['states'].append(state_tensor)
        memory['actions'].append(action)
        memory['logprobs'].append(logprob)
        memory['local_rewards'].append(local_reward)
        memory['action_masks'].append(action_mask.clone())

        if target_shard != prefix_data['current_shard']:
            migrations.append({
                "prefix": prefix_data['prefix'],
                "to_shard": target_shard
            })

        print(f"[AERO PPO] Prefix {prefix_data['prefix']} ({prefix_data['current_shard']}) -> {target_shard}")

    # 将经验池写回硬盘，供下一次进程唤醒时使用
    torch.save(memory, MEMORY_PATH)

    output_path = f"aero_io/action_{args.epoch}.json"
    with open(output_path, 'w') as f:
        json.dump({"migrations": migrations}, f, indent=2)

    print("[DEBUG] Python script finished writing file. Exiting now...")


if __name__ == "__main__":
    main()