const core = require('./core.js')

// 小程序的合法域名白名单需要 HTTPS 的已备案域名；开发/真机调试时在开发者工具里
// 关闭「不校验合法域名」即可直连演示环境的 HTTP 地址。
const DEFAULT_BASE = 'http://20.24.210.119:8787'

function baseUrl() {
  return wx.getStorageSync('ss-base') || DEFAULT_BASE
}

function setBase(value) {
  wx.setStorageSync('ss-base', String(value || '').trim().replace(/\/+$/, ''))
}

function getToken() {
  return wx.getStorageSync('ss-token') || ''
}

function setToken(token) {
  if (token) wx.setStorageSync('ss-token', token)
}

function clearSession() {
  wx.removeStorageSync('ss-token')
}

let redirected = false
function onUnauthorized(url) {
  if (url.indexOf('/api/auth/') >= 0) return
  clearSession()
  if (redirected) return
  redirected = true
  wx.showToast({ title: '登录已过期', icon: 'none' })
  setTimeout(() => wx.reLaunch({ url: '/pages/login/index' }), 400)
}

function request({ url, method, headers, body }) {
  return new Promise((resolve, reject) => {
    wx.request({
      url,
      method,
      header: headers,
      data: body,
      timeout: 20000,
      success(res) {
        if (res.statusCode === 401) onUnauthorized(url)
        const text = typeof res.data === 'string' ? res.data : JSON.stringify(res.data)
        resolve({ status: res.statusCode, text })
      },
      fail(err) {
        reject(new Error(err.errMsg || '网络请求失败'))
      },
    })
  })
}

// form = { filePath, name, formData, onProgress }；wx.uploadFile 一次只能带一个文件。
function upload({ url, method, headers, form }) {
  return new Promise((resolve, reject) => {
    const task = wx.uploadFile({
      url,
      filePath: form.filePath,
      name: form.name || 'file',
      header: headers,
      formData: form.formData || {},
      timeout: 60000,
      success(res) {
        if (res.statusCode === 401) onUnauthorized(url)
        resolve({ status: res.statusCode, text: res.data })
      },
      fail(err) {
        reject(new Error(err.errMsg || '上传失败'))
      },
    })
    if (form.onProgress && task.onProgressUpdate) {
      task.onProgressUpdate((e) => form.onProgress(e.progress))
    }
  })
}

const api = core.createApi({ baseUrl, getToken, request, upload })

module.exports = Object.assign({}, core, {
  api,
  baseUrl,
  setBase,
  getToken,
  setToken,
  clearSession,
  DEFAULT_BASE,
})
