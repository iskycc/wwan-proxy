# WWAN Control 前端

React 19、TypeScript、Ant Design 6、Ant Design Charts 和 Vite。Go API 与 SQLite 保持现有协议，生产静态资源全部内嵌在二进制中，不依赖 CDN。

## 开发

需要 Node.js 22.12+，推荐 Node.js 24。

```bash
npm ci
npm run dev
# 使用其他 API 地址：
WWAN_API_URL=http://127.0.0.1:9091 npm run dev
```

先启动 Go 服务，再访问 Vite 打印的开发地址。HTTP API 和 WebSocket 都经同源代理转发。页面通过 URL hash 导航，刷新和直接打开页面不需要服务端路由回退。

## 构建

```bash
npm run build
```

构建先执行 TypeScript 校验，再更新 `../internal/webui/static/`。提交前端修改时一并更新这些静态产物，以便只安装 Go 的环境直接编译。各功能页和图表按需加载，依赖版本由 `package-lock.json` 锁定。

## 浏览器验证

```bash
npx playwright install chromium
npm test
npm run test:integration
```

模拟测试运行构建后的页面，使用与 Go 服务一致的 CSP 和模拟 API/WebSocket，覆盖初始化、登录、会话失效、出口操作、凭据保留、Vohive 首次回显、实时更新期间的编辑草稿、日志与统计、系统设置、更新网口，以及手机导航和主题。

`tests/layout.spec.ts` 逐页检查 320、390、768、1024、1440 和 2560px 布局，并覆盖深色主题、长连接名称、IPv6、大数值、长错误、登录设备和确认弹窗。连接编辑的六个分组还会在手机横屏下检查底部按钮、动态输入行，以及跨分组的验证错误定位。手机连接、日志和事件采用纵向列表，桌面保留表格与可调整的分页。

账户菜单通过点击或键盘打开，完整用户名在菜单内显示。指标卡使用原生 `title` 提示完整数值，悬停时不创建额外浮层。`tests/hover.spec.ts` 在可见滚动条、浅色和深色主题下，验证持续实时更新期间的悬停布局稳定性，以及账户菜单的打开、关闭和退出登录。

`tests/select.spec.ts` 使用实际坐标点击下拉框，检查菜单出现在视口内、实时更新期间保持打开，以及选择后筛选请求和表单联动正常。默认测试覆盖窄屏、桌面和系统减少动画偏好；跨浏览器配置还覆盖 Firefox、WebKit 和模拟手机触摸：

```bash
npx playwright install chromium firefox webkit
npx playwright test --config playwright.select.config.ts
```

该配置同时运行 `tests/motion.spec.ts`，在正常和减少动画模式下检查账户菜单、手机导航抽屉和确认弹窗，包括 Escape 与取消按钮、关闭后的焦点恢复、重新打开及页面滚动恢复。编辑草稿测试还会在 DNS 菜单已经打开时切换系统动画偏好，确认草稿和菜单状态保留。

控制台启动时读取 `prefers-reduced-motion`，通过 Ant Design 的 `theme.token.motion` 关闭组件动画。不要用全局 `animation-duration` 或 `transition-duration` 覆盖组件动画：极短的过渡会干扰浮层准备与定位，使已经打开的菜单停留在屏幕外。系统动画偏好改变后重新加载页面生效，避免动态替换动画 Provider 丢失编辑草稿。

浏览器异常排查可将 `../scripts/debug-webui-select.js` 的全部内容粘贴到故障页面的 Console 中执行，再点击主题下拉框或用键盘打开菜单。脚本从打开操作开始采集，单纯聚焦控件不会提前结束诊断。两秒后会打印 `WWAN_SELECT_DIAGNOSTIC`，包含鼠标与键盘事件、展开状态、菜单坐标、样式、动画和过渡属性；每次快照重新读取视口和系统偏好，报告中的环境值来自输出时刻。脚本只读取页面状态，不读取表单值或请求后端。`tests/select-diagnostics.spec.ts` 验证正常、屏幕外和透明菜单的诊断结果，并覆盖聚焦等待、窗口调整和采集期间的动画偏好变化。

查看本次布局审查的页面截图（输出到临时目录，不纳入生产包）：

```bash
WWAN_UI_AUDIT=1 npm test -- layout.spec.ts
# /tmp/wwan-ui-audit/
# 指定输出目录以保留前后对比：
WWAN_UI_AUDIT=1 WWAN_UI_AUDIT_DIR=/tmp/wwan-ui-review npm test -- layout.spec.ts
```

集成测试需要 Go 和 GCC，会构建真实二进制并使用临时 SQLite 数据库在 `127.0.0.1:4180` 启动服务，验证配置保存、编辑回显、凭据保留和会话撤销。退出后自动删除临时文件。

如果机器已有 Chrome，可指定可执行文件：

```bash
PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/google-chrome npm test
```

## 代码位置

- `src/pages/`：总览、连接、性能、日志、Vohive 事件、历史统计和系统设置。
- `src/components/`：登录、出口编辑、系统与管理员表单、登录设备、程序更新和图表等 Ant Design 组件。
- `src/hooks/`：会话生命周期、API 查询、WebSocket 重连和最近五分钟吞吐采样。
- `src/lib/`：API、表单序列化、指标格式化和事件摘要。
- `tests/`：生产静态资源服务器、API/WebSocket fixture 和浏览器回归。

出口编辑每次打开前读取已保存的系统设置，确保首次编辑即可显示 Vohive 设备编码。编辑草稿独立于实时快照；已有代理用户和上游密码留空时遵守后端的密码保留协议。

执行 `npm run screenshots` 可使用演示数据重新生成 `docs/webui-*.png` 中的七个页面、管理员初始化、深色总览、出口编辑和手机端预览。

`tests/demo.ts` 提供正常、异常与停用出口，IPv6、TCP / UDP / HTTP 流量，多级日志、设备恢复事件、登录设备和历史曲线，供带数据的界面审查使用。设置 `WWAN_UI_SCREENSHOT_DIR=/tmp/wwan-ui-preview` 可把页面预览输出到临时目录；长名称、大数值和长错误等压力数据仍由布局测试单独覆盖。

仓库只保留根目录 README 引用的当前预览图，其他界面截图由 `.gitignore` 忽略。新增正式预览图时，同时更新 README 的引用和 `.gitignore` 的图片白名单。布局审查截图继续输出到 `/tmp/wwan-ui-audit/`。
