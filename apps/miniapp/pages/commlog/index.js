const { api, ts, f1, f2 } = require('../../utils/api.js')

const EVENT = {
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
const ACTOR = { buyer: '买方', seller: '卖方', server: '平台' }
const TONE = { buyer: 'link', seller: 'vio', server: '' }

function detailText(e) {
  try {
    const d = JSON.parse(e.detail || '{}')
    const p = []
    if (d.kind) p.push('类型 ' + d.kind)
    if (d.name) p.push(String(d.name).slice(0, 40))
    if (d.hold !== undefined) p.push('预扣 ' + f2(d.hold) + ' 点')
    if (d.index !== undefined) p.push('#' + d.index)
    if (d.dur) p.push(f1(d.dur) + ' 秒')
    if (d.price) p.push(f2(d.price) + ' 点/单位')
    if (d.wall !== undefined) p.push('耗时 ' + f1(d.wall) + ' 秒')
    if (d.match !== undefined) p.push(d.match ? '哈希一致' : '哈希不一致')
    if (d.cost !== undefined) p.push('结算 ' + f2(d.cost) + ' 点')
    if (d.spent !== undefined) p.push('实结算 ' + f2(d.spent) + ' 点')
    if (d.refunded !== undefined) p.push('退还 ' + f2(d.refunded) + ' 点')
    if (d.attempt) p.push('第 ' + d.attempt + ' 次')
    if (d.err) p.push(String(d.err).slice(0, 160))
    return p.join(' · ')
  } catch (x) {
    return e.detail || ''
  }
}

Page({
  data: { logs: [], loading: true, err: '', title: '通讯日志' },

  onLoad(q) {
    this._q = q || {}
    const type = this._q.type === 'node' ? 'node' : 'job'
    const id = this._q.id || ''
    this.setData({ title: (type === 'node' ? '节点' : '任务') + '通讯日志' })
    const p = type === 'node' ? api.nodeLogs(id) : api.jobLogs(id)
    p.then((d) => {
      const logs = (d.logs || []).map((e) => ({
        ...e,
        eventText: EVENT[e.event] || e.event,
        actorText: ACTOR[e.actor] || e.actor,
        tone: TONE[e.actor] || '',
        timeText: ts(e.ts),
        detailText: detailText(e),
      }))
      this.setData({ logs, loading: false })
    }).catch((e) => this.setData({ err: e.message || String(e), loading: false }))
  },

  onPullDownRefresh() {
    const q = this._q || {}
    this.setData({ loading: true, err: '' })
    const type = q.type === 'node' ? 'node' : 'job'
    const p = type === 'node' ? api.nodeLogs(q.id) : api.jobLogs(q.id)
    p.then((d) => {
      const logs = (d.logs || []).map((e) => ({
        ...e,
        eventText: EVENT[e.event] || e.event,
        actorText: ACTOR[e.actor] || e.actor,
        tone: TONE[e.actor] || '',
        timeText: ts(e.ts),
        detailText: detailText(e),
      }))
      this.setData({ logs, loading: false })
    }).catch((e) => this.setData({ err: e.message || String(e), loading: false }))
      .then(() => wx.stopPullDownRefresh())
  },
})
