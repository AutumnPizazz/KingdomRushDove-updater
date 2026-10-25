# 王国保卫战 Dove 版更新器

把游戏目录更新到服务端最新版本。程序是一个 exe，双击就能用，不需要装运行库。

更新方式跟游戏自带更新器一样：代码按提交差异同步，图片和音频按 `_assets/assets_index.lua`
里记的大小和哈希逐个比对，只下载变了的文件。

## 用 exe 更新

1. 双击 `krd-updater.exe`。浏览器会自动打开设置页。
2. 看「安装目录」对不对。要选到含 `main.lua` 和 `_assets` 的那一层，拿不准就点「探测」。
3. 点「检查更新」，看提示里有多少文件要变，再点「开始更新」。
4. 更新前退出游戏。`love.exe` 还开着的话文件写不进去。
5. 游戏装在 `C:\Program Files` 下时，右键选「以管理员身份运行」，否则没有写权限。

程序只改游戏安装目录里的文件，存档不动。更新源不用改，默认走官方镜像。

双击启动时不会弹黑色控制台窗口，程序在后台跑。关掉网页它会在两秒内自己退出。
想中途结束，点页面上的「退出程序」。

## 命令行

不习惯图形界面的话，命令行的功能一致。

```bash
krd-updater.exe check                 # 只检查，不改文件
krd-updater.exe update --yes          # 直接更新，不再询问
krd-updater.exe update --dry-run      # 打印将要做什么
krd-updater.exe verify                # 逐个比对本地资源的哈希
krd-updater.exe status --json         # 本地版本、服务端版本、待更数量
krd-updater.exe config init           # 生成配置文件
krd-updater.exe set-hash <提交号>      # 手动改本地版本标记
krd-updater.exe wizard                # 控制台问答式引导
```

图形界面是本地 HTTP 服务，端口随机、只监听 `127.0.0.1`，页面用 `go:embed` 打进 exe。
要固定端口或者不自动开浏览器：

```bash
krd-updater.exe gui --addr 127.0.0.1:8765 --no-open
krd-updater.exe gui --idle-timeout 0     # 页面关掉也不退出，留给脚本或调试用
```

页面每 15 秒发一次心跳，关页面时发一次 `/api/bye`，程序等两秒没有新请求就退出。
刷新页面不算关闭，因为新页面会立刻发请求，把这次退出取消掉。心跳停满 `--idle-timeout`
（默认 2 分钟）也会退出，兜住浏览器崩溃这种发不出 bye 的情况。

常用参数：

| 参数 | 作用 |
| --- | --- |
| `--dir PATH` | 指定安装目录，默认自动探测 |
| `--site URL` | 换更新源，可重复 |
| `--config PATH` | 指定配置文件 |
| `--no-assets` | 只更代码和数据 |
| `--no-delete` | 不删服务端标记为删除的文件 |
| `--backup` | 覆盖前备份旧文件 |
| `--keep-temp` | 保留临时目录，便于排查 |
| `--json` | 结果走 stdout 的 JSON，日志走 stderr |
| `--verbose` | 输出调试信息 |

退出码：`0` 成功或已最新，`2` 用法错误，`3` 配置错误，`4` 网络错误，`5` 安装目录错误，
`6` 权限不足，`7` 下载失败，`8` 落地失败。

## 配置文件

`krd-updater config init` 会生成 `krd-updater.jsonc`，带中文注释，也可以直接抄
`krd-updater.example.jsonc`。

查找顺序：`--config` 参数 → 当前目录 → exe 同目录 → 用户配置目录
（Windows 是 `%APPDATA%\krd-updater\`，Linux 是 `~/.config/krd-updater/`）。

常改的几项：

```jsonc
{
  "install_dir": "",        // 空 = 自动探测
  "sites": ["https://krdovedownload4.crazyspotteddove.top/"],
  "skip_assets": false,     // true = 只更代码和数据
  "delete_removed": true,   // 是否删服务端标记为删除的文件
  "proxy": "",              // 例如 http://127.0.0.1:7890
  "elevate": true,          // 目录不可写时自动请求管理员权限
  "backup": { "enabled": false, "dir": "" }
}
```

同名环境变量优先于配置文件：`KRD_INSTALL_DIR`、`KRD_SITES`、`KRD_PROXY`、`KRD_SKIP_ASSETS`、
`KRD_ELEVATE`、`KRD_JSON` 等，完整列表见 `internal/config/config.go`。

## 给开发者

### 编译

Go 版本锁在 1.27.3。`go.mod` 里写的是 `go 1.27.3`，`Makefile` 和 `scripts/deps.sh`
都设了 `GOTOOLCHAIN=go1.27.3`，本机没有这个版本时 Go 会自己下载。别用更低的版本编，会直接报错。

```bash
make            # 当前平台 -> dist/krd-updater
make windows    # Windows x64 -> dist/krd-updater.exe
make windows-arm64
make linux
make dist       # Windows exe 加示例配置，打成 zip
make test       # 单元测试
```

不走 `make` 时，先设 `export GOTOOLCHAIN=go1.27.3`（Windows 用
`set GOTOOLCHAIN=go1.27.3`）或者 `go env -w GOTOOLCHAIN=go1.27.3`，效果一样。

交叉编译不用 cgo，只调了 Win32 的 `syscall`：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o krd-updater.exe .
```

依赖只有一个 `github.com/tailscale/hujson`，用于解析 JSONC，已经放在 `vendor/`。
平时编译不联网。

### 拉不到模块时

`vendor/` 齐全时不需要联网。只有升级依赖（`go get`、`go mod tidy`）才要下载。

**一、换模块代理**

| 代理 | 地址 |
| --- | --- |
| 七牛 | `https://goproxy.cn` |
| goproxy.io | `https://goproxy.io` |
| 阿里云 | `https://mirrors.aliyun.com/goproxy/` |
| 腾讯云 | `https://mirrors.cloud.tencent.com/go/` |
| 百度云 | `https://goproxy.bj.bcebos.com` |
| 华为云 | `https://mirrors.huaweicloud.com/repository/goproxy/` |
| 官方 CN | `https://proxy.golang.com.cn` |
| 官方 | `https://proxy.golang.org`（需要外网） |

```bash
make deps                                    # 依次试上面的镜像，成功后顺手更新 vendor/
bash scripts/deps.sh https://goproxy.io      # 只试指定代理
go env -w GOPROXY=https://goproxy.cn,https://goproxy.io,direct
go env -w GOSUMDB=off                        # 校验库在墙外
```

**二、给 git 套 GitHub 加速站**

只在 `GOPROXY=direct` 时有用，那种模式下 Go 会直接 clone GitHub。

```bash
git config --global url."https://ghfast.top/https://github.com/".insteadOf "https://github.com/"
GOPROXY=direct GOSUMDB=off go mod download
git config --global --unset url."https://ghfast.top/https://github.com/".insteadOf
```

可换的加速站：`ghfast.top`、`gh-proxy.com`、`ghproxy.net`、`hk.gh-proxy.com`，
用法都是把域名拼在完整地址前面。这类站点会失效或限速，能改 `GOPROXY` 就先改。

**三、完全离线**

模块缓存里已有依赖时：

```bash
GOPROXY=off GOFLAGS=-mod=vendor go build ./...
```

### 更新协议

1. `POST {site}commits`，请求体带 `current_version_commit_hash.txt` 里的 40 位提交号。
   返回 `commits[]`、`added_or_modified_files[]`、`deleted_files[]`、`master_commit_hash`。
2. 取远端 `_assets/assets_index.lua`，按大小和哈希比对本地资源，得到待更清单。
3. 代码走 `POST {site}bundle/code`，资源走 `POST {site}bundle/assets`，服务端打成 zip 返回。
   超过 20 MB 或 200 个文件时分批，打包失败就退回逐文件下载。
4. 校验、落盘、删废弃文件，把 `master_commit_hash` 写回版本标记。

资源哈希照抄游戏里的 Lua：`hash = (bit.bxor(hash, x) * 16777619) % 0xFFFFFFFF`。
先读头 4096 字节，文件大于 8192 字节时再读末 4096 字节，最后把文件大小异或进去。
两处容易写错，`internal/assetindex/index.go` 里有注释：

- `bit.bxor` 结果是有符号 int32，按 uint32 自然溢出写，第一次比较就差了符号。
- 取模是 LuaJIT 的 `a - floor(a/b)*b`，乘法在 double 里做，乘积能到 2^56 会被舍入。
  换成 `fmod` 或整型取模，结果差 1，整库哈希全对不上。

### 目录

```
main.go                 命令分发
wizard.go               控制台引导
internal/assetindex     assets_index.lua 解析与哈希比对
internal/config         配置读写与 JSONC 模板
internal/gui            本地网页界面（页面用 go:embed 打包）
internal/osutil         平台相关：提权、打开浏览器、控制台编码与隐藏
internal/pipeline       检查与更新的主流程
internal/remote         服务端接口、断点下载
internal/ui             进度输出
```

## 常见问题

| 现象 | 处理 |
| --- | --- |
| 提示没有写权限 | 右键「以管理员身份运行」 |
| 提示游戏正在运行 | 完全退出游戏 |
| 更新源都连不上 | 用 `--site` 换源，官方镜像有 `krdovedownload4...` 和 `krdovedownload6...:52000`；也可以配 `proxy` |
| 资源校验有异常 | 再跑一次更新补齐；还不行加 `--keep-temp` 留现场 |
| 双击闪退看不到报错 | 在命令行里运行，或在配置里把 `log_file` 设成 `krd.log` |
| 关掉网页后还想再开 | 重新双击 exe，会起一个新的后台进程和端口 |
| 后台进程停不下来 | 点页面「退出程序」，或用任务管理器结束 `krd-updater.exe` |
| 版本标记丢了 | `krd-updater set-hash <提交号>`，提交号看服务端 `commits` 接口或发布页 |
