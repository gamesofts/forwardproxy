# Naive UoT 实现比较与本项目取舍

核查日期：2026-10-20。范围为 GitHub 代码检索发现的 9 个相关 fork，以及其中与 UDP 有关的额外分支；不代表已经穷尽所有公开或未公开实现。结论来自固定版本源码检查、本项目自动化测试及本机微基准，不把 README 中“支持 UDP”的声明当作完整兼容性证明。

## 比较范围

| 仓库与核查提交 | UoT 实现 | 主要结论 |
| --- | --- | --- |
| [SagerNet](https://github.com/SagerNet/forwardproxy/tree/97c50f427b4412889b8ed48b79b3b21037bbe22e) | 早期 sing ServerConn，只识别旧 magic 地址 | 原始接入思路；UDP 分支在 ACL、端口和 upstream 分支之前返回，未对真实目的地执行这些策略；依赖 Caddy 2.5.1 |
| [txtyb](https://github.com/txtyb/forwardproxy/tree/6c9b4db7c10a39f87b4aeed2b40cfcd166909938) | 同类早期适配 | UoT 核心仍为旧地址和旧 sing API |
| [binarycraft007](https://github.com/binarycraft007/forwardproxy/tree/71ca33e331ef89b778c1e0ea1ad6e6ffa45474ca) | 同类早期适配 | 与 txtyb 的 forwardproxy.go blob 相同 |
| [Sora39831](https://github.com/Sora39831/forwardproxy/tree/d40d23e85488a5a7357cb56cbc3fcc012d37a1e7) | 同类早期适配 | 与 SagerNet 的 forwardproxy.go blob 相同；Caddy 依赖更新到 2.6.4，不等于升级 UoT 协议 |
| [Master-Hash](https://github.com/Master-Hash/forwardproxy/tree/7b212601c5661980ee1101e614e8de6f52adf845) | 正确区分 v1/v2，sing v0.7.5 | 双版本接入正确；仍在真实 UDP 目的地的 ACL/端口/upstream 策略之前返回 |
| [zedifen](https://github.com/zedifen/forwardproxy/tree/32d42bdde692776b0b2249a08692db29ca9b2584) | 正确区分 v1/v2，sing v0.9.6 | Caddy 2.11.7、Go 1.26；更新依赖，但 UoT 目的地策略仍与简单适配方案相同 |
| [aUsernameWoW / udpintcp](https://github.com/aUsernameWoW/forwardproxy/tree/5cf089c17a07adc32fb178957ee8a6758092cccf) | 两个 magic 地址均调用 Version=2 | 旧教程常用分支；v1 识别后解码版本错误，真实目的地 ACL/端口未执行 |
| [aUsernameWoW / naive](https://github.com/aUsernameWoW/forwardproxy/tree/c55724423ecd39402624538071f198036be79c25) | v1/v2、IP/端口过滤、SOCKS5 UDP ASSOCIATE、可选 UoT 透传，并合入 MASQUE | 比其 udpintcp 分支完善。sing 在 wrapper 前解析域名，因此 wrapper 只能看到 IP，原始域名 ACL 不完整；仍使用通用 ServerConn 的生命周期和包缓冲策略 |
| [amd64v3](https://github.com/amd64v3/forwardproxy/tree/ce61aefcf98e76c4dc61c09439cba88f34b28fa5) | v1/v2 + MASQUE，sing v0.8.1 | UoT 目的地策略仍为简单适配；MASQUE 是另一条协议路径，不能替代 sing-box 的 udp_over_tcp |
| [imgk / udpinhttp](https://github.com/imgk/forwardproxy/tree/6d1c91becd7d530a7cca2c26d3fd8bc70035d8ec) | UDP over HTTP / MASQUE | 属于不同协议方案；核查的默认分支没有 sing UoT magic 地址接入，不可作为现有 sing-box UoT 的直接替代 |

本表把 aUsernameWoW 的两个分支分别列出，因此 9 个仓库对应 10 行。保留原上游、Apache-2.0 许可和来源说明。

## 通用 ServerConn 的具体限制

对比 [sing v0.7.5](https://github.com/SagerNet/sing/blob/v0.7.5/common/uot/server.go) 和 v0.9.6 的实现：

- v2 请求头读取失败时直接返回，没有在该返回路径关闭全部 pipe/socket。不能仅依靠此适配器完成异常会话回收。
- 域名解析在真正读取该包的长度和 payload 之前进行。解析失败后 `continue` 会使下一轮从尚未消费的长度字段开始读地址，导致帧失步。本项目先读取完整包，再处理解析/策略失败。
- `buf.NewPacket()` 在标准构建中使用 16 KiB 包缓冲。65,000 字节输入在本机基准中导致 adapter 关闭；本项目使用完整 UDP 长度的缓冲，已通过带 padding 的 65,000 字节往返测试。
- v2 connect 状态由输入循环写入、输出循环读取，缺少显式同步。这里仅报告源码中的同步疑点；一次 race-enabled 基准没有复现 race，不能将疑点写成已证实故障。本项目在启动响应循环前读取请求，使状态成为只读。

## 本项目最终选择

1. 复用 sing v0.7.5 的请求与数据帧编解码，正确区分 v1/v2；不复制一套新的协议解析器。该版本保持现有 Go 1.22/1.23 测试矩阵兼容，更新到 sing v0.9.6 会要求 Go 1.25，当前编解码所需功能不依赖这次工具链升级。
2. 保留既有 CONNECT、认证、探测抵抗和 Naive padding 路径。UoT 位于 padding 内，不改变普通 TCP 隧道的协议格式。
3. 域名、解析后的 IP 和目的端口复用同一策略检查，发送时直接使用已检查的 IP，避免检查后再次解析。解析遵循请求 context。
4. v1 和 v2 non-connect 每个隧道只使用一个 UDP socket，保持本地出口端口稳定，不按目标累加 FD、读取协程或 64 KiB 缓冲。外部 NAT、防火墙仍可能影响端到端 NAT 类型，不能据此承诺所有网络都是 full-cone。
5. v2 connect 使用 connected UDP，固定目的地只检查一次，内核过滤其他来源。
6. 多目标模式采用最多 256 条、有效期一分钟的正/负策略及地址缓存。缓存满后新目的地仍可发送，只是不新增缓存；不会因累计目标数达到上限就关闭会话。
7. 多目标模式的拒绝、解析失败或发送失败只丢弃对应包，不破坏其他目标。协议帧损坏则关闭隧道。
8. 上传 EOF、请求取消、固定目标 I/O 失败和两分钟空闲会关闭隧道及其 socket；关闭时等待响应循环退出。单个空闲计时器使用单调时钟，不在每包路径重建 timer。
9. upstream 模式把原 UoT CONNECT 交给配置的上游，不绕过上游直接发 UDP。最终节点必须理解 UoT；并非所有 SOCKS/HTTP 服务都接受 magic 地址和 port 0。暂不加入独立的 SOCKS5 UDP ASSOCIATE 转换或 MASQUE 协议实现，这两者增加配置与维护范围，当前 sing-box UoT 使用场景不需要。

初版曾为每个目标使用 connected UDP。比较后主动舍弃该设计：它虽然便于源地址过滤，但多目标模式出口端口会改变，而且资源随目标数增长。这不是适合直接移植到本项目的最终方案。

## 微基准与性能边界

命令（Go 1.22.12，Linux amd64，GOMAXPROCS=2，本机 UDP echo，单隧道，重复三次取中位数）：

```sh
GOMAXPROCS=2 go test -run '^$' -bench 'BenchmarkUOTRelay/.*/connect=.*/bytes=(64|1200)$' -benchtime=500ms -count=3 .
```

`sing-adapter` 是多数 fork 共同使用的 `uot.NewServerConn`，不包含其整个 Caddy 服务、ACL wrapper、SOCKS5 或 MASQUE。`forwardproxy` 包含本项目的 UoT 中继与生命周期管理。均不包含公网延迟、TLS/HTTP2 或 Naive padding，不可用来推断真实跨境带宽、丢包率或抗识别效果。

| 数据模式 / payload | sing adapter 往返 | 本项目往返 | sing / 本项目分配次数 | sing / 本项目分配字节 |
| --- | ---: | ---: | ---: | ---: |
| 多目标 / 64 B | 14.68 μs | 19.71 μs | 25 / 23 | 456 / 408 B |
| 多目标 / 1200 B | 15.93 μs | 20.00 μs | 25 / 23 | 456 / 406 B |
| 固定目标 / 64 B | 14.25 μs | 16.44 μs | 14 / 11 | 344 / 291 B |
| 固定目标 / 1200 B | 14.38 μs | 15.84 μs | 14 / 11 | 344 / 293 B |

这证明本项目不是每项纯微基准都最快：本轮中位数额外约 1.5–5 μs/次本地往返，换取更完整的策略和生命周期管理，分配次数下降约 8%（多目标）和 21%（固定目标）。计时有调度噪声，不能承诺固定性能增益。针对当前需求，选择兼容性、稳定性、资源有界和可维护性的综合取舍，不宣称全场景数学意义上的“最优”。

## 验证

`uot_test.go` 覆盖实际 HTTP/2 CONNECT、认证、padding 开/关、首八次 padding 后的流量、v1、v2 connect/non-connect、空包、65,000 字节 payload、IPv4/IPv6/域名、多目标、稳定出口端口、域名/IP/端口 ACL、缓存容量外流量、超大 UDP 发送失败隔离、upstream 委托、请求取消、上传 EOF、坏请求和空闲超时。

受执行环境限制，本地原测试套件中两个依赖外部 DNS 的 ACL 测试无法解析 Google 域名；其余原有测试和全部新增 UoT 测试使用 `-race` 通过。完整原测试应由 GitHub Actions 的联网 Linux/Windows runner 验证。发布检查还需在选定的 Caddy 版本上运行，而不是只检查模块自己的最低依赖版本。
