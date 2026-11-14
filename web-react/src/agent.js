// 星舷 Agent 意图引擎：把一句自然语言解析为可执行意图。
// 纯函数、无 DOM 依赖；React 与原生内嵌版共用同一套规则。
// 识别不了时回落 {type:'chat'} 交给模型对话。

const NAV_MAP = [
  ['我的任务', 'jobs'], ['任务', 'jobs'],
  ['钱包', 'wallet'], ['账单', 'wallet'], ['充值', 'wallet'],
  ['论坛', 'forum'], ['社区', 'forum'],
  ['设置', 'settings'],
  ['总览', 'home'], ['首页', 'home'], ['仪表盘', 'home'],
  ['算力市场', 'pool'], ['算力池', 'pool'],
  ['我的节点', 'nodes'], ['节点', 'nodes'], ['机器', 'nodes'],
  ['服务市场', 'market'], ['市场', 'market'],
  ['我的发布', 'sell'], ['发布', 'sell'],
  ['消息', 'messages'], ['会话', 'messages'],
  ['生图', 'image'], ['生视频', 'video'],
  ['渲染', 'render'], ['转码', 'render'],
  ['编排', 'build'], ['编译', 'build'],
  ['模型', 'api'], ['api', 'api'], ['密钥', 'api'], ['上游', 'api'],
]
export const VIEW_TITLE = {
  home: '总览', image: '生图', video: '生视频', render: '渲染', build: '编排',
  api: '模型 API', market: '服务市场', sell: '我的发布', messages: '消息',
  nodes: '我的节点', pool: '算力市场', jobs: '我的任务', wallet: '钱包账单',
  forum: '论坛', settings: '设置',
}
const strip = (t, re) => t.replace(re, '').trim()
const cleanPrompt = (t, re) => strip(t, re).replace(/(的)?(图|图片|图像|视频)$/, '').trim() || t

export function parseIntent(raw) {
  const t = String(raw || '').trim()
  if (!t) return { type: 'chat', text: t }
  const s = t.toLowerCase()
  const has = (...ks) => ks.some(k => s.includes(k))
  let m

  // 充值（带金额）
  if (has('充值', '充点', '加点', '添加算力点', '充 ')) {
    m = t.match(/(\d+(?:\.\d+)?)/)
    return { type: 'recharge', amount: m ? +m[1] : 100 }
  }
  // 余额查询
  if (has('余额', '还剩', '多少点', '多少算力', '查余额', '查询余额')) return { type: 'balance' }

  // 生视频（先于生图判断）
  if (has('视频') && has('生成', '做', '创作', '制作', '拍', '生视频')) {
    return { type: 'video', prompt: cleanPrompt(t, /^(帮我|请|麻烦)?(生成|做|创作|制作|拍)(一?(段|个|条))?(视频)?[:：,，]?\s*/) }
  }
  // 生图
  if (has('图', '图片', '图像') && has('生成', '画', '绘制', '创作', '出', '做')) {
    return { type: 'image', prompt: cleanPrompt(t, /^(帮我|请|麻烦)?(生成|画|绘制|创作|出|做)(一?(张|幅|个))?(图|图片|图像)?[:：,，]?\s*/) }
  }
  // 渲染 / 转码
  if (has('转码', '压缩视频', '渲染', 'blender')) {
    return { type: 'render', kind: has('blender', '渲染') ? 'blender' : 'transcode' }
  }
  // 编译 / 编排
  if (has('编译', '打包', '编排', '构建')) return { type: 'build' }

  // 市场搜索
  if (has('市场', '挂牌', '服务') && has('搜索', '搜', '找', '筛选')) {
    const kw = strip(t, /^.*(搜索|搜|找)/).replace(/(市场|服务|挂牌|里|中|的|一下)/g, '').trim()
    return { type: 'marketSearch', q: kw }
  }
  // 租云主机
  if ((has('租', '租用', '租赁') && has('云主机', '虚拟机', '主机', 'vm')) || (has('云主机', '虚拟机') && has('租'))) {
    return { type: 'vmRent' }
  }
  // 发布服务
  if (has('发布', '上架', '出售', '卖') && has('服务', '挂牌', '商品')) {
    return { type: 'sellPublish', title: strip(t, /^.*(发布|上架|出售|卖)(一个|个)?(服务|挂牌|商品)?[:：,，]?\s*/) }
  }
  // 发帖
  if (has('发帖', '发帖子', '发布帖子', '论坛发', '发一个帖')) {
    return { type: 'forumPost', title: strip(t, /^.*(发帖|发帖子|发布帖子|发一个帖|论坛发)(子)?[:：,，]?\s*/) }
  }
  // 创建 API Key
  if (has('创建', '新建', '生成') && has('api key', 'apikey', '密钥', 'key')) {
    return { type: 'createKey', name: strip(t, /^.*(创建|新建|生成)(一个|个)?(api ?key|密钥|key)?[:：,，]?\s*/) }
  }
  // 任务状态（inline 摘要）
  if (has('任务') && has('状态', '怎么样', '进度', '有哪些', '列表', '查看', '看看', '运行', '进行中')) return { type: 'tasks' }
  // 节点状态（inline 摘要）
  if ((has('节点', '机器') && has('在线', '状态', '怎么样', '收益', '列表', '查看', '看看')) || has('我的节点')) return { type: 'nodes' }

  // 通用导航
  if (has('打开', '进入', '跳转', '切换到', '去', '到', '看看', '查看', '逛')) {
    for (const [kw, view] of NAV_MAP) if (s.includes(kw)) return { type: 'nav', view, tab: view === 'market' && has('云主机', '虚拟机') ? 'vm' : null }
  }
  // 裸视图名
  for (const [kw, view] of NAV_MAP) if (s === kw || s === kw + '页' || s === '去' + kw) return { type: 'nav', view, tab: null }

  return { type: 'chat', text: t }
}