import type { CapacitorConfig } from '@capacitor/cli'

// androidScheme http keeps the WebView origin at http://localhost so the app can
// talk to the broker over plain HTTP on :8787 without hitting mixed-content rules.
// localhost is still a secure context, so clipboard and other web APIs keep working.
const config: CapacitorConfig = {
  appId: 'com.starstack.compute',
  appName: '星栈算力',
  webDir: 'mobile/dist',
  server: { androidScheme: 'http', cleartext: true },
  android: { allowMixedContent: true, backgroundColor: '#0b0b0c' },
}

export default config
