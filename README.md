# S3 每日目录备份

一个轻量的 Windows 桌面工具，可将多个本地目录同步到 S3 兼容对象存储，并支持手动备份和每日定时备份。传输由 s5cmd 执行，界面保持响应；每个目录同步到独立的远端子路径。

## 功能

- 配置 S3 兼容端点、区域、Bucket、前缀和凭据。
- 添加多个本地目录，测试 Bucket 连接并立即备份。
- 启动每日计划：启动时先执行一次，此后每天本地时间零点执行。计划运行期间需保持程序打开。
- 同步新增和变更文件，并删除远端对应目录中已从本地移除的文件。

## 构建和运行

安装 Go 后，在 Windows PowerShell 中构建：

```powershell
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags="-H windowsgui" -o s3-dir-backup-gui.exe .
```

从官方 s5cmd 项目获取 Windows AMD64 版本的 `s5cmd.exe`，与程序放在同一目录。首次启动后可在界面填写配置；也可以复制 `config.example.json` 为 `backup.json` 后编辑。示例值均为占位符。

默认连接字段是通用占位值：`https://s3.example.com`、`us-east-1`、`your-bucket` 和 `daily-backup`。请换成自己的 S3 服务信息。

## 凭据和同步行为

凭据及目录列表保存在程序旁的 `backup.json`。此文件包含密钥和本机路径，已加入 `.gitignore`，请勿提交或分享。传输时程序会创建临时凭据文件，并在操作结束后删除；日志不记录密钥。

同步会删除远端专属前缀下、本地已不存在的文件。建议为此工具配置专用前缀；中断后再次运行会重新对齐内容。该工具不保留历史版本。

本项目使用 MIT 许可证。随附的 `S5CMD-LICENSE.txt` 是 s5cmd 的第三方许可证。
