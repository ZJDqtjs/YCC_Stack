Component({
  data: {
    selected: 0,
    list: [
      { pagePath: '/pages/submit/index', text: '提交', iconPath: '/custom-tab-bar/icons/submit.png', selectedIconPath: '/custom-tab-bar/icons/submit-on.png' },
      { pagePath: '/pages/jobs/index', text: '任务', iconPath: '/custom-tab-bar/icons/jobs.png', selectedIconPath: '/custom-tab-bar/icons/jobs-on.png' },
      { pagePath: '/pages/market/index', text: '市场', iconPath: '/custom-tab-bar/icons/market.png', selectedIconPath: '/custom-tab-bar/icons/market-on.png' },
      { pagePath: '/pages/bill/index', text: '账单', iconPath: '/custom-tab-bar/icons/bill.png', selectedIconPath: '/custom-tab-bar/icons/bill-on.png' },
      { pagePath: '/pages/me/index', text: '我的', iconPath: '/custom-tab-bar/icons/me.png', selectedIconPath: '/custom-tab-bar/icons/me-on.png' },
    ],
  },
  methods: {
    switchTab(e) {
      const { index, path } = e.currentTarget.dataset
      if (this.data.selected === index) return
      wx.switchTab({ url: path })
      this.setData({ selected: index })
    },
  },
})
