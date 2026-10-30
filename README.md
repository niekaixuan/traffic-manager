# traffic-manager

Linux 下按应用统计网络流量的工具（类似 nethogs + vnStat），Go + bubbletea 实现。

- **后台 daemon** 常驻抓包，把每个 app 的上下行流量按小时/天写入 SQLite
- **TUI** 查询历史用量：今天 / 昨天 / 近 7 天 / 近 30 天 / 全部，含每应用明细
- 也有实时模式（`live`）和文本报表模式（`query`）

## 原理

1. `AF_PACKET`(TPACKETv3) 在各网卡抓包；
2. 用 `/proc/net/{tcp,tcp6,udp,udp6}` 把五元组映射到 socket inode；
3. 扫描 `/proc/<pid>/fd` 反查 inode 所属进程，得到应用名；
4. daemon 每 30 秒把增量写入 SQLite（小时粒度保留 8 天，天粒度保留 400 天）。

## 构建

```sh
go build -o traffic-manager .
```

## 使用

```sh
# 1. 启动采集（需要 root 或 capability）
sudo ./traffic-manager daemon
# 或授权后免 sudo：
sudo setcap cap_net_raw,cap_net_admin,cap_sys_ptrace=eip ./traffic-manager
./traffic-manager daemon

# 2. 查看历史 TUI（不需要 root）
./traffic-manager

# 3. 文本报表
./traffic-manager query -days 7

# 4. 实时监控（需要 root/cap）
sudo ./traffic-manager live

# 其它
./traffic-manager daemon -i eth0,lo   # 指定网卡
./traffic-manager -db /path/x.db      # 指定数据库
```

默认数据库位置：root 为 `/var/lib/traffic-manager/traffic.db`，普通用户为
`~/.local/share/traffic-manager/traffic.db`。建议 daemon 用 root 跑（systemd），
普通用户通过 `-db /var/lib/traffic-manager/traffic.db` 读历史（需对该文件有读权限）。

## 安装为系统服务

```sh
sudo install -m755 traffic-manager /usr/local/bin/
sudo install -m644 traffic-manager.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now traffic-manager
```

## 历史 TUI 按键

| 键 | 功能 |
|----|------|
| tab / ←→ / h l | 切换时段（今天、昨天、近7天、近30天、全部） |
| 1-5 | 快捷切换时段 |
| ↑/↓, j/k | 移动选择 |
| enter | 应用明细（当天按小时、更长时段按天的柱状图） |
| esc | 返回 |
| q | 退出 |

## 目录结构

```
capture/   AF_PACKET 抓包与进程归属
procmon/   /proc 解析：socket inode → PID
stats/     流量聚合与速率计算
storage/   SQLite 持久化（hourly + daily 两张表）
ui/        bubbletea TUI（历史 + 实时）
app/       采集器与 daemon
```

注意：少数短命连接的包在 socket 进入 /proc 表之前到达，会归入 `(unknown)`。
