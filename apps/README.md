# 星栈算力 · 移动端与小程序

同一套后端接口的两个客户端：

- `mobile/` — React + Vite + Capacitor 的安卓 App，覆盖控制台全部 9 个视图。
- `miniapp/` — 原生微信小程序，覆盖需求方的 5 个页面 + 登录。
- `harmony/` — HarmonyOS NEXT（API 12+）ArkTS 壳，Web 组件加载同一份 React 构建产物。
- `shared/` — 两个客户端共用的接口客户端、类型约定、状态文案与格式化函数。

后端已支持 `Authorization: Bearer <session token>`（登录响应里新增 `token` 字段）与 `/api/*` 的 CORS 预检，浏览器 Cookie 会话保持原样。

## 目录

```
apps/
  shared/            api.js / constants.js / format.js（ESM，两端共用）
  mobile/src/        React 页面：Auth, Submit, Jobs, Market, Vm, Nodes, ApiView, Bill, Forum, Me
  mobile/dist/       构建产物，被打进 APK 的本地资源
  miniapp/           原生小程序；utils/core.js 由 shared/ 打包生成
  android/           Capacitor 生成的安卓工程
  scripts/           verify-miniapp.cjs（Node 侧校验小程序）
```

## 安卓

```bash
cd apps
npm install

# 浏览器里调 UI（Vite 代理到 STARSTACK_BROKER，默认公网演练环境）
STARSTACK_BROKER=http://127.0.0.1:8787 npm run dev   # http://127.0.0.1:5173

# 构建网页产物并同步进原生工程
npm run build
npx cap sync android

# 打 debug APK（需要 JDK 17 与 Android SDK）
cd android && ./gradlew assembleDebug
# 产物：android/app/build/outputs/apk/debug/app-debug.apk
```

装到设备/模拟器：

```bash
adb install -r android/app/build/outputs/apk/debug/app-debug.apk
adb shell am start -n com.starstack.compute/.MainActivity
```

- APK 内是本地资源，不依赖远程网页；默认连 `http://20.24.210.119:8787`，可在登录页或「我的 → 服务器地址」改。
- `capacitor.config.ts` 里 `androidScheme: 'http'` + `cleartext: true`，否则 `https://localhost` 的页面会被混合内容规则拦住明文请求；`localhost` 仍是安全上下文，剪贴板等 API 可用。
- Gradle 分发包在 `android/gradle/wrapper/gradle-wrapper.properties` 指向腾讯镜像（`services.gradle.org` 在部分网络下会重置连接）；换回官方源删掉该行即可。
- 文件上传走 WebView 的原生选择器，大文件没有分片续传，移动端建议只传小样。

## 小程序

1. 用微信开发者工具「导入项目」，目录选 `apps/miniapp`，AppID 可用测试号（`project.config.json` 里是 `touristappid`）。
2. 详情 → 本地设置 → 勾选「不校验合法域名、web-view、TLS 版本以及 HTTPS 证书」，即可直连 HTTP 演示环境。
3. 登录页可改服务器地址；账号与网页控制台通用。

覆盖范围（小程序是需求方的「遥控器」，不是全功能端）：

| 页面 | 能力 |
|---|---|
| 提交 | 视频转码（选视频自动读时长）与 Blender 帧区间渲染；单文件上传、带进度 |
| 任务 | 任务卡、分片进度、哈希校验、结算；下载链接复制到剪贴板 |
| 市场 | 按编码能力聚合 + 在线节点（只读） |
| 账单 | 流水、节点收益、自助充值（部署允许时） |
| 我的 | 余额、我的节点（只读）、高级选项、服务器地址、退出 |

刻意不做：云电脑（无 WebRTC/键鼠）、模型上游登记（长表单）、论坛（多图/视频）、节点改价与一键接入（需要下载并运行本机脚本）。

正式版上线前必须：域名备案 + HTTPS + 在微信后台配置 request 合法域名；`http://20.24.210.119:8787` 只能用于开发与体验版。

`utils/core.js` 是 `shared/` 的 CommonJS 打包产物，改完 `shared/` 后重新生成：

```bash
npm run sync:core
```

## 校验

```bash
# 页面注册、WXML 表达式、模块加载，并用 Node 模拟 wx.* 跑通登录/估算/上传/账单
STARSTACK_SAMPLE=/path/to/sample.mp4 STARSTACK_BROKER=http://127.0.0.1:8787 \
  node scripts/verify-miniapp.cjs

cd apps && npx tsc --noEmit && npm run build
```

## 编译打包（共享算力任务）

平台新增 `build` 任务类型：买方上传源码 zip，节点解压后执行命令，产物打包成 `result.zip` 回传，按实际墙钟分钟结算（1 分钟下限），失败不结算。

- 提交：控制台「提交任务 → 编译打包」，或 `POST /api/jobs/build`（multipart：file / command / output / timeout / req）。
- 能力匹配：节点必须以 `--allow-build` 启动并上报 `build` 运行时，调度器只会把编译任务派给这类节点；转码/渲染仍按编码器与运行时匹配。
- 白名单：broker 以 `--allow-build --build-users a,b`（或 `STARSTACK_BUILD_USERS`）开启，未列名的账号无法提交。
- 缺环境自动补齐：启动脚本会为白名单账号自动加 `--allow-build --auto-provision`；任务脚本也可自行引导安装（参考本次桌面端 `build.cmd` 安装 .NET SDK 到 `%LOCALAPPDATA%\starstack-sdk`）。
- 产物：`GET /download/<job-id>`，zip 内含 `build.log`。

本次桌面端就是用这条链路构建的：源码包 → 1145 节点 → `dotnet publish` → `result.zip` → 在本机安装。

### 桌面端产物形态

`desktop/build.cmd` 现在发布**自包含单文件**：

```
dotnet publish StarStack.Desktop.csproj -c Release -r win-x64 \
  --self-contained true -p:PublishSingleFile=true \
  -p:IncludeNativeLibrariesForSelfExtract=true -p:EnableCompressionInSingleFile=true
```

产物目录里只有一个 `StarStack.Desktop.exe`（约 70–90 MB），**双击即可运行**，不需要预装 .NET 运行时，也不需要先解压一堆 DLL。`install.bat` / `install.ps1` 只在需要开始菜单快捷方式和固定安装目录时使用，属于备选。
