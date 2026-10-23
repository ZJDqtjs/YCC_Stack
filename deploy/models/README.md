# 模型上游：分布式与 MoE 部署

星栈的模型网关不跑推理内核，它只做**路由、计量和结算**。只要推理端暴露 OpenAI 兼容的
`GET /v1/models` 与 `POST /v1/chat/completions`，就能在控制台的「模型 API → 接入模型上游」
里登记，成为平台的一个模型供给方。买家拿到的是一个统一的 `sk-ss-...` 密钥和同一个
`/v1` 地址，不需要知道请求最终落在哪台机器。

```
买家 ── sk-ss-key ──> Broker /v1/chat/completions
                         │  按模型选最低价且健康的上游
                         ├── llama.cpp / vLLM / Ollama（本机或内网）
                         └── 另一家提供方的公网端点
                         │
                         ├─ 预扣 max_tokens 上限
                         ├─ 透传响应 / SSE
                         └─ 按 usage 结算：90% 给上游提供方，10% 平台撮合费
```

## 1. 选择部署形态

| 场景 | 方案 | 入口 |
|---|---|---|
| 单卡放得下 | llama.cpp / vLLM / Ollama | `http://host:port` |
| 单机显存不足，MoE 专家层多 | llama.cpp `--n-cpu-moe N`，专家放内存、注意力留 GPU | `http://host:8080` |
| 多机都有 GPU，想聚合成一个模型 | 每台 `rpc-server` + 主节点 `--rpc` | 主节点 `http://host:8080` |
| 多卡同一台机，稠密模型 | vLLM `--tensor-parallel-size N` | `http://host:8000` |
| 已有 Ollama | `OLLAMA_HOST=0.0.0.0 ollama serve` | `http://host:11434` |

脚本都在本目录，环境变量即配置：

```bash
# 单机 MoE：24 层专家放 CPU
MODEL=./Qwen3-30B-A3B-Q4_K_M.gguf N_CPU_MOE=24 bash llama-moe.sh

# 多机聚合：10/11 两台机器先各跑 rpc-server
LLAMA_BIN_DIR=~/llama.cpp/build/bin RPC_PORT=50052 bash llama-rpc.sh
MODEL=./Qwen3-30B-A3B-Q4_K_M.gguf \
  RPC=192.168.1.10:50052,192.168.1.11:50052 N_CPU_MOE=16 bash llama-distributed.sh

# vLLM 双卡
MODEL=Qwen/Qwen3-30B-A3B TP=2 bash vllm-tp.sh

# Ollama
MODEL=qwen3:30b HOST=0.0.0.0 bash ollama.sh
```

> `--n-cpu-moe` 是 llama.cpp 近期版本才有的参数，旧构建会直接报 unknown argument。
> 先 `llama-server --help | grep n-cpu-moe` 确认；没有就升级 llama.cpp，或改用
> vLLM 的 `--cpu-offload-gb` / 多卡 TP。

## 2. 通过公网让 Broker 访问

Broker 在公网（例如 Azure），模型在宿舍 NAT 后面时，三种做法任选：

- 云服务器到宿舍的 WireGuard / Tailscale 组网，登记内网地址；
- `cloudflared tunnel --url http://127.0.0.1:8080` 之类临时隧道的公网域名；
- 有公网 IP 时直接 `http://公网IP:8080`，并只放行 Broker 出口 IP。

**安全提示**：`--public` 档会拒绝把上游指向 RFC1918 私网、链路本地和云元数据地址；
`127.0.0.1` 允许，因为运维可能把模型服务就放在 Broker 本机，且网关只会向后拼接固定的
`/v1/models`、`/v1/chat/completions` 路径。上行请求会带提供方登记的 API Key；如果模型
服务暴露在公网，请务必给 llama.cpp 加 `--api-key`、给 vLLM 加 `--api-key`，或只允许
Broker 的来源 IP。

## 3. 登记到平台

登录控制台 →「模型 API」→「接入模型上游」：

- **Base URL** 填到端口即可（`http://192.168.1.20:8080`），带不带 `/v1` 都行；
- **提供的模型名** 必须与推理端 `/v1/models` 返回的 id 一致（如 `qwen3-30b-a3b`）；
- **单价** 是提供方每 1000 token 的报价，买家实付同价，提供方拿 90%；
- 「探测」会请求上游的 `/v1/models`，成功后再登记。

也可以直接走 API（Cookie 来自登录）：

```bash
# 登录并保存会话
curl -c /tmp/ss.jar -X POST http://BROKER/api/auth/login \
  -H 'Content-Type: application/json' -d '{"name":"provider","password":"..."}'

# 登记上游
curl -b /tmp/ss.jar -X POST http://BROKER/api/models/upstreams \
  -H 'Content-Type: application/json' -d '{
    "name":"宿舍 4090 · Qwen3-30B-A3B",
    "base_url":"http://192.168.1.20:8080",
    "api_key":"llama-server 的 --api-key（没有就留空）",
    "models":"qwen3-30b-a3b",
    "price":0.6
  }'
```

上游 API Key 用 Broker 数据目录下的 `gateway.key` 做 AES-GCM 加密后存库。备份
`starstack.db` 时不要漏掉这个文件；反过来，`gateway.key` 泄露等于上游密钥泄露。
轮换它会让已登记的上游密钥无法解密，需要重新登记。

## 4. 买家调用

先在「模型 API」创建 `sk-ss-...` 密钥，然后：

```bash
curl http://BROKER/v1/chat/completions \
  -H "Authorization: Bearer sk-ss-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3-30b-a3b","messages":[{"role":"user","content":"你好"}]}'

# 流式
curl -N http://BROKER/v1/chat/completions \
  -H "Authorization: Bearer sk-ss-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3-30b-a3b","stream":true,
       "messages":[{"role":"user","content":"写一段 Go 并发示例"}]}'
```

OpenAI SDK 只需改 `base_url` 和 `api_key`：

```python
from openai import OpenAI
c = OpenAI(base_url="http://BROKER/v1", api_key="sk-ss-...")
r = c.chat.completions.create(model="qwen3-30b-a3b",
    messages=[{"role": "user", "content": "你好"}])
print(r.choices[0].message.content)
```

## 5. 计费与故障转移

1. 请求进来先按 `messages` 估算 prompt token；`max_tokens` 缺省 1024、上限 8192。
2. 按候选上游中的**最高价**预扣 `(prompt + max_tokens) / 1000 × 单价`，余额不足直接 402。
3. 按价格从低到高尝试健康上游；连接失败、5xx、429 会标记冷却 20 秒并切下一家，
   4xx 视为请求本身无效，原样透传并全额退还预扣。
4. 响应结束后用上游 `usage` 结算；上游不返回 usage 时按输出文本估算，差额多退少补。
5. 买家扣费后，90% 记入上游提供方账户（账单里是「收益」），10% 留在平台。

流式请求会向上游注入 `stream_options.include_usage`；如果上游忽略 `stream=true` 返回
普通 JSON，网关也能识别并按非流式结算。

## 6. 常驻运行（systemd 示例）

```ini
[Unit]
Description=llama.cpp MoE server for StarStack
After=network.target

[Service]
User=model
WorkingDirectory=/opt/models
Environment=MODEL=/opt/models/Qwen3-30B-A3B-Q4_K_M.gguf
Environment=N_CPU_MOE=24
Environment=API_KEY=换成你的上游密钥
ExecStart=/opt/llama.cpp/build/bin/llama-server -m ${MODEL} \
  --host 0.0.0.0 --port 8080 -c 8192 -ngl 99 --n-cpu-moe ${N_CPU_MOE} \
  --api-key ${API_KEY} --jinja
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

多机时每台 `rpc-server` 也各自用 systemd 常驻，并在主节点的 `ExecStart` 里加
`--rpc 10.0.0.10:50052,10.0.0.11:50052`。
