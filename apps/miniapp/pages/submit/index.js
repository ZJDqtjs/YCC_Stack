const { api, f1, f2, QUALITY, getToken } = require('../../utils/api.js')

const KINDS = ['视频转码', 'Blender 渲染']
const QUALITIES = ['快速', '均衡（推荐）', '高质量']
const QUALITY_KEYS = ['fast', 'balanced', 'high']
const ENGINES = ['沿用场景设置', 'CYCLES', 'BLENDER_EEVEE_NEXT', 'BLENDER_EEVEE', 'BLENDER_WORKBENCH']
const ENGINE_VALUES = ['', 'CYCLES', 'BLENDER_EEVEE_NEXT', 'BLENDER_EEVEE', 'BLENDER_WORKBENCH']

Page({
  data: {
    kinds: KINDS,
    kindIndex: 0,
    qualities: QUALITIES,
    qualityIndex: 1,
    engines: ENGINES,
    engineIndex: 0,
    file: null,
    duration: 0,
    scene: '',
    samples: '0',
    from: '1',
    to: '120',
    fps: '24',
    percent: '0',
    quote: null,
    quoteErr: '选择文件后自动估算',
    msg: '',
    busy: false,
    progress: 0,
  },

  onShow() {
    if (typeof this.getTabBar === 'function' && this.getTabBar()) {
      this.getTabBar().setData({ selected: 0 })
    }
    if (!getToken()) {
      wx.reLaunch({ url: '/pages/login/index' })
      return
    }
    this.quote()
  },

  onHide() {
    clearTimeout(this._qt)
  },

  onUnload() {
    clearTimeout(this._qt)
  },

  onInput(e) {
    this.setData({ [e.currentTarget.dataset.k]: e.detail.value }, () => this.debounceQuote())
  },

  onKind(e) {
    this.setData({ kindIndex: Number(e.detail.value), quote: null }, () => this.debounceQuote())
  },

  onQuality(e) {
    this.setData({ qualityIndex: Number(e.detail.value) }, () => this.debounceQuote())
  },

  onEngine(e) {
    this.setData({ engineIndex: Number(e.detail.value) })
  },

  chooseFile() {
    const blender = this.data.kindIndex === 1
    if (blender) {
      wx.chooseMessageFile({
        count: 1,
        type: 'file',
        extension: ['zip', 'blend'],
        success: (r) => {
          const f = r.tempFiles[0]
          this.setData({ file: { path: f.path, name: f.name, size: f.size }, quote: null }, () => this.quote())
        },
      })
      return
    }
    wx.chooseMedia({
      count: 1,
      mediaType: ['video'],
      sourceType: ['album', 'camera'],
      success: (r) => {
        const f = r.tempFiles[0]
        this.setData(
          { file: { path: f.tempFilePath, name: 'video.mp4', size: f.size }, duration: Math.round(f.duration || 0), quote: null },
          () => this.quote(),
        )
      },
    })
  },

  debounceQuote() {
    clearTimeout(this._qt)
    this._qt = setTimeout(() => this.quote(), 350)
  },

  units() {
    const a = Number(this.data.from || 0)
    const b = Number(this.data.to || 0)
    return b - a + 1
  },

  decorate(q, head) {
    return {
      ...q,
      head: head || '',
      holdText: f2(q.hold),
      balanceText: f2(q.balance),
      priceText: f2(q.hold_price),
      ok: !!(q.affordable && q.eligible > 0),
    }
  },

  async quote() {
    const kind = this.data.kindIndex === 1 ? 'blender' : 'transcode'
    if (kind === 'blender') {
      const n = this.units()
      if (!(n > 0)) {
        this.setData({ quote: null, quoteErr: '填写起止帧后按帧数估算' })
        return
      }
      try {
        const q = await api.estimate({ duration: n, kind: 'blender', vcodec: 'libx264', req: {} })
        this.setData({ quote: this.decorate(q, `第 ${this.data.from}~${this.data.to} 帧，共 ${n} 帧`), quoteErr: '' })
      } catch (e) {
        this.setData({ quote: null, quoteErr: '估算失败：' + (e.message || e) })
      }
      return
    }
    const dur = Number(this.data.duration || 0)
    if (!(dur > 0)) {
      this.setData({ quote: null, quoteErr: '选择视频后自动估算' })
      return
    }
    try {
      const q = await api.estimate({ duration: dur, vcodec: 'libx264', req: {} })
      this.setData({ quote: this.decorate(q, ''), quoteErr: '' })
    } catch (e) {
      this.setData({ quote: null, quoteErr: '估算失败：' + (e.message || e) })
    }
  },

  async submit() {
    if (!this.data.file) return wx.showToast({ title: '请先选择文件', icon: 'none' })
    const kind = this.data.kindIndex === 1 ? 'blender' : 'transcode'
    const preset = QUALITY[QUALITY_KEYS[this.data.qualityIndex]] || QUALITY.balanced
    const formData =
      kind === 'blender'
        ? {
            req: '{}',
            segs: '4',
            kind: 'blender',
            scene: this.data.scene,
            engine: ENGINE_VALUES[this.data.engineIndex],
            samples: this.data.samples,
            from: this.data.from,
            to: this.data.to,
            percent: this.data.percent,
            fps: this.data.fps,
          }
        : { req: '{}', segs: '4', vcodec: preset.vcodec, preset: preset.preset, q: preset.q, height: '0', audio: '1', baseline: '0' }

    this.setData({ busy: true, progress: 0, msg: '上传中…' })
    const form = {
      filePath: this.data.file.path,
      name: 'file',
      formData,
      onProgress: (p) => this.setData({ progress: p, msg: `上传中 ${p}%` }),
    }
    try {
      const r = kind === 'blender' ? await api.submitRender(form) : await api.submitTranscode(form)
      const detail = kind === 'blender' ? `（${r.pieces} 段 / ${f1(r.units)} ${r.unit}）` : ''
      this.setData({ busy: false, msg: `已入队 ${r.id}${detail}，预扣 ${f2(r.hold)} 点` })
      wx.showToast({ title: '任务已提交', icon: 'success' })
      setTimeout(() => wx.switchTab({ url: '/pages/jobs/index' }), 600)
    } catch (e) {
      this.setData({ busy: false, msg: '失败：' + (e.message || e) })
    }
  },
})
