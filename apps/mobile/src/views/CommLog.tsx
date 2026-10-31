import React, { useEffect, useState } from 'react'
import { ts, f1, f2 } from '../../../shared/index.js'
import { Empty, Loader, Sheet, Tag } from '../ui'

export const EVENT_LABEL: Record<string, string> = {
  'job.created': '任务创建',
  'job.done': '任务完成',
  'job.failed': '任务失败',
  'job.cancelled': '任务取消',
  'seg.dispatched': '派发给节点',
  'seg.fetching': '节点取源',
  'seg.running': '节点开始执行',
  'seg.uploading': '节点回传',
  'seg.done': '分片完成',
  'seg.failed': '分片失败',
  'seg.settled': '结算',
}

const ACTOR_LABEL: Record<string, string> = { buyer: '买方', seller: '卖方', server: '平台' }
const ACTOR_TONE: Record<string, string> = { buyer: 'link', seller: 'vio', server: 'mute' }

export function detailText(e: any) {
  try {
    const d = JSON.parse(e.detail || '{}')
    const parts: string[] = []
    if (d.kind) parts.push('类型 ' + d.kind)
    if (d.name) parts.push(String(d.name).slice(0, 40))
    if (d.hold !== undefined) parts.push('预扣 ' + f2(d.hold) + ' 点')
    if (d.index !== undefined) parts.push('#' + d.index)
    if (d.dur) parts.push(f1(d.dur) + ' 秒')
    if (d.price) parts.push(f2(d.price) + ' 点/单位')
    if (d.wall !== undefined) parts.push('耗时 ' + f1(d.wall) + ' 秒')
    if (d.match !== undefined) parts.push(d.match ? '哈希一致' : '哈希不一致')
    if (d.cost !== undefined) parts.push('结算 ' + f2(d.cost) + ' 点')
    if (d.spent !== undefined) parts.push('实结算 ' + f2(d.spent) + ' 点')
    if (d.refunded !== undefined) parts.push('退还 ' + f2(d.refunded) + ' 点')
    if (d.attempt) parts.push('第 ' + d.attempt + ' 次')
    if (d.err) parts.push(String(d.err).slice(0, 160))
    return parts.join(' · ')
  } catch {
    return e.detail || ''
  }
}

export function CommTimeline({ logs }: { logs: any[] }) {
  if (!logs.length) return <Empty>暂无通讯记录</Empty>
  return (
    <div className="list">
      {logs.map((e) => (
        <div className="listrow" key={e.id} style={{ display: 'block' }}>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <span className="name">{EVENT_LABEL[e.event] || e.event}</span>
            <Tag tone={ACTOR_TONE[e.actor] || ''}>{ACTOR_LABEL[e.actor] || e.actor}</Tag>
            {e.node_id && <Tag>{e.node_id}</Tag>}
          </div>
          <div className="meta" style={{ marginTop: 4 }}>
            {ts(e.ts)}
            {e.seg_id ? ' · ' + e.seg_id : ''}
          </div>
          {detailText(e) && <div className="hint" style={{ marginTop: 4 }}>{detailText(e)}</div>}
        </div>
      ))}
    </div>
  )
}

export function CommSheet({ open, title, loader, onClose }: { open: boolean; title: string; loader: () => Promise<any>; onClose: () => void }) {
  const [logs, setLogs] = useState<any[] | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    if (!open) return
    setLogs(null)
    setErr('')
    loader()
      .then((d: any) => setLogs(d.logs || []))
      .catch((e: any) => setErr(e.message || String(e)))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  return (
    <Sheet open={open} title={title} onClose={onClose}>
      {err ? <div className="err">{err}</div> : logs === null ? <Loader /> : <CommTimeline logs={logs} />}
    </Sheet>
  )
}
