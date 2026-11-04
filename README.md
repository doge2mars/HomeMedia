# HomeMedia

轻量自研家庭影音系统：Go + SQLite，目录库只囤元数据，点开才即时搜源、服务端去广告播放。

当前版本：v2.1（开发中）。电视端沿用改名后的 Selene-TV（见下），网页端是同样完整的观看面，两端共用同一套后端接口、目录库、观看记录与收藏。

## 功能

- **目录库只囤元数据**：标题、海报 URL、年份、地区、类型、豆瓣 ID/评分、简介，不囤播放链接，杜绝死链。
- **点开即时搜源**：详情页向各采集源并发拉取新鲜 m3u8，首分片测速自动推荐最快线路，失败自动换源。
- **服务端 m3u8 去广告**：电视端 ExoPlayer 跑不了浏览器去广告脚本，广告清洗放在服务端 `/api/proxy/m3u8`。
- **分片磁盘缓存**：TS 分片缓存（上限 2GB，LRU），重看、多设备同集走局域网缓存。
- **货架浏览**：按电影 / 剧集 / 综艺 / 动漫 / 短剧 / 体育分标签页，每个分类各存一套筛选条件（年份 / 题材 / 地区按分类出选项），不用先想剧名，点卡片直接开详情。
- **即将上映 + 想看**：上映日程整页时间线，想看红点 = 已上映且未进播放记录数。
- **多用户**：观看记录、收藏、想看按账号隔离；海报走服务端图片代理（SSRF 防护、磁盘缓存、可配多个代理）。
- **电视端兼容**：实现 Selene-TV 的 `ApiService` 契约（Cookie 鉴权、搜索、播放记录、收藏、直播源），电视端不用换、老人无感切换。

## 快速运行

需要 Go 1.23+。

```bash
go build -o home-media .
HOME_MEDIA_PORT=8090 HOME_MEDIA_DATA=./data ./home-media
```

打开 `http://localhost:8090`，默认管理员 `admin / admin123`。

## Docker 部署

```bash
git clone https://github.com/doge2mars/HomeMedia.git
cd HomeMedia
# 先在 .env 里写好下面两项（不要用默认值对外服务）：
#   HOME_MEDIA_SECRET=<长随机串，如 openssl rand -hex 32 的输出>
#   HOME_MEDIA_ADMIN_PASS=<管理员密码>
docker compose up -d --build
```

打开 `http://localhost:8080`。数据（SQLite 库、海报缓存、分片缓存）全部 bind mount 在项目目录下的 `./data/` 里，备份整个项目目录即备份全部数据；部署到 NAS 时把 compose 里 `./data:/data` 那行改成大盘路径即可。

注意：刚部署好的系统目录库和采集源是空的——采集源存在你原来那台服务器的数据库里，不在仓库里。进设置页加好源，再点“同步目录”，货架才有内容。

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HOME_MEDIA_PORT` | `8080` | 监听端口 |
| `HOME_MEDIA_DATA` | `/opt/home-media/data` | 数据目录（SQLite 库、图片缓存、分片缓存） |
| `HOME_MEDIA_SECRET` | `home-media-dev-secret-change-me` | Cookie 签名密钥，**正式部署务必改掉** |
| `HOME_MEDIA_ADMIN_PASS` | `admin123` | 初始管理员密码，**首次运行后务必改掉** |
| `HOME_MEDIA_IMAGE_PROXIES` | 空 | 图片代理（逗号分隔），直连失败时按顺序重试；也可在设置页里改 |

数据目录建议 bind mount 进项目目录，不要用 Docker 命名卷。

## 目录结构

```text
main.go          入口、鉴权、播放记录 / 收藏 / 想看接口
cms.go           采集源搜索、详情聚合、目录同步、m3u8/分片代理
v2.go v3.go      搜索、直播、用户与设置等接口
v4.go            SSE 流式聚合详情
v5.go            即将上映、想看
image_proxy.go   海报代理（SSRF 防护、磁盘缓存、多代理）
web/             网页端（单页 index.html）
```

## 安全提醒

本项目仍在开发中，默认凭据只是开箱即用的占位：**自己部署时请先改 `HOME_MEDIA_SECRET` 和管理员密码**，且不要把服务直接暴露到公网。电视端 ExoPlayer 不带 Cookie，因此 `/api/proxy/m3u8` 与 `/api/proxy/segment` 目前不做登录鉴权，仅做了内网过滤之外的基础限制，部署时请放在内网或可信反代之后。

## 许可

自用项目，暂未定许可。
