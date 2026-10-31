App({
  globalData: {
    // pages write their timer handles here so onHide can stop polling
    timers: {},
  },
  onLaunch() {
    if (!wx.getStorageSync('ss-token')) {
      wx.reLaunch({ url: '/pages/login/index' })
    }
  },
})
