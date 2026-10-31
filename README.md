# 星栈算力 · 转码共享 Demo

闲置算力共享交易平台的**最小可跑闭环**，第一条落地链路选的是**视频转码**：
一台机器上的 Broker 把一个视频按关键帧切成若干分片，派发给注册上来的算力节点并行转码，
节点回传产物做 SHA-256 校验，Broker 无损拼接成片，并和单机基线对比出**真实加速比**。

这条链路是整个毕设里工程风险最低、演示效果最直接的一条：输入输出都是人眼可验证的视频，
而调度、租约、校验、结算这些平台层要做的事，一个都绕不过去。

第二条落地链路是 **IPv6 云主机共享**：卖方发布虚拟机配置，买方下单后卖方机器上的 Agent
自动用 QEMU 拉起系统镜像、生成 cloud-init 配置、按 IPv6/端口映射接入；预扣、按秒结算、
失联冻结和自动停机复用同一套账本与节点通道（见下文「IPv6 云主机共享」）。

## 技术栈与选型理由

| 层 | 选择 | 为什么 |
|---|---|---|
| Broker / Agent | **Go，单一 codebase** | 交叉编译出 ~9MB 静态二进制，丢到任意闲置 Windows/Linux 机器就能跑，不需要装运行时——这是"闲置算力"接入的现实前提 |
| 通信 | **Worker 主动发起的 HTTP 长轮询** | 家庭/宿舍机器在 NAT 后面，没有公网 IP 也没有端口。让节点 outbound 拉任务，穿透天然成立。后续可平替成 gRPC stream / WebSocket，协议层已隔离 |
| 执行 | **ffmpeg 子进程，不自研转码内核** | 复用成熟引擎。系统贡献在分片策略与调度，不在编码器 |
| 分片 | **ffprobe 抽关键帧 → 关键帧对齐切点 → `-ss` 精确 seek** | 任意时间点切割会花屏和音画不同步；对齐到 IDR 帧是这套方案真正的工程难点 |
| 合并 | **concat demuxer + `-c copy`** | 不二次编码，无损拼接 |
| 存储 | 本地磁盘 + HTTP 分发源片 | demo 够用，预留 MinIO/NFS |
| 渲染 | **ffmpeg + Blender 子进程，不自研渲染器** | PR/FCPXML 时间线还原、Blender 帧渲染都复用成熟引擎；平台负责按秒/按帧切片、分发、校验和合成 |
| 模型推理 | **OpenAI 兼容网关，不自研推理内核** | llama.cpp / vLLM / Ollama 作为上游接入；平台只做按模型的低价路由、token 计量与 90/10 分成。MoE 用 `--n-cpu-moe` 或多机 `rpc-server` 部署 |
| 前端 | `go:embed` 单页 HTML | 零构建链，二进制里自带仪表盘 |
| 状态 | **SQLite（`modernc.org/sqlite`，纯 Go 免 CGO）+ 内存态热数据** | 账户/会话/节点/任务/分片/流水要能跨重启存活，否则"租约回收"和"按量结算"都没法验证；纯 Go 驱动才能继续维持"交叉编译一个二进制"这条前提。写路径是 write-through：内存调度照常，落库只做快照 |

## 跑起来

```bash
cd starstack
bash scripts/run-demo.sh              # Broker + 2 节点 + 60s 测试片 + 提交任务
# 浏览器打开 http://127.0.0.1:8787/

bash scripts/fault-test.sh            # 故障注入：跑到一半杀掉一个节点，看分片被回收重派
```

手动方式：

```bash
go build -o bin/starstack-server.exe ./cmd/server
go build -o bin/starstack-worker.exe ./cmd/worker

./bin/starstack-server.exe --data ./data --addr :8787 \
  --base-url http://192.168.1.20:8787        # 跨机演示必须填，节点要能访问到这个地址

# 在其它闲置机器上（把对应平台的二进制拷过去即可）
./starstack-worker --id dorm-pc --server http://192.168.1.20:8787 --slots 2
```

任务接口现在要求登录，先用一个会话 Cookie 再提交（用户名保持 ASCII：Windows 的 curl 会按本地代码页发送请求体，中文会乱码）：

```bash
curl -c /tmp/jar -X POST http://127.0.0.1:8787/api/auth/register \
  -H 'Content-Type: application/json' -d '{"name":"demo","password":"demo1234"}'

curl -b /tmp/jar -X POST http://127.0.0.1:8787/api/jobs \
  -F "path=/path/to/input.mp4" -F "vcodec=libx264" -F "preset=veryfast" \
  -F "q=28" -F "height=720" -F "segs=4" -F "audio=1" -F "baseline=1" \
  -F 'req={"need_gpu":true,"min_cpus":8,"max_price":1.0}'      # 软硬件硬约束，可选
```

端到端验收脚本（注册→认领节点→报价→提交→分片结算→账单→隐私与越权检查，11 步）：

```bash
bash scripts/smoke.sh        # 自己起 Broker + 2 个节点，跑完打印全部断言
```

跨重启恢复（任务在飞时杀掉 Broker 再原样拉起，检查旧 Cookie 仍有效、归属/单价/收益装回、
在飞分片 `attempt+1` 重派、账本无重复结算）：

```bash
python scripts/restart-test.py    # 端口 8798，独占一份 data 目录
```

Web 控制台七视图：注册/登录后依次是 提交任务（默认简洁模式：选文件 + 任务类型 + 画质；高级选项在个人资料里开启）、
我的任务（分片态、租约、进度、下载）、云主机、论坛（发帖与评论，支持图片/视频）、
模型 API（创建密钥、登记模型上游、看目录与调用示例）、
算力市场（按编码器聚合的可调度算力与价格区间）、
我的节点（待认领列表 + 一键认领 + 改价 + 该机的收益/吞吐/成功率）、账单（账户流水与按节点收益）、
个人资料（用户名、余额与界面设置）。
控制台的档位是自适应的：启动时先取一次匿名 `/api/state` 读 `policy`，公网档会自动收掉充值框、
Broker 本地路径输入、待认领列表和基线对比，Agent 命令行也换成带 `--token` 的版本——
页面上不会留下一个服务端必然拒绝的入口。

调度与结算的核心逻辑有单元测试兜底（不依赖 ffmpeg，直接构造节点池）：

```bash
go test ./internal/hub/    # 价格权重确实改变选择、Excluded/停派、预扣口径、冻结价结算、
                           # 公平份额、任务墙钟、硬约束字段
```

## 部署到公网服务器

Broker 用同一个二进制服务演示和公网两种场景，区别只在 `--public` 装的策略档位
（`internal/hub/policy.go`）。公网档把三样演示用的便利关掉，因为它们在公网 IP 上都是攻击面：
Broker 本地路径当任务源 = 任意文件读，待认领列表 = 抢别人的机器，自助充值 = 白送点数。

```bash
bash deploy/deploy.sh          # 交叉编译 linux/amd64 → scp → systemd 单元 → 启动
bash deploy/enable-nginx.sh --yes   # 在已有站点上加 /starstack/ 反代（会 nginx -t 校验，失败自动回滚）
```

`deploy.sh` 只做加法：新增 `/opt/starstack`（二进制）、`/var/lib/starstack`（SQLite 与产物）、
一个 `starstack` 系统用户和单元文件，不动这台机器上已经在跑的别的服务。
它**默认不代装 ffmpeg**（缺 `ffprobe` 时直接退出并打印命令），因为装软件包动的是整台机器；
要脚本代做就 `FORCE_FFMPEG=1`。接入令牌首次部署时 `openssl rand -hex 16` 生成在
`/etc/starstack/env`（`root:starstack 0640`），之后每次部署保持不变，脚本结尾把它打出来给
Provider Agent 当 `--token`。令牌走环境变量而不是命令行参数，`ps` 里就看不到它。
公网档还要求注册邀请码：`STARSTACK_SIGNUP_INVITE` 也放在 `/etc/starstack/env`，
`deploy.sh` 首次部署时生成、老部署缺这条时补生成，已有值不动，重启不会让已发出的邀请码失效；
新用户在注册页必须填写。确实要开放注册时加 `--open-signup` 显式说明，否则 `--public`
没有邀请码会直接拒绝启动。

单元文件里的运行约束是照着这台 891 MB 内存、上面还跑着别的服务的机器写的：
`MemoryMax=512M` / `MemoryHigh=384M` / `TasksMax=128`，越界时先杀 Broker 而不是让内核挑受害者；
`ProtectSystem=strict` + `ProtectHome=true` + 只有 `/var/lib/starstack` 可写。

**实际采用的入口是独立端口**：`--addr 0.0.0.0:8787` + `--base-url http://<Broker 地址>:8787`，
在 Azure 门户给这块网卡加了 8787 的入站规则后从公网 curl 得到 200。因为前面没有反代，
单元里**既不加 `--trust-proxy` 也不设 `--cookie-path`**：相信 `X-Forwarded-For` 等于让任何人
自己挑一个假来源、把登录限流整个绕过，而会话 Cookie 也不需要落在前缀下。

另一条路（这台机器上原本计划的那条）是复用已经在跑的 80 端口站点：`--addr 127.0.0.1:8787`
+ `--cookie-path /starstack/` + `--trust-proxy`，再用 `bash deploy/enable-nginx.sh --yes`
在现成站点里插一段 `location ^~ /starstack/`。它的前提是这台 VM 的 NSG 只放 80——
2026-10-22 实测就是这样（公网 curl `:8787` 返回 `000`、`:80` 返回 `200`），
后来是**持有订阅的人在门户里补了一条 8787 入站规则**才换成直连的。
`deploy/nginx-starstack.conf` 与 `enable-nginx.sh` 仍然保留，走前缀时它俩直接可用；
`enable-nginx.sh` 会先备份站点配置、`nginx -t` 通过才 reload、失败自动回滚，
因为它动的是别人正在用的服务。

Agent 侧接入：

```bash
./starstack-worker --id dorm-pc --server http://broker.example.com:8787 \
  --token <deploy.sh 打出的接入令牌> --price 0.4 --slots 2
```

`--server` 指到 Broker 的公开地址即可（走前缀时写 `http://IP/starstack`，
Agent 只是往这个 base 后面拼 `/api/...`）。节点的注册、长轮询领活、回传产物全部是
从节点向 Broker 发起的**出站** HTTP，因此不需要在任何一台节点机上开入站端口——
宿舍网、内网、NAT 后面都能参与。

### 网页里的一键提供算力

上面那条命令要求来访者已经有接入令牌、自己想一个节点 ID、还装好了二进制——对一个
只是想把闲置笔记本挂进来赚点的陌生人来说太重了。控制台里改成一步：登录后进「一键提供
算力」，填单价和并发槽数，点下载，把 `.cmd` / `.sh` 拷到那台机器上双击。

```
POST /api/nodes/launch      ->  {node_id, code, script, worker, ready}
GET  /launch/<code>/<os>/<arch>  -> 启动脚本（带上一次性启动码）
GET  /dl/starstack-worker-<os>-<arch>[.exe] -> 节点程序，来自 --node-bin-dir
```

启动码是比接入令牌弱得多的凭证：只授权**一个**节点 ID、只属于铸出它的那个账户、
30 分钟内有效、Agent 注册成功那一刻就花掉。绑定完成后 Broker 发回该节点自己的
凭据，Agent 存成 `<work>/node-<id>.key`；之后重新双击同一个脚本不再需要任何码，
`admitAgent` 认这台机器是"回来的"。文件名按节点 ID 走，因为同一台机器可能双击两个
不同节点的脚本而共用一个 `--work` 目录——共用 `node.key` 会把 A 的凭据递给 B，
而 Broker 正是把它当作"B 是我认识的节点"的证据。

脚本里不含令牌，也不问配对码；Windows 那份刻意只写 ASCII：cmd.exe 按机器自身的
OEM 代码页读 `.cmd`，UTF-8 中文在 GBK 机器上会变成解析错误（实测 `echo` 一行价格
就报 `'池：0.65' 不是内部或外部命令`），所以节点名由账户在网页里填、Broker 在绑定时
从启动码里取，走 JSON 就没有代码页可踩。`/api/state` 里的 `launch_windows` 等三个
开关告诉控制台这台 Broker 的 `--node-bin-dir` 里到底有没有对应平台的程序，没有就
不显示按钮，而不是发一个下不动的脚本。

## 渲染作业：Blender 与 PR/FCPXML

渲染和转码共用同一套「切片 → 派发 → 校验 → 合并 → 结算」骨架，区别只在切片单位和执行命令：

| 类型 | `kind` | 切片单位 | 节点产物 | Broker 合并 |
|---|---|---|---|---|
| 视频转码 | 空 / `transcode` | 视频秒 | `segXXX.mkv` | concat 无损拼接 |
| Blender | `blender` | **帧** | `segXXX.zip`（`frame_####.png`） | 解压全部帧 → `ffmpeg -framerate` 编码成 mp4 |
| PR / FCP 时间线 | `pr` | 视频秒 | `segXXX.mkv` | 本地 concat；编码参数不一致时自动转码兜底 |

**Blender**：把 `.blend` 连同贴图、缓存打成 zip（也可只传 `.blend`）。任务表单里可以覆盖
引擎、采样数、分辨率百分比和帧范围；节点执行

```bash
blender -b scene.blend -noaudio -o <tmp>/frame_#### -F PNG \
  -s <start> -e <end> -E <engine> --python-expr "<覆盖采样/分辨率>" -a
```

Worker 通过 `Fra:` 行解析真实进度，打包该片段负责的 PNG 回传；Broker 校验 SHA 后解压、
按帧号排序并用 `ffx.EncodeFrames` 合成视频。节点必须装 Blender，并在 PATH 中或用
`--blender` 指定。帧范围上限 20000 帧。

**PR / FCP**：Premiere 不能直接渲染 `.prproj`（它是工程文件，不是可交换的像素描述），
请在 Premiere 里「文件 → 导出 → Final Cut Pro XML」，把导出的 XML 或 FCPXML 与素材
按原始文件名一起打成 zip 上传。Broker 解析时间线、按剪辑点切片，每个节点把自己那段
clip 裁剪、缩放/补边、统一帧率与像素格式后编码，Broker 直接 concat。转场、调色、特效、
多轨叠加不在交换格式的可还原范围内：多视频轨只渲染第一轨并在任务 note 里写明；检测到
变速片段时按时间线时长渲染并提示。素材按**不区分大小写的文件名**匹配，因此不同目录下
同名文件会被视为同一个。

报价与转码同构：Blender 按「帧 × 节点单价」，PR 按「视频秒 × 节点单价」，预扣和差额退还
沿用交易层。提交入口是 `POST /api/jobs/render`（multipart：`kind=blender|pr` + 文件 +
参数字段）。

## 模型网关：OpenAI 兼容 API

控制台「模型 API」视图把买家与提供方接在同一套账本上：

- **买家**：创建 `sk-ss-...` 密钥（只显示一次，库中仅存 SHA-256 哈希），用任意 OpenAI SDK
  指向 `http://BROKER/v1`。支持 `/v1/models`、`/v1/chat/completions`、SSE 流式。
- **提供方**：登记 OpenAI 兼容地址、模型名、输入单价，并声明最大上下文与能力
  （工具调用 / JSON 输出 / 图像输入 / 推理 / 确定性采样 / 前缀缓存），可选填输出与缓存单价。

**路由**先按请求实际需要的上下文长度和能力过滤，再在可用上游中按价格排序；同价轮流，
避免一台机器吃满。会话可用 `X-Starstack-Session`（或 `prompt_cache_key`）固定回原上游
30 分钟，复用其前缀缓存；上游冷却或消失时价格重新接管。

**计费**按未缓存输入 / 缓存命中输入 / 输出三段计价，未填写的段沿用输入价。
预扣按 prompt 估算 + `max_tokens`（默认 1024，上限 8192）取候选上游的最高价，余额不足 402；
结束后优先用上游 `usage`（含 `cached_tokens`），缺失时按输出文本估算，多退少补。
提供方拿 90%，平台留 10% 撮合费。

**可复现与结构化输出**：`"starstack_deterministic": true` 只路由到声明了
`deterministic` 的上游，网关注入 `temperature=0`、`seed=1`，并在响应头返回
`X-Starstack-Output-SHA256`。请求 `response_format.type=json_object|json_schema` 时，
网关会校验上游返回的 content 是否为 JSON 对象；不合法就按失败切换上游。

上游凭据用数据目录下的 `gateway.key` 做 AES-GCM 加密后入库；公网档会拒绝把上游指向
私网/链路本地/云元数据地址。

MoE 与分布式部署的具体参数、脚本和 systemd 示例见
[`deploy/models/README.md`](deploy/models/README.md)：

```bash
# 单机 MoE：专家层放 CPU，注意力层留 GPU
MODEL=./Qwen3-30B-A3B-Q4_K_M.gguf N_CPU_MOE=24 bash deploy/models/llama-moe.sh

# 多机：每台跑 rpc-server，主节点聚合显存
bash deploy/models/llama-rpc.sh
RPC=192.168.1.10:50052,192.168.1.11:50052 bash deploy/models/llama-distributed.sh

# vLLM 双卡张量并行
MODEL=Qwen/Qwen3-30B-A3B TP=2 bash deploy/models/vllm-tp.sh
```

```bash
curl http://BROKER/v1/chat/completions \
  -H "Authorization: Bearer sk-ss-..." \
  -H "X-Starstack-Session: conv-42" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3-30b-a3b","starstack_deterministic":true,
       "messages":[{"role":"user","content":"你好"}]}'
```

## IPv6 云主机共享

转码市场卖的是"切片算力"，云主机市场卖的是**一段独占的虚拟机租期**。两者共用同一套账户、节点归属、
预扣/结算账本和控制台，但生命周期完全独立：卖方发布配置，买方下单后 Broker 通过节点自己的长轮询
通道把一条 `start` 指令送到卖方机器，Agent 用 QEMU 拉镜像、建差量盘、生成 cloud-init 种子盘、
分配 IPv6/端口，然后把 `running` 和连接端点回报给 Broker。

### 卖方：发布配置

控制台「云主机 → 发布我的虚拟机配置」填：节点、镜像 URL/格式/SHA-256、vCPU/内存/磁盘、
网络模式、IPv6、端口映射、每小时单价和最长租期。发布时 Broker 会核对：

- 节点属于当前账户，且以 `--vm` 启动、在线、上报了 `qemu` 能力；
- 卖方填的公网 IPv6 **确实在这台机器当前上报的地址列表里**（只接受 GUA，拒绝 ULA/链路本地/回环）；
- vCPU/内存/磁盘不超过这台机器上报的物理容量；
- `routed` 模式必须同时给 `/48 ~ /64` 前缀和网关，且 Agent 配置了 `--vm-bridge`。

Agent 侧启动参数：

```bash
./starstack-worker --id dorm-pc --server http://broker:8787 \
  --vm --qemu qemu-system-x86_64 --qemu-img qemu-img \
  --vm-max 2 --vm-dir /var/lib/starstack-vms \
  --vm-bridge br0 --vm-uplink eth0      # 只有 routed 模式需要后两个
```

`--vm` 会让 Agent 探测 qemu 版本、硬件加速器（KVM/WHPX/HVF，找不到就退回 TCG）和全局 IPv6，
并把它作为节点能力上报。没有安装 qemu 时 Agent 仍然可以正常做转码，只是控制台不会给它发布入口。

### 网页一键接入（推荐）

不需要先有节点。登录后进「云主机 → 一键把这台机器接入云主机」，选择目标机器的系统/芯片、
最多同时跑几台、是否用 routed 网桥，点「下载云主机启动脚本」。把脚本拷到那台机器上运行
（Windows 双击 `.cmd`；Linux/macOS 在终端执行 `sh 脚本名`）：

1. 首次会校验并下载约 10MB 的 Agent，缓存到用户目录，之后双击不再重复下载；
2. 检查 `qemu-system-x86_64` 和 `qemu-img`。Windows 找不到时会给出
   `winget install --id SoftwareFreedomConservancy.QEMU -e` 并询问是否代装；Linux 会按
   apt/dnf/pacman 给命令并询问；macOS 用 brew；
3. 提示 WHPX（Windows）或 `/dev/kvm`（Linux）状态，以及需要放行的 IPv6 入站端口
   （控制台 5900-5999、SSH/自动端口 20000-29999，外加配置里显式填写的端口）；
4. 以 `--vm --vm-max N` 启动 Agent，用一次性启动码自动绑定到当前账户。

脚本是一次性的，30 分钟内有效；Agent 启动后回网页刷新，节点出现在「发布配置」里。
关掉窗口即停止提供算力，下次重新双击同一个脚本会用缓存的节点凭据自动回来。

**没有网桥就选 slirp**：不需要 root、不需要改网络，最省事；routed 需要 Linux 网桥、
root/CAP_NET_ADMIN，并在启动脚本里填网桥/上联网卡。当前云主机客户机按 **x86_64** 镜像准备
（`qemu-system-x86_64` + `q35`）；ARM 主机可以用 QEMU 的 TCG 模拟 x86，但会明显变慢。

### 独立预检脚本

不想通过网页生成脚本、或者已经手动放好 Agent 二进制时，可以直接用仓库里的：

```bash
# Linux / macOS
bash scripts/vm-host-setup.sh --server http://broker:8787 --id dorm-pc \
  --launch <网页生成的一次性码> --vm-max 2
bash scripts/vm-host-setup.sh --server http://broker:8787 --id dorm-pc --check-only

# Windows PowerShell（脚本本身保持 ASCII，避免 5.1 代码页问题）
powershell -ExecutionPolicy Bypass -File scripts\vm-host-setup.ps1 `
  -Server http://broker:8787 -Id dorm-pc -Launch <code> -VmMax 2
```

两个脚本都支持 `--yes/-Yes` 跳过安装确认、`--worker/-Worker` 指定 Agent 路径、
`--bridge/--uplink` 配置 routed 模式；`--check-only` 只做预检。

### 买方：下单与计费

`POST /api/vm/orders` 传 `offer_id`、可选主机名、租期小时数和 SSH 公钥。Broker 在锁内完成：

1. 校验配置/节点在线、不能租自己的机器、单账户在租数量上限；
2. 按**活跃实例**（不是已发布配置）核算节点剩余的 vCPU/内存/磁盘；
3. 分配资源：`slirp` 模式从 20000-29999 选空闲宿主机端口（SSH 22 自动补一条映射），
   `routed` 模式在前缀里选一个未占用的 guest `/128`；VNC 从 5900-5999 分配；
4. 按"单价 × 小时数"预扣，余额不足 402；
5. 生成 RSA 2048 SSH 密钥对（买方给了公钥就用买方的）、root 口令、VNC 口令，
   前两个用 `gateway.key` 做 AES-GCM 加密后入库；**私钥/口令只在响应里返回一次**；
6. 写入实例并投递 `start` 指令。

计费与转码同构：预扣先扣，按**实际 running 秒数**结算，`spent = min(预扣, 单价 × 秒数 / 3600)`，
停机时退还 `预扣 - spent`，卖方拿 90%、平台留 10%。预扣同时是运行时长的硬上限：
到 `ExpiresAt`（`started_at + 预扣/单价`）Broker 自动下发 `stop`。买方可以在到期前 `extend`，
按增量再次预扣；已到期则拒绝，不再扣钱。运行中控制台展示的"已用"是按同一公式实时估算的，不是冻结值。

状态机：`queued → provisioning → running → stopping → stopped`，失败走 `failed`。
节点 90 秒没有心跳转 `unreachable`（计费按最后心跳冻结），离线超过 5 分钟按最后心跳结算并退还剩余预扣；
Agent 重新上线时 Broker 会把漏掉的 `start/stop` 重新投递。Agent 回报 `stopped/failed` 后清理标记置位，
不会无限重发停机命令。

### 两种 IPv6 接入方式

**slirp（推荐，跨平台）**：QEMU 用户态网络给虚拟机 10.0.2.15 + IPv4 NAT，hostfwd 只绑
`127.0.0.1:<port>`；Agent 另起一个 **IPv6 专用转发代理**监听 `[::]:<port>`，把买方的
`[卖方 GUA]:port` 连接转发到 QEMU 的 IPv4 回环映射。TCP 双向拷贝，UDP 按客户端做 60 秒会话表。
控制台 VNC 也走同一个代理（QMP 设在 `127.0.0.1`，VNC 只绑回环并设口令）。
这样做是因为 slirp 的入站 hostfwd 在多数 QEMU 构建里只支持 IPv4，依赖它会把"公网 IPv6 可达"变成版本彩票。

**routed（Linux，虚拟机直接拿 GUA）**：Agent 创建 tap、挂到 `--vm-bridge` 指定的网桥，
`ip -6 route replace <guest>/128 dev tap`；如果 `--vm-uplink` 非空，再开 `proxy_ndp` 并注册
`ip -6 neigh proxy`，让上游路由器/交换机把邻居发现代答给宿主机。虚拟机通过 cloud-init 的
network-config v2 静态配置地址、网关和 DNS。`routed` 模式不需要端口转发，虚拟机自己的 22/80 端口就是
对外端口，前提是卖方防火墙放行。它需要 root/`CAP_NET_ADMIN` 和预先配置好的网桥。

### Agent 侧自动装机

- 镜像按 URL + SHA-256 缓存到 `<vm-dir>/images`，同一基础镜像只下载一次，32 GiB 上限，7 天清理；
  SHA-256 不匹配直接失败（镜像一旦被换掉，买方拿到的就不是卖方描述的系统）。
- cloud 镜像用 `qemu-img create -f qcow2 -F <fmt> -b <base>` 生成差量盘，磁盘大小按配置；
- 内置一个最小 ISO9660 + Joliet 写入器生成 NoCloud 种子盘（卷标 `cidata`，含 `user-data`/
  `meta-data`/`network-config`），不依赖 `genisoimage`/`cloud-localds`，单个 Agent 二进制即可跑；
- `user-data` 创建 `starstack` 用户（sudo 免密 + 买方公钥）、写入 root/用户口令、关闭 SSH 密码登录；
  控制台仍可用 root + 一次性口令通过 VNC 登录；
- QEMU 参数：q35、`-accel kvm|whpx|hvf|tcg`、virtio 磁盘/网卡、`-daemonize` 的等价 detach（Unix 用 Setsid，
  Windows 用 detached process + taskkill），`-vnc 127.0.0.1:<display>,password=on`，QMP 走
  `127.0.0.1:<随机端口>`，串口日志落 `console.log`；硬件加速器初始化失败时会自动回退
  `-accel tcg` 再启动一次（慢但能跑），不会因为没开 WHPX/KVM 权限就把买方订单直接判死；
- QMP 在启动后设置 VNC 口令；没有口令就拒绝启动，避免开一个无认证控制台；
- 每个实例目录有 `state.json`（plan/PID/QMP 端口/tap/端点）。Agent 重启后按 state + QMP
  `query-status` 重新认领正在跑的虚拟机，并重建 IPv6 转发代理，不重复下载/建盘；
- 停机走 QMP `system_powerdown`，20 秒内没关就 `quit`/kill，然后删 tap/路由/NDP 代理和实例目录
  （买方数据不留在卖方磁盘上）。镜像缓存单独保留。

### 云主机 API

```
GET    /api/vm/offers                 卖方自己的 + 所有可租配置
POST   /api/vm/offers                 发布（必须是自己在线且带 --vm 的节点）
PUT    /api/vm/offers/{id}            上架/下架 {"enabled":true|false}
DELETE /api/vm/offers/{id}            删除（已租实例按订单快照继续）
POST   /api/vm/orders                 下单 {offer_id,name,hours,ssh_public_key}
GET    /api/vm/instances              买方/卖方可见的实例
POST   /api/vm/instances/{id}/stop    买方停机 / 卖方强制停机
POST   /api/vm/instances/{id}/extend  续费 {hours}
GET    /api/vm/claim?worker=...       Agent 长轮询（节点凭据）
POST   /api/vm/report                 Agent 回报 downloading/provisioning/running/stopping/stopped/failed
```

### 验证

Broker 的不依赖 QEMU 部分有端到端单测：发布 → 下单 → 预扣 → Agent 渲染 `running` → 按 30 分钟
结算 → 退款与 90% 分成，另有 routed 地址分配、端口冲突、余额不足、自租拦截：

```bash
go test ./internal/hub -run TestVM -v
go test ./internal/vm ./internal/vmhost -v
```

控制台的云主机视图用 Playwright 在真实浏览器里跑过卖方/买方 × 浅色/深色 × 宽屏/窄屏，
页面无 JS 错误、无横向溢出；发布表单、租用表单、端点展示、续费/停机按钮均可用。
**QEMU 真机开虚拟机没有在本机跑过**（开发机未安装 qemu），这一段的正确性由 seed ISO 的
pycdlib 解析、转发代理的 IPv6 回环测试和 QMP 参数构造支撑；到有 KVM/WHPX 的机器上应先
用一台 Ubuntu cloud image 做一次冒烟。

### 已知限制（云主机部分）

1. **ISO 安装不是全自动的。** `image_format=iso` 会挂安装器和 NoCloud 种子盘，但发行版安装器
   的自动化协议各不相同；当前需要买方在 VNC 里点完安装。qcow2/raw 的 cloud image 才是"下单即用"。
2. **slirp 模式下虚拟机本身没有公网 IPv6**，公网 IPv6 落在卖方宿主机到虚拟机的端口映射上。
   需要虚拟机自己持有 GUA（比如跑需要主动入站 IPv6 的服务）时用 routed 模式。
3. **routed 模式需要卖方自己准备网桥、路由和防火墙。** 平台只做 tap/路由/NDP 代理，
   不接管整机网络配置；`proxy_ndp` 通过写 `/proc` 完成，非 root 会失败。
4. **IPv6 地址变化会使已发布配置失效。** 发布时校验的是"当前上报地址"，运营商重新前缀委派后
   卖方需要下架重发；已租实例的连接地址也不会跟着变。
5. **没有浏览器内控制台。** 返回的是 `vnc://host:port` + 一次性口令，需要 VNC 客户端；
   没做 noVNC/WebSocket 网关。
6. **没有快照、迁移和 GPU 直通。** 停机即删盘；虚拟机跑在卖方单机上，节点掉线只能按最后心跳结算。
7. **没有 guest agent。** 优雅关机依赖 ACPI；不响应 ACPI 的镜像会被 20 秒后强杀。
8. **镜像下载在 Agent 端。** 大镜像会占用卖方上行带宽，Broker 只负责把 URL 和校验值放进指令；
   下载进度会回报，但 Broker 的启动总时限是 25 分钟。
9. **端口转发是 Agent 进程内的用户态代理。** Agent 挂掉期间虚拟机还在跑但不可达；重启后会重建代理。
   它也不做 TLS 终止/访问控制，虚拟机里该开防火墙还是要开。VNC 本身是明文协议、口令认证只有
   8 位 DES 强度，公网使用建议买方自己走 SSH 隧道或 `ssh -L`。
10. **镜像 URL 不能内嵌账号口令。** 需要私有镜像时先在卖方机器上把镜像放到可匿名拉取的地址，
    或后续接对象存储的预签名 URL；当前 Agent 不做鉴权下载。

## 代码结构

```
internal/ffx/    ffmpeg/ffprobe 封装：探测、关键帧抽取、切分规划、转码+进度解析、拼接
                 + Detect()：把这台机器的软硬件一次测清楚（核数/内存/磁盘/GPU/编码器/运行时）
internal/hub/    Broker：节点注册、两阶段匹配调度、租约回收、回传校验、合并、基线对比
                 + users.go：账户/会话/口令哈希、节点认领、报价、预扣与结算、账单流水
                 + render.go：Blender/时间线渲染的登记、切片分发与合并
                 + gateway.go：OpenAI 兼容网关、能力/上下文路由、三段计费、会话粘性、结构化输出校验
                 + vm.go：云主机配置发布、下单、IPv6/端口分配、Agent 指令通道、按秒结算与自动停机
                 + policy.go：演示/公网两套部署档位、Agent 凭证与签名源地址、登录限流、提交护栏
                 + launch.go：启动码、按平台生成的 .cmd/.sh 启动器（可带 --vm 与 QEMU 预检）、/dl/ 下发节点程序、暂停派活
                 + scheduler_test.go：调度与结算核心逻辑的单元测试（不需要 ffmpeg）
internal/render/ Blender 帧范围与 FCP XML/FCPXML 时间线解析、按帧/按秒的纯函数规划
internal/vm/     云主机领域契约：IPv6 GUA/前缀校验、guest 地址与宿主机端口分配、按秒计费、
                 cloud-init NoCloud seed ISO（内置 ISO9660/Joliet 写入器）
internal/vmhost/ Agent 侧 QEMU：镜像缓存与校验、差量盘、cloud-init seed、QMP 控制、VNC 口令、
                 IPv6 TCP/UDP 转发代理、Linux tap/routed + NDP 代理、实例状态与重启认领
internal/store/  SQLite 持久化：users / sessions / nodes / jobs / segments / ledger / api_keys /
                 upstreams / vm_offers / vm_instances
cmd/server/      平台服务端（--public / --agent-token / --cookie-path / --trust-proxy / --base-url）
cmd/worker/      算力节点 Provider Agent（硬件发现、能力上报、取任务、转码/渲染、回传）
                 + vm.go：云主机指令轮询、启动/停机执行、运行心跳与失败回报
internal/hub/web 前端单页控制台（登录 + 提交任务/我的任务/模型 API/算力市场/我的节点/账单 六视图）
deploy/          服务器部署件：systemd 单元、nginx 反代片段、env 模板、deploy.sh / enable-nginx.sh
deploy/models/   MoE/分布式推理部署：llama.cpp rpc-server、--n-cpu-moe、vLLM TP、Ollama
scripts/         演示与验收：run-demo.sh / fault-test.sh / smoke.sh / restart-test.py
                 + vm-host-setup.sh / .ps1：云主机 QEMU/网桥/权限预检与 Agent 启动
```

## 已经实现并能演示的机制

**资源接入层** — Agent 启动即调 `ffx.Detect()` 把这台机器测一遍：CPU 核数、内存、可用磁盘、
`nvidia-smi` 显卡、本机 ffmpeg **实际可用**的编码器（libx264/x265、h264/hevc_nvenc、qsv、amf、svt-av1）、
以及 ffmpeg/ffprobe/python 等运行时版本，按能力上报注册。上报字段走白名单，Agent 不能自报"我有 9999 核"
之外的东西来骗派发——但**单价是提供者自己定的**，这是市场设计的一部分。硬约束过滤发生在这里：
一个 `h264_nvenc` 的任务绝不会派给只有 CPU 的节点。

**用户接入与信任边界** — 账户口令用 PBKDF2-HMAC-SHA256（20 万次迭代，手工实现，RFC 6070 向量对齐）
存哈希不存明文，校验走 `subtle.ConstantTimeCompare`；会话是 30 天 HttpOnly + SameSite=Lax Cookie。
注册接口对"用户名已存在"和"口令错误"返回同一句话，避免账户枚举。
**节点归属靠一次性配对码**：Agent 注册时 Broker 铸一个 6 位码打在该机 stdout（Broker 日志和
控制台「我的节点」的待认领列表也能看到），登录用户在网页输入后才把这台机器挂到自己名下——
没有这一步，任何账户都能把别人的算力吞进自己账上。认领之后：非本人看不到这台机器的主机名、
内网地址、磁盘与收益（`/api/state` 按账户脱敏），非本人改价一律 401。
公网档再收紧四层：新用户注册必须带邀请码（`STARSTACK_SIGNUP_INVITE`，控制台注册页会多出输入框）；
Agent 首次注册必须带 `--token`（共享密钥走环境变量 `STARSTACK_AGENT_TOKEN`，
不进命令行也不进 `ps`），注册成功后 Broker 用 HMAC 给它派生一个**按节点 id 的**凭证
`X-Node-Secret`，此后回报和回传里的 `worker=xxx` 才是一个被验证过的事实而不是自称；
任务源文件下发的是**带签名的过期 URL**（`?exp=&sig=`，签名把到期时间算进去），泄露的地址会自己失效；
登录/注册按来源 IP 做固定窗口限流（演示 30 次/分、公网 10 次/分）。

**调度层（软评分匹配）** — 任务可以带一组软硬件硬约束（`req`：`need_gpu`、`min_gpu_mem_mb`、
`min_cpus`、`min_mem_mb`、`runtime`、`arch`、`max_price`），先做过滤再做评分：
`pickWorkerLocked` 在通过硬约束的在线节点里打分取最优：

```
score = 实测吞吐 × 信誉 × （可行池均价 ÷ 该节点单价）^ CostWeight / (1 + 当前占用)
```

吞吐是该节点历史分片的指数滑动平均（单位：每秒墙钟转出的视频秒数），新节点无历史数据时取探针值 1.0
参与竞争——这就是论文里"试用期探索"的最小实现。价格项刻意写成**与池均价的比值**再取幂，所以它是
无量纲的：市场整体交易在 0.1 点/秒还是 10 点/秒，同一家平台的相对偏好不会变。`CostWeight`
由部署档位决定（演示 0.5 / 公网 0.75），控制台报价里会把这条公式按当前权重原样打出来，
`/api/quote` 还顺带返回这一片的 `first_pick`——买方看得见"谁会接到活、为什么是他"。
**派发即占槽**，避免慢节点被连续塞满任务；派发那一瞬间还把该节点的报价冻进 `seg.PriceAt`，
之后它改价不影响已在飞的分片。
约束不满足时任务不会失败，而是排队并在 note 里写清"当前没有满足硬件要求的在线节点"——
闲置池的含义就是"现在没有"不等于"永远没有"。
每个 tick 还有**公平份额**：`dispatchBudgetLocked` 把当前空闲槽平分给排队任务，
最老的 job 不能一口吞掉全部算力把后面的人饿死。

**交易层（预扣 → 按量结算 → 退还）** — 提交任务时按**最贵的可行节点 × 整片时长**保守预扣
（`estimateHoldLocked`），余额不足直接 402 拒单。预扣用的费率不是猜的：`holdPriceLocked`
会在三种口径里挑一个并把人话写进 note 与报价里——"满足约束的在线节点中最高报价"、
"暂无满足约束的节点，按在线节点最高报价预留"、"暂无在线节点，按节点默认报价预留"
（最后一档的 1.0 点/秒是 Agent `--price` 的默认值，属于**文档化的兜底**，不是实测出来的市场价）。
每个分片完成并经 Broker 重算 SHA-256 一致后，按 `该分片视频秒数 × 派发时冻结的 seg.PriceAt`
真实结算，任务结束时把预扣与实结的差额退还。用冻结价而不是"该节点当时的单价"是刻意的：
买方授权的是这一片在这个价上的算力，节点跑一半涨价不该由买方买单。
提供者侧同时记两张账：节点累计收益点、已结算算力秒数，账单页可查。
基线对比跑在 Broker 自己机器上、不产生分片，因此**不计费**——这条在代码注释里写明，
免得答辩时被问"你自己单机跑一遍也要收钱？"。

**容错与租约** — 每次派发带租约（`45s + 2×分片时长`，下限 30s），节点心跳随进度上报刷新租约。
回收扫描**按分片驱动而非按节点驱动**：持有分片的节点离线（25s 无心跳）立即回收，不必等租约到期；
节点活着但卡死则走租约超时。两条路都退回 pending → 重新派给其它在线节点，最多重试 3 次后判定任务失败。
重派有两条新增的约束：回收/失败时把该节点写进这个分片的 `Excluded` 名单（分片级、不是全局黑名单），
同一个节点不会再把同一块活再接回去；同一节点连着丢 3 片会被 `punishLocked` **停派 90 秒**，
机器在市场里仍然可见、`parked_sec` 倒计时看得见，只是暂时不接新活。
语义是 at-least-once，但**租约带所有权校验**：一个分片被回收重派之后，老节点再回传进度、失败、
甚至产物文件，Broker 一律拒绝（`/api/upload` 返回 409，日志打 `stale ... ignored`），已完成的分片
也不会被迟到的重复结果改写或二次结算。不做这一步，"慢节点再交一次同样的活"就能白拿钱，
而一个"失败"回报还能把别人正在跑的healthy分片标记成失败。
最后还有一道**任务级墙钟**：`deadlineLocked` = 视频时长 × `JobSlowFactor`，下限 `MinJobDeadline`
（演示 60×/30min，公网 30×/15min）。超时就把任务判失败并**退还预扣**——一条永远不会有人接的约束
不该把买方的点数永久锁住。

**结果校验** — 节点回传自己算的 SHA-256，Broker 落盘时独立重算，两个哈希都一致才记 `match=true`。
这就是"半可信平台"里让造假有成本的那一步，仪表盘上能看到"哈希校验一致 4/4"。

**计量与对比** — 记录分布式墙钟、各分片耗时之和、以及**单机基线的实测墙钟**（同机同参数跑一遍完整转码），
直接给出真实加速比，而不是从理论推算。

**算力市场** — `/api/market` 按编码器能力把在线节点分桶（节点数、空闲槽、价格区间、平均吞吐、最强显卡），
买方一眼看到"这个能力档位现在值多少点"。提供者自己定价，改价只影响从此刻起新派发的分片，
不影响已在飞的任务。

**持久化与重启恢复** — 账户、会话、节点归属、任务、分片、账本全走 write-through 落 SQLite；
Broker 重启后 `restore()` 把节点连同归属/单价/实测吞吐/收益一并装回内存，未完成任务的在飞分片
回退成 `pending` 且 `attempt+1` 重新派发，用户的 Cookie 也不因为重启而失效。
配对码是内存态的，重启后由 Broker 重新铸造并打在日志/控制台待认领列表里。

## 实测结果（第一轮：无账户体系时的纯调度链路，AMD Ryzen 7 6800H 16 核 / RTX 3060 Laptop，60s 1080p→720p，4 分片，libx264 veryfast）

| 指标 | 值 |
|---|---|
| 分片耗时之和 | 24.8 s |
| 分布式墙钟 | **7.5 s** |
| 单机基线墙钟 | 10.9 s |
| 真实加速比 | **1.45×** |
| 分片并行度 | 3.3× |
| 哈希校验 | 4/4 一致 |
| 拼接产物 | 60.1 s，全片解码无错误（接缝干净） |

> **为什么加速比不是 4×？** 所有"节点"跑在同一台笔记本上，抢的是同一组 CPU 核和同一块 GPU。
> 单机基线还和分布式任务并发跑，互相争抢。这个数字的诚实解读是：**分片调度链路本身已经打通，
> 并行度 3.3× 说明切分和派发是对的，端到端 1.45× 是单机资源上限决定的。**
> 跨机演示（一台台式机 + 一台轻薄本 + 一台旧游戏本，各自独立编码器/CPU）才能体现闲置池的规模效应。
> 这恰好也是论文的论点之一：加速比受节点异构性和同时可用性约束，不是节点数的线性函数。

## 容错实测（`bash scripts/fault-test.sh`）

6 分片任务跑到第 12 秒时强杀 `node-b`（它手上正压着 #4、#5 两片），`node-a` 保持存活：

```
19:25:05  seg 61a1ccd14f-4 done on node-a: 6.3s, verified=true, cost=10.000 points
19:25:28  reclaiming job 61a1ccd14f seg 5 from node-b (node offline, attempt 1)
19:25:32  seg 61a1ccd14f-5 done on node-a: 3.4s, verified=true, cost=10.000 points
19:25:32  job 61a1ccd14f done: wall=44.6s seg-sum=53.4s billed=60.000 points
```

掉线节点手上的分片被租约回收、由 `node-a` 接手重算（`attempt=1`，SHA-256 校验一致），
6 片各结算一次、共 60.000 点，产物 60.145578s、全片解码无错误。**节点掉线不再让任务永久卡死**——
这正是闲置算力共享区别于稳定资源池的核心工程问题，也是答辩时"你这和 AutoDL 有什么区别"的实证回答。

## 实测结果（第二轮：账户、鉴权、按节点单价结算）

同一台机器起 Broker + `node-a` + `node-b`，全部走登录后的 Web/API 路径。

**验收套件**（`bash scripts/smoke.sh`，11 步）：匿名 `GET /api/jobs` → 401；`min_cpus=9999`
的硬约束 → 可派发 0 台；4 分片全部 `verified=True`；墙钟 6.2s / 分片之和 18.0s / 并行比 2.89×；
预扣 60.00、实结 60.00（两片 14.00 + 两片 16.00，按各自节点单价）；自己的节点改价 0.5 成功、
匿名改价 401；非本人节点的响应里不含 hostname/address。

**重启恢复**（`python scripts/restart-test.py`，任务跑到一半杀 Broker 再起）：
`restored 2 node(s), 1 unfinished job(s)`；旧 Cookie 不重新登录仍可访问；两个节点的归属、
单价（node-a=0.8）、实测吞吐、累计收益全部装回；在飞的 #4/#5 回退成 `pending` 且 `attempt+1`
后重新派发并跑完；6 片各结算一次（3×8.000 + 3×10.000），账本里**没有重复结算的分片**；
预扣 60.00 → 实结 54.00 → 退还 6.00 → 余额回到 100.00；产物 60.145578s 解码无错误。
跨重启的墙钟（18.5s）里含停机窗口，所以控制台和日志都拒绝打印加速比，只标 `跨 Broker 重启恢复`。

**浏览器实测**（`http://127.0.0.1:8787/`，注册→认领→改价→上传文件→提交→账单，全程点界面）：
新账户 `网页演示` 注册得 100.00 点；待认领列表出现两台在线 Agent 及配对码，一键认领成功；
`node-a` 界面改价 0.80 点/视频秒；上传 `samples/input.mp4` 后浏览器自动读出时长 60s 并刷出报价
（预扣 60.00 = 最贵可行节点价，最省 48.00）；6 分片任务墙钟 15.3s、分片之和 46.1s、
**分片并行比 3.01×**、单机基线 **真实加速比 1.36×**、6/6 哈希一致、实结 54.00 点、
界面显式显示 `+6.00 预扣退还`；账单两张表分别给出账户流水和按节点的收益
（node-b +30.00、node-a +24.00）；`/download/{job}` 返回 200、`video/mp4`、28,698,344 字节；
退出后匿名访问 `/api/state`、`/api/estimate`、`/api/ledger` 全部 401，错密码提示
"用户名或密码错误"（不泄露账户是否存在），重新登录后余额 150.00 点（含界面充值 50.00）仍是同一个会话账户。

> 这里的 1.36× 和第一轮的 1.45× 都是**同机三个进程抢同一组 CPU/GPU** 测出来的，
> 基线还和分布式任务并发跑。它们只证明度量链路是通的，不代表跨机部署的收益。


## 已知限制（都是论文里可以写的东西）

1. **切分粒度受源片关键帧密度限制。** 长 GOP 的视频（比如 5s 一个关键帧）切不出任意多片，
   任务画像里的"分片数"存在物理上限——仪表盘 note 会直接打印"asked for 8 pieces, got 5"。
2. **拼接后时长有 ~0.1s 溢出。** 每个分片的 AAC 编码器各自带 priming samples，拼接后累加。
   正式实现应在分片头写入容器级时间戳修正，或直接对音轨走 `-c:a copy` + 独立切分。
3. **产物体积比基线大 ~5%。** 各分片独立起 GOP，码率分配无法跨片全局最优。
4. **源片要整片下载到节点。** demo 走 HTTP 全量分发；正式实现应做分片区间请求（HTTP Range）
   或预先切片，否则大文件时带宽会成为瓶颈——这是"异构任务分片粒度"论点的反面案例。
5. **持久化只做了一半。** 任务/分片/账本/归属能跨重启恢复，但**分片产物本身**没进对象存储，
   Broker 宕机时正在上传的分片会丢（靠重算补回来，at-least-once 语义兜住了正确性，代价是白烧算力）。
   配对码也是内存态。正式实现应把租约流水单独建表并接 MinIO/S3。
6. **跨重启的任务不计量。** 恢复后墙钟里混进了停机窗口，控制台和 Broker 日志都会显式拒绝算加速比
   （`resumed` 标记），宁可少一个数字，也不要一个看起来很好但没意义的数字。
7. **账户体系是"能演示"级别。** PBKDF2 自己实现是为了去掉 C 依赖，正式环境应换 argon2id 并加
   CSRF Token/短信邮箱验证；登录限速与公网档（`--public` 关掉充值、本地路径、待认领列表，
   注册送点从 100 降到 20）是这轮加固补上的，但 `/api/me/recharge` 本身仍是演示用的无限额度，
   真实交易要接支付与对账。
8. **Provider Agent 二进制因为引用了 store 层而变大**（~9MB → ~15MB，纯 Go 驱动仍在，
   没引入 CGO）。后续可把节点注册 payload 抽成独立小包，让 Agent 不再链接 SQLite。
9. **没有真正的 GPU 隔离。** 一台机器多节点共享 GPU 靠的是 NVENC 硬件单元自带的并发能力，
   不是 MIG/vGPU。多租户超卖场景仍需要论文里设计的 MIG 切片。
10. **未认领的节点照常扣买家，但没人可付。** Agent 上报后如果还没被任何账户认领，
    它跑的分片仍然计入 `spent`（买家的预扣不会因为算力来自"无主"机器而少收），
    但结算时找不到 `owner`，这部分点既不退买家也不付提供者，等于被平台留存。
    正式实现应把它放进托管账户（escrow），提供者完成认领后补发；
    现在 `settleSegment()` 里只有注释说明，没有兜底。这也是为什么演示脚本一定先配对再提交任务。
11. **调度里的新系数全是拍脑袋的常数。** 价格权重 0.5/0.75、丢 3 片停派 90 秒、任务墙钟 60×/30×、
    重试上限 3——每一个都有它的理由（限流不被绕过、坏机器不能反复接活、预扣不能被永久锁住），
    但没有一个是从数据里拟合出来的。这一轮调度/结算改动实测过的只有 8 个单元测试和两组同机两节点
    对照（60s 片、4 分片、libx264 veryfast，两台同硬件、单价 0.3 与 1.0）：
    两台都还没有历史吞吐时，`/api/estimate` 的 `first_pick` 落在 0.3 的节点上，4 片按 2/2 分配，
    预扣 60.00 → 实结 40.40 → 退 19.60；紧接着再跑一轮，`dear` 已经带着上轮实测吞吐 6.35
    （`cheap` 3.18），`first_pick` 就翻回了贵的节点——**吞吐项在这个权重下压得过价格项**，
    这一轮实结 37.60。两轮里每片的 `price_at` 都冻结成派发时的那一档并落库，4/4 哈希一致，
    提供者台账分别记 `dear +28.00 / cheap +9.60`。也就是说价格项不是装饰，但它买不来"一定给最低价"，
    这是评分式而非价低者得的有意取舍，答辩要讲清楚。公网服务器上的数字还没有，部署完才能填。

## 往毕设方向扩展的路径

- **定价与信誉的联合优化**：现在单价由提供者自己填、信誉只是成功/失败计数。下一步把
  `ledger` 里的历史结算和 `nodes` 表的成功率喂进一个显式的评分函数，做 A/B：固定价格 vs
  信誉加权，看任务完成时间与失败重派率的差。数据表已经在了，这是纯策略层的活。
- **第二个任务画像**：Blender 按帧分片和转码按时间分片是同构问题——把 `Claim` 的
  `Start/Dur` 换成 `FrameFrom/FrameTo`，执行适配器从 ffmpeg 换成 blender CLI 即可复用整套调度
  （`Req.Runtimes` 已经在探测 python/ffmpeg 版本，加一个 blender 项就行）。
- **同时可用性模型**：Agent 上报的在线时段已经能算出来（节点上下线时间戳 + 每次 hello 的
  `last_seen`），模拟"需要 N 节点同时就绪"的成功率是纯离线分析，不需要真部署。
- **真钱**：`ledger` 已经是复式结构（预扣/结算/退还/收益四条流水 + 余额快照），接支付只需要
  把 `recharge` 换成网关回调，账本不用改。
