import matplotlib.pyplot as plt
import numpy as np
import csv
import os

# 配置 CSV 文件路径 
file_path = '../result/supervisor_measureOutput/CrossTransaction_ratio.csv'

# 路径防错处理
if not os.path.exists(file_path):
    file_path_alt = '../expTest/result/supervisor_measureOutput/CrossTransaction_ratio.csv'
    if os.path.exists(file_path_alt):
        file_path = file_path_alt
    else:
        print(f"找不到文件，请检查路径是否正确！")
        exit()

data = []
# 读取 CSV 数据
with open(file_path, 'r') as f:
    reader = csv.reader(f)
    # 跳过第一行的表头 (EpochID, Total tx #...)
    header = next(reader, None) 
    
    for row in reader:
        if not row:
            continue
        try:
            # 【核心修复】：只读取最后一列的数据 (CTX ratio)
            val = float(row[-1].strip())
            data.append(val * 100)
        except (ValueError, IndexError):
            continue

# 我们有 0~100 共 101 个点
epochs = min(101, len(data))
data = data[:epochs]
x_axis = np.arange(0, epochs) # 从 Epoch 0 开始画

# ==========================================
# 🌟 学术论文级图表布局优化 (AERO 原文风格)
# ==========================================
plt.rcParams.update({
    'font.family': 'Times New Roman',
    'font.size': 14,
    'axes.linewidth': 1.5,
    'xtick.direction': 'in',
    'ytick.direction': 'in',
    'xtick.major.width': 1.5,
    'ytick.major.width': 1.5,
    'xtick.major.size': 6,
    'ytick.major.size': 6,
})

fig, ax = plt.subplots(figsize=(8, 5), dpi=300) 

# 画出参考线
ax.axhline(y=61.0, color='#d62728', linestyle='--', linewidth=2.5, label='AERO Baseline (61.0%)')
ax.axhline(y=62.0, color='gray', linestyle='-.', linewidth=2, alpha=0.8, label='Wake-up Threshold (62.0%)')

# 画出折线
ax.plot(x_axis, data, color='#1f77b4', linestyle='-', linewidth=2.5, 
        marker='o', markersize=6, markevery=10, markerfacecolor='white', markeredgewidth=2,
        label='Our Approach (Adaptive-AERO)')

ax.set_xlabel('Epoch', fontweight='bold', fontsize=16)
ax.set_ylabel('Cross-Shard Tx Ratio (%)', fontweight='bold', fontsize=16)

ax.set_xlim(0, 100)
# 纵坐标自适应
ax.set_ylim(50, 80)

ax.grid(True, linestyle='-', color='#E0E0E0', linewidth=1)
ax.set_axisbelow(True)

ax.legend(loc='upper right', frameon=True, edgecolor='black', fancybox=False, fontsize=12)

plt.tight_layout()
output_filename = 'AERO_Adaptive_Academic.png'
plt.savefig(output_filename, bbox_inches='tight')
plt.show()

print(f"✅ 学术版绘图完成！图表已保存为 figurePlot 目录下的: {output_filename}")