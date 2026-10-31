const { api, f2, ts, LEDGER_KIND, getToken } = require('../../utils/api.js')

Page({
  data: { spend: [], earn: [], loading: true, err: '', amount: '100', canRecharge: false, msg: '', busy: false },

  onShow() {
    if (typeof this.getTabBar === 'function' && this.getTabBar()) {
      this.getTabBar().setData({ selected: 3 })
    }
    if (!getToken()) {
      wx.reLaunch({ url: '/pages/login/index' })
      return
    }
    this.load()
  },

  async onPullDownRefresh() {
    await this.load()
    wx.stopPullDownRefresh()
  },

  async load() {
    try {
      const [d, st] = await Promise.all([api.ledger(), api.state()])
      const spend = (d.spend || []).map((e) => ({
        ...e,
        kindText: LEDGER_KIND[e.kind] || e.kind,
        tsText: ts(e.ts),
        amountText: (Number(e.amount) >= 0 ? '+' : '') + f2(e.amount),
        pos: Number(e.amount) >= 0,
        balanceText: f2(e.balance),
      }))
      const earn = (d.earn || []).map((e) => ({
        ...e,
        tsText: ts(e.ts),
        amountText: '+' + f2(e.amount),
        nodeText: String(e.node_id || '节点').slice(0, 14),
      }))
      const pol = (st.policy || {})
      this.setData({ spend, earn, loading: false, err: '', canRecharge: !!pol.recharge })
    } catch (e) {
      this.setData({ loading: false, err: e.message || String(e) })
    }
  },

  onAmount(e) {
    this.setData({ amount: e.detail.value })
  },

  async recharge() {
    const n = Number(this.data.amount)
    if (!(n > 0)) return wx.showToast({ title: '请输入大于 0 的点数', icon: 'none' })
    this.setData({ busy: true, msg: '' })
    try {
      await api.recharge(n)
      wx.showToast({ title: '已充值', icon: 'success' })
      await this.load()
    } catch (e) {
      this.setData({ msg: '充值失败：' + (e.message || e) })
    } finally {
      this.setData({ busy: false })
    }
  },
})
