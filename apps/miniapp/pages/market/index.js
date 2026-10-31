const { api, f2, getToken } = require('../../utils/api.js')

Page({
  data: { buckets: [], nodes: [], loading: true, err: '' },

  onShow() {
    if (!getToken()) {
      wx.reLaunch({ url: '/pages/login/index' })
      return
    }
    this.load()
    this.timer = setInterval(() => this.load(), 6000)
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
      const [m, st] = await Promise.all([api.market(), api.state()])
      const buckets = (m.slices || []).map((b) => ({
        ...b,
        gpuTag: /nvenc|qsv|amf/.test(b.encoder),
        priceText: b.min_price === b.max_price ? f2(b.min_price) + ' 点/秒' : f2(b.min_price) + ' ~ ' + f2(b.max_price) + ' 点/秒',
        speedText: b.avg_speed > 0 ? f2(b.avg_speed) + '×' : '—',
        gpu: b.best_gpu || '无独显',
      }))
      const nodes = (st.workers || [])
        .filter((w) => w.online)
        .sort((a, b) => b.mine - a.mine || String(a.name || a.id).localeCompare(String(b.name || b.id), 'zh-CN'))
        .map((w) => {
          const gpus = (w.gpus || []).map((g) => g.name + ' ' + Math.round(g.mem_mb / 1024) + 'G').join(' + ')
          return {
            id: w.id,
            name: w.name || w.id,
            mine: !!w.mine,
            spec: w.cpus + ' 核 · ' + (w.mem_mb ? Math.round(w.mem_mb / 1024) + 'G' : '内存未知') + ' · ' + (gpus || '无独显'),
            runtimes: (w.runtimes || []).map((r) => r.name || r),
            encoders: w.encoders || [],
            slots: w.active + '/' + w.slots,
            speedText: w.speed > 0 ? f2(w.speed) + '×' : '—',
            priceText: f2(w.price) + ' 点/秒',
            reliability: w.ok + w.bad > 0 ? Math.round((100 * w.ok) / (w.ok + w.bad)) + '%' : '—',
          }
        })
      this.setData({ buckets, nodes, loading: false, err: '' })
    } catch (e) {
      this.setData({ loading: false, err: e.message || String(e) })
    }
  },
})
