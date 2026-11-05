# HomeMedia

轻量自研家庭影音系统：Go + SQLite，目录库只囤元数据，点开才即时搜源、服务端去广告播放。

当前版本：v2.1（开发中）。电视端沿用改名后的电视应用（见下），网页端是同样完整的观看面，两端共用同一套后端接口、目录库、观看记录与收藏。

## 功能

- **目录库只囤元数据**：标题、海报 URL、年份、地区、类型、豆瓣 ID/评分、简介，不囤播放链接，杜绝死链。
- **点开即时搜源**：详情页向各采集源并发拉取新鲜 m3u8，首分片测速自动推荐最快线路，失败自动换源。
- **服务端 m3u8 去广告**：电视端 ExoPlayer 跑不了浏览器去广告脚本，广告清洗放在服务端 `/api/proxy/m3u8`。
- **分片磁盘缓存**：TS 分片缓存（上限 2GB，LRU），重看、多设备同集走局域网缓存。
- **货架浏览**：按电影 / 剧集 / 综艺 / 动漫 / 短剧 / 体育分标签页，每个分类各存一套筛选条件（年份 / 题材 / 地区按分类出选项），点卡片直接开详情。
- **即将上映 + 想看**：上映日程整页时间线，想看红点 = 已上映且未进播放记录数。
- **多用户**：观看记录、收藏、想看按账号隔离；海报走服务端图片代理（SSRF 防护、磁盘缓存、可配多个代理）。
- **电视端兼容**：实现电视端的 `ApiService` 契约（Cookie 鉴权、搜索、播放记录、收藏、直播源），电视端不用换、老人无感切换。

## 电视端

电视端应用在 [doge2mars/HomeMedia-TV](https://github.com/doge2mars/HomeMedia-TV)（Selene-TV 的 fork），两个版本加一个关机小应用，包名不同，可装同一台盒子共存：

| 应用 | 包名 | 版本 | 播放方式 |
| --- | --- | --- | --- |
| 通用版 B | `org.moontechlab.homemedia.general` | 1.4.13 | 电视直连源站 CDN |
| 专用版 C | `org.moontechlab.homemedia` | 1.1.6 | 默认走服务端代理（去广告、解析分享页），三档模式：仅代理 / 代理优先 / 仅直连 |
| 关机 | `org.moontechlab.shutdown` | 1.3 | 点开即关机（需 root） |

- **日常求稳用 B，播分享页的源、要去广告用 C。** C 的“代理优先”：代理行就走代理，代理失败自动切直连一次，位置不断。
- C 的代理就是本仓库的 `/api/proxy/m3u8`：分享页解析（电影天堂这类 HTML 落地页）、`#EXT-X-KEY` / `#EXT-X-MAP` 地址改写、广告分片过滤都在服务端做；电视端只负责播。
- 两个版本找片方式一样（搜片名是服务端实时搜各源，货架是查服务端目录库，继续观看是服务端存的记录），同一账号登录，B 和 C 的记录是同一份。
- 连 LunaTV 也能用（搜、详情、直连播），但去广告、分享页解析、货架都没有——那是 HomeMedia 才有的。

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
cp .env.example .env   # 编辑：HOME_MEDIA_SECRET（openssl rand -hex 32）、管理员密码、PUID/PGID、端口
mkdir -p data          # 自己先建好数据目录（见下面“非 root 运行”）
docker compose up -d --build
```

打开 `http://<主机IP>:8090`（宿主机端口由 `.env` 里的 `HOME_MEDIA_HOST_PORT` 决定，默认 8090，容器内部固定 8080）。数据（SQLite 库、海报缓存、分片缓存）全部 bind mount 在 `./data/`（或 `.env` 里的 `HOME_MEDIA_DATA_DIR`），备份整个项目目录即备份全部数据；部署到 NAS 时把 `HOME_MEDIA_DATA_DIR` 指到大盘路径即可。

**非 root 运行（飞牛 fnOS / 群晖 / 普通 Linux 账号）**：容器以 `PUID:PGID`（默认 1000:1000）运行，这样 `data/` 里的文件属于你自己的账号，在文件管理器 / SMB 里可以直接复制、备份、删除，不会被 root 锁死。用 `id -u`、`id -g` 查你的 ID 填进 `.env`。`data/` 要由你**先建好**——不存在时 Docker 会以 root 创建，非 root 的容器就写不进去。若已经被 root 创建过：`sudo chown -R $(id -u):$(id -g) data`。普通账号执行 docker 需要 `sudo`，或先 `sudo usermod -aG docker $USER` 后重新登录。

镜像带 `HEALTHCHECK`（探测 `/api/ping`），`docker ps` 里能看到 healthy / unhealthy；`restart: unless-stopped` 保证异常退出和开机后自动拉起。

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
segment_test.go  分片/m3u8 代理的回归测试（`go test ./...`）
web/             网页端（单页 index.html）
```

## 安全提醒

本项目仍在开发中，默认凭据只是开箱即用的占位：**自己部署时请先改 `HOME_MEDIA_SECRET` 和管理员密码**，且不要把服务直接暴露到公网。电视端 ExoPlayer 不带 Cookie，因此 `/api/proxy/m3u8` 与 `/api/proxy/segment` 目前**既没有登录鉴权，也没有内网地址过滤**（内网过滤只在海报代理里做了），可被用来请求任意 URL。部署时请放在内网或可信反代之后，不要暴露到公网。

## 来源

服务端代码是原创的（不是 LunaTV 的 fork），但在设计上与 LunaTV 兼容：

- 电视端接口按 Selene-TV 实际调用的 `ApiService` 契约实现（Cookie 鉴权、搜索、播放记录、收藏、直播源），这样电视端不用换、老人无感切换——接口兼容是逆向电视端实际请求抽出来的，不是抄 LunaTV 的服务端代码。
- 网页端的布局参考了 LunaTV 的配置页样式（视频源、直播源等）。
- 电视端应用本身是 Selene-TV 的 fork，见 [doge2mars/HomeMedia-TV](https://github.com/doge2mars/HomeMedia-TV)。

## 许可

自用项目，暂未定许可。
