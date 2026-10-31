const { api, f2, f1, baseUrl, setBase, clearSession, getToken } = require('../../utils/api.js')

Page({
  data: {
    me: null,
    balanceText: '—',
    nodes: [],
    base: '',
    adv: false,
    loading: true,
    msg: '',
    saveMsg: '',
  },

  onShow() {
    if (!getToken()) {
      wx.reLaunch({ url: '/pages/login/index' })
      return
    }
    this.setData({ base: baseUrl() })
    this.load()
  },

  async onPullDownRefresh() {
    await this.load()
    wx.stopPullDownRefresh()
  },

  async load() {
    try {
      const [me, st, settings] = await Promise.all([api.me(), api.state(), api.settings()])
      const nodes = (st.workers || [])
        .filter((w) => w.mine)
        .map((w) => ({
          id: w.id,
          name: w.name || w.id,
          online: !!w.online,
          meta: `${w.cpus} 核 · ${w.mem_mb ? Math.round(w.mem_mb / 1024) + 'G' : ''} · ${f2(w.price)} 点/秒 · 已赚 ${f2(w.earned || 0)}`,
        }))
      this.setData({ me, balanceText: f2(me.credits), nodes, adv: !!settings.advanced, loading: false, msg: '' })
    } catch (e) {
      this.setData({ loading: false, msg: e.message || String(e) })
    }
  },

  async toggleAdv(e) {
    const on = e.detail.value
    this.setData({ adv: on })
    try {
      const r = await api.saveSettings(on)
      this.setData({ adv: !!r.advanced })
    } catch (err) {
      this.setData({ adv: !on, msg: '保存失败：' + (err.message || err) })
    }
  },

  openNodeLogs(e) {
    wx.navigateTo({ url: '/pages/commlog/index?type=node&id=' + encodeURIComponent(e.currentTarget.dataset.id) })
  },

  onBase(e) {
    this.setData({ base: e.detail.value, saveMsg: '' })
  },

  saveBase() {
    setBase(this.data.base)
    this.setData({ saveMsg: '已保存，正在重连…' })
    setTimeout(() => wx.reLaunch({ url: '/pages/login/index' }), 500)
  },

  logout() {
    wx.showModal({
      title: '退出登录',
      content: '退出后需要重新输入用户名和密码。',
      success: async (r) => {
        if (!r.confirm) return
        try {
          await api.logout()
        } catch (e) {
          /* 会话可能已过期，仍按退出处理 */
        }
        clearSession()
        wx.reLaunch({ url: '/pages/login/index' })
      },
    })
  },
})
