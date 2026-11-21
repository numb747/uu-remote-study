文件 50MB

| 工具 | 方向 | 轮 | 总时长 | 建连 | 到直连 | 最终路径 | 吞吐 Mbps | sha256 |
|---|---|---|---|---|---|---|---|---|
| p2pcp | ph2pc | 1 | 14.3s | 168ms | 168ms | 直连 | 30 | ok |
| sendme | ph2pc | 1 | 14.2s | 1362ms | 1909ms | 直连 | 33 | ok |
| p2pcp | pc2ph | 1 | 4.1s | 181ms | 181ms | 直连 | 114 | ok |
| sendme | pc2ph | 1 | 6.2s | 1360ms | 2034ms | 直连 | 87 | ok |
| p2pcp | ph2pc | 2 | 12.3s | 173ms | 173ms | 直连 | 35 | ok |
| sendme | ph2pc | 2 | 13.9s | 865ms | 1466ms | 直连 | 32 | ok |
| p2pcp | pc2ph | 2 | 5.2s | 183ms | 183ms | 直连 | 86 | ok |
| sendme | pc2ph | 2 | 9.8s | 1250ms | 1800ms | 直连 | 49 | ok |
| p2pcp | ph2pc | 3 | 14.6s | 176ms | 176ms | 直连 | 30 | ok |
| sendme | ph2pc | 3 | 15.1s | 1416ms | 2416ms | 直连 | 31 | ok |
| p2pcp | pc2ph | 3 | 4.0s | 223ms | 223ms | 直连 | 116 | ok |
| sendme | pc2ph | 3 | 7.4s | 2128ms | 2864ms | 直连 | 80 | ok |
| p2pcp-1c-bbr | pc2ph | 1 | 4.4s | 176ms | 176ms | 直连 | 102 | ok |
| p2pcp-1c-bbr | pc2ph | 2 | 33.0s | 245ms | 245ms | 直连 | 13 | ok |
| p2pcp-1c-cubic | pc2ph | 1 | 5.6s | 210ms | 210ms | 直连 | 79 | ok |
| p2pcp-1c-cubic | pc2ph | 2 | 5.4s | 180ms | 180ms | 直连 | 82 | ok |

中位数：

| 工具 | 方向 | 总时长 | 建连 | 到直连 | 吞吐 Mbps |
|---|---|---|---|---|---|
| p2pcp | ph2pc | 14.3s |173ms | 173ms | 30 |
| sendme | ph2pc | 14.2s |1362ms | 1909ms | 32 |
| p2pcp | pc2ph | 4.1s |183ms | 183ms | 114 |
| sendme | pc2ph | 7.4s |1360ms | 2034ms | 80 |
| p2pcp-1c-bbr | pc2ph | 18.7s |210ms | 210ms | 57 |
| p2pcp-1c-cubic | pc2ph | 5.5s |195ms | 195ms | 81 |
