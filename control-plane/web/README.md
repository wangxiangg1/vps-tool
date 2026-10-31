# vps-tool 管理工作台

这是一个无构建步骤的静态前端，主控 FastAPI 启动后会从 `/` 自动提供 `index.html`、`styles.css` 和 `app.js`。

## 本地预览

请先按 `../server/README.md` 启动主控。前端使用同源 Cookie 和 CSRF Token 调用主控 API，打开：

```text
http://127.0.0.1:8000/
```

界面覆盖管理员登录、节点筛选、状态查看、固定 Action、批量选择、请求进度、任务列表和错误/空状态。它不会暴露任意 Shell、命令字符串或脚本输入。
