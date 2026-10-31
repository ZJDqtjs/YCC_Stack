export const JOB_ST = {
  pending: '待调度',
  leased: '已派发',
  fetching: '取源中',
  running: '转码中',
  uploading: '回传中',
  merging: '合成中',
  queued: '排队中',
  done: '完成',
  failed: '失败',
}

export const VM_ST = {
  queued: '排队中',
  provisioning: '启动中',
  running: '运行中',
  unreachable: '失联',
  stopping: '停机中',
  stopped: '已停机',
  failed: '失败',
}

export const LEDGER_KIND = {
  hold: '预扣',
  settle: '结算',
  refund: '退还',
  earn: '收益',
  recharge: '充值',
}

export const CAP_NAMES = {
  tools: '工具调用',
  json_object: 'JSON 输出',
  json_schema: 'JSON Schema',
  regex: '正则约束',
  vision: '图像输入',
  reasoning: '推理',
  deterministic: '确定性',
  prefix_cache: '前缀缓存',
}

export const QUALITY = {
  fast: { vcodec: 'libx264', preset: 'ultrafast', q: '28' },
  balanced: { vcodec: 'libx264', preset: 'fast', q: '26' },
  high: { vcodec: 'libx264', preset: 'slow', q: '20' },
}

export const INFLIGHT = ['fetching', 'leased', 'running', 'uploading']

// Tone shared by both clients: one word decides the color of a status pill.
export const toneOf = (status) =>
  status === 'done'
    ? 'ok'
    : status === 'failed'
      ? 'bad'
      : INFLIGHT.includes(status) || status === 'running'
        ? 'link'
        : status === 'queued' || status === 'pending' || status === 'provisioning'
          ? 'vio'
          : 'mute'
