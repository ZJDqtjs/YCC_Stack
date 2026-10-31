import { webview } from '@kit.ArkWeb';

// The console is the same React build the desktop and Android clients embed.
// Keeping it in rawfile means the HarmonyOS shell needs no server of its own;
// the API still goes to the broker over CORS + Bearer.
@Entry
@Component
struct Index {
  controller: webview.WebviewController = new webview.WebviewController();

  build() {
    Column() {
      Web({ src: $rawfile('web/index.html'), controller: this.controller })
        .javaScriptAccess(true)
        .domStorageAccess(true)
        .fileAccess(true)
        .imageAccess(true)
        .zoomAccess(false)
        .width('100%')
        .height('100%')
    }
    .width('100%')
    .height('100%')
  }
}
