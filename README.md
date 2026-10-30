# traffic-manager

Linux 下按进程统计网络流量的 TUI 工具（类似 nethogs），使用 Go + bubbletea 实现。

## 原理

1. 通过 `AF_PACKET`(TPACKETv3) 在各网卡上抓包；
2. 解析出五元组后，用 `/proc/net/{tcp,tcp6,udp,udp6}` 找到对应 socket 的 inode；
3. 扫描 `/proc/<pid>/fd` 反查 inode 所属进程，得到进程名/路径；
4. 按进程聚合上下行字节数与速率（EWMA），TUI 实时展示。

## 构建

```sh
go build -o traffic-manager .
```

## 运行

抓包需要 root 或 `CAP_NET_RAW`：

```sh
sudo ./traffic-manager
# 或授权后直接运行
sudo setcap cap_net_raw,cap_net_admin=eip ./traffic-manager
./traffic-manager
```

## 选项

```
-i eth0,lo   指定监听的网卡（默认：所有 up 且非 loopback 的网卡）
-list        列出网卡并退出
```

## 按键

| 键 | 功能 |
|----|------|
| ↑/↓, j/k | 移动选择 |
| enter | 查看该进程的连接明细（远端地址、流量） |
| esc | 返回列表 |
| s | 切换排序（total / ↓rx / ↑tx / name / pid） |
| q / Ctrl+C | 退出 |

## 目录结构

```
capture/   AF_PACKET 抓包与进程归属
procmon/   /proc 解析：socket inode → PID
stats/     流量聚合与速率计算
ui/        bubbletea TUI
app/       组装各模块
```

注意：极少数短命连接的包可能在新连接尚未进入 /proc 表时到达，会被计入 `unmatched` 并在下一次 rescan 后归属。
