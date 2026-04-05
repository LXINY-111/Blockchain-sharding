import torch
import torch.nn as nn
import torch.nn.functional as F
from torch.distributions import Categorical

# 定义 Actor-Critic 神经网络结构
class ActorCritic(nn.Module):
    def __init__(self, state_dim, action_dim):
        super(ActorCritic, self).__init__()
        
        # Actor 网络：观察状态 -> 输出去哪个分片的概率
        self.actor = nn.Sequential(
            nn.Linear(state_dim, 128),
            nn.Tanh(),
            nn.Linear(128, 64),
            nn.Tanh(),
            nn.Linear(64, action_dim)
        )
        
        # Critic 网络：观察状态 -> 给当前局面打分 (训练时才用)
        self.critic = nn.Sequential(
            nn.Linear(state_dim, 128),
            nn.Tanh(),
            nn.Linear(128, 64),
            nn.Tanh(),
            nn.Linear(64, 1)
        )

    def forward(self):
        raise NotImplementedError

    # 决策函数 (推理时用)
    def act(self, state):
        # 计算动作概率
        action_probs = F.softmax(self.actor(state), dim=-1)
        # 根据概率采样动作
        dist = Categorical(action_probs)
        action = dist.sample()
        return action.item()

    # 评估函数 (训练时用)
    def evaluate(self, state, action):
        action_probs = F.softmax(self.actor(state), dim=-1)
        dist = Categorical(action_probs)
        
        action_logprobs = dist.log_prob(action)
        dist_entropy = dist.entropy()
        state_values = self.critic(state)
        
        return action_logprobs, state_values, dist_entropy