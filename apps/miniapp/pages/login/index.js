const { api, setToken, getToken, baseUrl, setBase } = require('../../utils/api.js')

Page({
  data: {
    mode: 'login',
    name: '',
    pass: '',
    pass2: '',
    invite: '',
    msg: '',
    busy: false,
    base: '',
    showBase: false,
    inviteRequired: false,
    note: '本注册不赠送算力点。',
  },

  onShow() {
    if (getToken()) {
      wx.switchTab({ url: '/pages/submit/index' })
      return
    }
    this.setData({ base: baseUrl() })
    api
      .state()
      .then((st) => {
        const pol = st.policy || {}
        const inviteRequired = !!pol.signup_invite_required
        const note =
          (inviteRequired ? '本部署注册需要邀请码。' : '') +
          (pol.signup_credit > 0 ? '新用户注册赠送 ' + pol.signup_credit.toFixed(2) + ' 算力点。' : '本注册不赠送算力点。')
        this.setData({ inviteRequired, note })
      })
      .catch(() => {})
  },

  onInput(e) {
    this.setData({ [e.currentTarget.dataset.k]: e.detail.value })
  },

  setMode(e) {
    this.setData({ mode: e.currentTarget.dataset.mode, msg: '' })
  },

  toggleBase() {
    this.setData({ showBase: !this.data.showBase })
  },

  saveBase() {
    setBase(this.data.base)
    this.setData({ msg: '已保存，重新连接中…' })
    setTimeout(() => {
      this.setData({ msg: '' })
      this.onShow()
    }, 300)
  },

  async submit() {
    const { mode, name, pass, pass2, invite } = this.data
    if (!name.trim()) return this.setData({ msg: '请输入用户名' })
    if (mode === 'reg' && pass !== pass2) return this.setData({ msg: '两次密码不一致' })
    this.setData({ busy: true, msg: '正在校验…' })
    try {
      const r = mode === 'reg' ? await api.register(name.trim(), pass, invite.trim()) : await api.login(name.trim(), pass)
      setToken(r.token)
      this.setData({ busy: false, msg: '' })
      wx.switchTab({ url: '/pages/submit/index' })
    } catch (e) {
      this.setData({ busy: false, msg: '失败：' + (e.message || e) })
    }
  },
})
