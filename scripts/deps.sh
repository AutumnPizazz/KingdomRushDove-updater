#!/usr/bin/env bash
# 在「访问不到 GitHub / proxy.golang.org」的环境里把依赖拉下来。
#
# 用法：
#   bash scripts/deps.sh            # 依次尝试内置镜像，成功后顺手 vendor
#   bash scripts/deps.sh <proxy>    # 只用指定代理
#
# 依赖很少（目前只有 hujson），仓库里已带 vendor/，
# 所以正常编译根本不需要联网；这个脚本只在升级依赖后才用得上。
set -euo pipefail

cd "$(dirname "$0")/.."

# 工具链锁在 1.27.3，和 go.mod 一致；本机版本不符时 GOTOOLCHAIN 会去下载这个版本。
export GOTOOLCHAIN=go1.27.3

# 常见的 Go 模块代理，国内优先，官方兜底。
MIRRORS=(
  "https://goproxy.cn"                        # 七牛，国内用得较多
  "https://goproxy.io"                        # 国内备用
  "https://mirrors.aliyun.com/goproxy/"       # 阿里云
  "https://mirrors.cloud.tencent.com/go/"     # 腾讯云
  "https://goproxy.bj.bcebos.com"             # 百度云
  "https://mirrors.huaweicloud.com/repository/goproxy/"  # 华为云
  "https://proxy.golang.com.cn"               # 官方 CN 镜像
  "https://proxy.golang.org"                  # 官方（需要能上外网）
  "direct"                                    # 直连，配合 GitHub 加速用
)

if [[ $# -gt 0 ]]; then
  MIRRORS=("$@")
fi

echo "Go 版本: $(go env GOVERSION)"
for m in "${MIRRORS[@]}"; do
  printf '尝试模块代理 %-55s ' "$m"
  if GOPROXY="$m" GOFLAGS=-mod=mod GONOSUMCHECK=1 GOSUMDB=off \
      go mod download all >/tmp/krd-deps.log 2>&1; then
    echo "成功"
    echo "把 vendor/ 也更新一下……"
    GOPROXY="$m" GOFLAGS=-mod=mod GOSUMDB=off go mod vendor
    echo "完成。之后 go build 会直接使用 vendor/，不需要网络。"
    exit 0
  fi
  echo "失败"
done

echo
echo "所有镜像都不可用。可选办法：" >&2
echo "  1) 挂代理后重试：HTTPS_PROXY=http://127.0.0.1:7890 bash scripts/deps.sh" >&2
echo "  2) 手动指定：bash scripts/deps.sh https://goproxy.cn" >&2
echo "  3) 如果仓库里已有 vendor/，直接 make build 即可（完全离线）" >&2
tail -5 /tmp/krd-deps.log >&2 || true
exit 1
