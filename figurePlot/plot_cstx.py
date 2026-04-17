import pandas as pd
import matplotlib.pyplot as plt
import os

# 1. 精准定位文件
current_dir = os.path.dirname(os.path.abspath(__file__))
root_dir = os.path.dirname(current_dir)
file_path = os.path.join(root_dir, 'expTest', 'result', 'supervisor_measureOutput', 'Tx_number.csv')

if not os.path.exists(file_path):
    print(f"❌ 找不到文件：{file_path}")
    exit()

print(f"✅ 成功读取：{file_path}")

# 2. 读取 CSV
try:
    df = pd.read_csv(file_path)
    df.columns = [c.strip() for c in df.columns]
except Exception as e:
    print(f"❌ 读取 CSV 失败: {e}")
    exit()

col_epoch = df.columns[0]
col_total = df.columns[1]
col_normal = df.columns[2]

# 3. 按 Epoch 聚合，单纯把相同 Epoch 的数据加起来，绝不做任何过滤！
df_grouped = df.groupby(col_epoch)[[col_total, col_normal]].sum().reset_index()

# 强制按 Epoch 顺序排好，防止线条乱飞
df_grouped = df_grouped.sort_values(by=col_epoch)

# 计算跨片率
df_grouped['CSTX_Ratio'] = ((df_grouped[col_total] - df_grouped[col_normal]) / df_grouped[col_total]) * 100

epochs = df_grouped[col_epoch].values
cstx_ratios = df_grouped['CSTX_Ratio'].values
total_txs = df_grouped[col_total].values

# 4. 开始画图
plt.rcParams.update({'font.family': 'Times New Roman', 'font.size': 14})
mark_spacing = max(1, len(epochs) // 20)

# ====== 图 1：跨片率 ======
fig1, ax1 = plt.subplots(figsize=(10, 5), dpi=300)
ax1.plot(epochs, cstx_ratios, color='#1f77b4', linestyle='-', linewidth=2.5, 
         marker='o', markersize=4, markevery=mark_spacing, markerfacecolor='white', markeredgewidth=1.5,
         label='Cross-Shard Ratio')

ax1.set_xlabel('Epoch', fontweight='bold', fontsize=16)
ax1.set_ylabel('Cross-Shard Tx Ratio (%)', fontweight='bold', fontsize=16)
ax1.set_xlim(min(epochs), max(epochs))
ax1.set_ylim(max(0, min(cstx_ratios)-5), min(100, max(cstx_ratios)+5))
ax1.grid(True, linestyle='-', color='#E0E0E0')
ax1.legend()
plt.tight_layout()
fig1.savefig('Calculated_CSTX_Ratio.png', bbox_inches='tight')

# ====== 图 2：吞吐量 ======
fig2, ax2 = plt.subplots(figsize=(10, 5), dpi=300)
ax2.plot(epochs, total_txs, color='#2ca02c', linestyle='-', linewidth=2.5, 
         marker='s', markersize=4, markevery=mark_spacing, markerfacecolor='white', markeredgewidth=1.5,
         label='Total Tx Volume per Epoch')

ax2.set_xlabel('Epoch', fontweight='bold', fontsize=16)
ax2.set_ylabel('Total Transactions', fontweight='bold', fontsize=16)
ax2.set_xlim(min(epochs), max(epochs))
ax2.set_ylim(max(0, min(total_txs) - 500), max(total_txs) + 500)
ax2.grid(True, linestyle='-', color='#E0E0E0')
ax2.legend()
plt.tight_layout()
fig2.savefig('Calculated_Throughput.png', bbox_inches='tight')

plt.show()
print("🎉 画图完成！这次绝对是连续的真实曲线。")