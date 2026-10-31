import { createApi } from '../../shared/index.js'

const DEFAULT_BASE = 'http://20.24.210.119:8787'

export const store = {
  get: (k: string) => {
    try {
      return localStorage.getItem(k) || ''
    } catch {
      return ''
    }
  },
  set: (k: string, v: string) => {
    try {
      if (v) localStorage.setItem(k, v)
      else localStorage.removeItem(k)
    } catch {
      /* private mode */
    }
  },
}

// In dev the Vite proxy forwards /api, so an empty base keeps requests same-origin.
const devProxy = location.port === '5173'

export function readBase(): string {
  const saved = store.get('ss-base')
  if (saved) return saved
  return devProxy ? '' : DEFAULT_BASE
}

export function writeBase(base: string) {
  store.set('ss-base', base.trim().replace(/\/+$/, ''))
}

async function request({ url, method, headers, body }: any) {
  const res = await fetch(url, { method, headers, body })
  return { status: res.status, text: await res.text() }
}

async function upload({ url, method, headers, form }: any) {
  const res = await fetch(url, { method, headers, body: form })
  return { status: res.status, text: await res.text() }
}

export const api = createApi({
  baseUrl: readBase,
  getToken: () => store.get('ss-token'),
  request,
  upload,
})

export function openExternal(url: string) {
  const w = window.open(url, '_system')
  if (!w) window.open(url, '_blank')
}

// Downloads go through fetch + a blob link rather than window.open: inside the
// desktop WebView a popup is blocked, and the system browser does not carry this
// client's Bearer token. The native Android shell keeps the external handoff.
export async function downloadArtifact(url: string, filename: string) {
  const cap = (window as any).Capacitor
  if (cap && typeof cap.isNativePlatform === 'function' && cap.isNativePlatform()) {
    openExternal(url)
    return
  }
  const token = store.get('ss-token')
  const res = await fetch(url, { headers: token ? { Authorization: 'Bearer ' + token } : {} })
  if (!res.ok) {
    let detail = ''
    try {
      detail = await res.text()
    } catch {
      /* binary or empty body */
    }
    throw new Error(detail || 'HTTP ' + res.status)
  }
  const blob = await res.blob()
  const href = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = href
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(href), 10000)
}

export async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}
