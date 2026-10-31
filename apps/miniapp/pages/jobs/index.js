const { api, f1, f2, JOB_ST, toneOf, INFLIGHT } = require('../../utils/api.js')

function decorate(j) {
  const segs = j.segs || []
  const done = segs.filter((s) => s.status === 'done').length
  const verified = segs.filter((s) => s.match).length
  const speedup = !j.resumed && j.wall > 0 ? j.sum_wall / j.wall : 0
  const req = j.req || {}
  const rq = []
  if (req.need_gpu) rq.push('需 GPU')
  if (req.min_cpus) rq.push('≥' + req.min_cpus + ' 核')
  if (req.runtime && j.kind !== 'build') rq.push('需 ' + req.runtime)
  if (req.max_price) rq.push('≤' + f2(req.max_price) + ' 点/秒')

  const tags = []
  if (j.kind === 'build') {
    tags.push('编译打包')
    if (j.spec && j.spec.command) tags.push(String(j.spec.command).slice(0, 40))
    tags.push('超时 ' + ((j.spec && j.spec.timeout_sec) || 1800) + 's')
  } else if (j.kind === 'blender') {
    tags.push('Blender' + (j.spec && j.spec.engine ? ' · ' + j.spec.engine : ''))
    tags.push(Math.round(j.duration) + ' 帧 · ' + segs.length + ' 段')
  } else if (j.kind === 'pr') {
    tags.push('时间线渲染')
    tags.push(f1(j.duration) + 's · ' + segs.length + ' 片')
  } else {
    const p = j.profile || {}
    tags.push(p.vcodec + ' · Q' + p.q + ' · ' + p.preset)
    tags.push(p.height ? p.height + 'p' : '原始分辨率')
    tags.push(f1(j.duration) + 's · ' + segs.length + ' 片')
  }

  let stats = [
    { k: '墙钟', v: f1(j.wall) + 's' },
    { k: '分片完成', v: done + '/' + segs.length },
    { k: '哈希一致', v: verified + '/' + segs.length },
    { k: '并行比', v: speedup > 0 ? f2(speedup) + 'x' : '—' },
    { k: '实结算', v: f2(j.credits_spent) },
    { k: '预扣', v: f2(j.credits_hold) },
  ]
  if (j.kind === 'build') stats = [
    { k: '编译耗时', v: f1(j.wall) + 's' },
    { k: '产物校验', v: verified + '/' + segs.length },
    { k: '实结算', v: f2(j.credits_spent) },
  ]
  if (j.released && j.credits_hold > j.credits_spent) stats.push({ k: '退还', v: '+' + f2(j.credits_hold - j.credits_spent) })
  if (j.baseline) {
    stats.push({ k: '单机基线', v: j.baseline.status === 'done' ? f1(j.baseline.wall) + 's' : j.baseline.status === 'running' ? '运行中' : '排队' })
  }

  return {
    ...j,
    statusText: j.kind === 'build' && j.status === 'running' ? '编译中' : (JOB_ST[j.status] || j.status),
    tone: toneOf(j.status),
    progress: segs.length ? Math.round((done / segs.length) * 100) : 0,
    tags,
    rq,
    stats,
    canDownload: j.status === 'done',
    segs: segs.map((s) => ({
      id: s.id || s.index,
      index: s.index,
      letter: s.status === 'done' ? 'done' : s.status === 'failed' ? 'failed' : INFLIGHT.includes(s.status) ? 'run' : 'wait',
      worker: (s.worker || '—').slice(0, 8),
    })),
  }
}

Page({
  data: { jobs: [], loading: true, err: '', me: null },

  onShow() {
    this.load()
    this.timer = setInterval(() => this.load(), 4000)
  },

  onHide() {
    clearInterval(this.timer)
  },

  onUnload() {
    clearInterval(this.timer)
  },

  async onPullDownRefresh() {
    await this.load()
    wx.stopPullDownRefresh()
  },

  async load() {
    try {
      const d = await api.jobs()
      this.setData({ jobs: (d.jobs || []).map(decorate), loading: false, err: '' })
    } catch (e) {
      this.setData({ loading: false, err: e.message || String(e) })
    }
  },

  openLogs(e) {
    wx.navigateTo({ url: '/pages/commlog/index?type=job&id=' + encodeURIComponent(e.currentTarget.dataset.id) })
  },

  download(e) {
    const j = this.data.jobs[e.currentTarget.dataset.i]
    const url = api.downloadUrl(j.id, e.currentTarget.dataset.which || '')
    wx.setClipboardData({
      data: url,
      success: () => wx.showToast({ title: '下载链接已复制', icon: 'none' }),
    })
  },
})
