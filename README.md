# HomeMedia

轻量自研家庭影音系统：**Go + SQLite 单二进制**，本地片库只囤元数据、点开即时并发搜源，**网页端与电视端共享服务端 m3u8 去广告代理与分片缓存**。

---

## 核心亮点（与 LunaTV / 纯前端壳子的本质区别）

| 能力维度 | LunaTV / 纯前端转发项目 | HomeMedia（本项目） |
| :--- | :--- | :--- |
| **架构形态** | 无本地片库，强依赖外部接口与浏览器实时扫源 | **Go + SQLite 单进程**，自带 4.7 万+ 本地元数据货架与多级缓存 |
| **去广告能力** | 仅网页端播放器插件生效，电视盒子束手无策 | **服务端 `/api/proxy/m3u8` 统一剥离 `#EXT-X-DISCONTINUITY` 广告切片**，网页与电视端 ExoPlayer 同步受益 |
| **播放加速与缓存** | 每次播放、倒带、多设备同看均直连源站慢 CDN | **测速探针与播放共享 2GB 磁盘分片缓存（LRU）**，测完即落盘，点播首帧局域网秒开 |
| **找片与浏览** | 豆瓣接口被封或风控时首页直接空白 | **「菜单 + 后厨」双层设计**：豆瓣五档自动降级 + Bangumi 每日放送 + 本地 4.7 万部清洗后货架随时兜底 |
| **源健康与自愈** | 坏源/慢源每次搜索都拖慢整页，停用后无法自动恢复 | **源健康度自动记分 + 10 分钟负缓存 + 6 小时后台自动复测捞回**；SSE 聚合流带 3 分钟缓存与并发限流 |
| **家庭内容安全** | 无分级或仅前端隐藏 | **同步与实时搜索双链路成人拦截** + **「其他/敏感内容」4 位数字 PIN 锁（`HttpOnly` 签名 Cookie）** |

---

## 工作原理：「菜单」与「后厨」分离

1. **三层「菜单」（只管挑片，不存易死链的播放地址）**：
   - **豆瓣精选榜单 / Bangumi 番组日历**：首页「热门电影、热门剧集、华语口碑剧集榜、海外剧周榜、国内综艺榜、高分喜剧、90年代经典港片、高分纪录片」并发加载；豆瓣支持 `direct / tencent / ali / zwei / custom` 五档代理并在主档位超时/拦截时自动降级。
   - **本地货架（4.7 万+ 部元数据）**：每日凌晨（或按间隔）单事务批量同步各源分类元数据，自动清洗年份/评分，并通过**分类规则 + 片名季集特征 + `vod_remarks`/集数二次校验**精准分离「电影 / 剧集 / 短剧 / 动漫 / 综艺 / 其他」；支持毫秒级搜索下拉联想（`/api/search/suggest`）与多维筛选。
   - **即将上映 + 想看提醒**：抓取院线与剧集排期时间线，加入「🔔 想看」后上映自动红点提醒。
2. **统一「后厨」（即时搜源 + 真实分片测速 + 失败无感换源）**：
   - **全局搜索（`/api/search`）与点播聚合（`/api/detail-aggregate-stream`）**：拿着片名实时并发向 20+ 个苹果 CMS 采集源拉取最新集数与 `m3u8`；搜源信号量与测速信号量解耦，快源秒出。
   - **真实首分片探针（`probePlayURL`）**：解析落地分享页（`/share/` HTML）、校验真实 TS/fMP4 分片（拒绝 200 状态码的防盗链 HTML 错误页），按实测下载速率（KB/s）与分辨率自动优选最快线路；播放中途卡死自动保持进度无缝换源（`failover`）。

---

## 功能一览

- **目录库只囤元数据**：标题、海报 URL、年份、地区、类型、评分、周点击量、简介，不囤播放链接，杜绝死链。
- **点开即时搜源 + 探针落盘秒开**：SSE 流式聚合详情，首分片测速同时写入 `segment-cache`，播放器请求首分片直接命中本地磁盘。
- **服务端 m3u8 去广告与分享页解析**：自动拆解电影天堂等 `/share/` HTML 落地页提取真实 m3u8，改写 `#EXT-X-KEY` / `#EXT-X-MAP` 与分片地址，集中剔除插播广告。
- **双磁盘缓存与自动淘汰**：
  - 视频分片缓存（`segment-cache`）：上限 **2GB**，按最近访问时间（`mtime`）自动淘汰至 1.5GB；并发同分片请求单飞合并（`segInflight`）。
  - 海报图片缓存（`image-cache`）：上限 **1GB**，自动淘汰至 750MB；针对豆瓣图床（`*.doubanio.com`）自动携带 Chrome UA 与豆瓣 `Referer` 防 `418` 拦截，并内置 5 分钟失败负缓存与 `wsrv.nl` 兜底。
- **家庭多用户与 4 位 PIN 内容锁**：
  - 每个家庭成员独立的观看进度（换源自动继承集内时间点）、收藏、想看、倍速设置。
  - 「🔒 其他」分类及敏感影片受 **4 位数字 PIN 锁**（默认 `1234`，站长可在设置页修改）保护，未解锁时从货架、搜索、联想、相关推荐中全面隐藏。
- **电视端原生兼容**：完整实现电视端 `ApiService` 契约（Cookie 鉴权、SSE 搜索、播放记录、收藏、M3U/TVBox 直播源订阅），老人小孩无感使用。

---

## 电视端

电视端应用在 [doge2mars/HomeMedia-TV](https://github.com/doge2mars/HomeMedia-TV)（Selene-TV 的 fork），两个版本加一个关机小应用，包名不同，可装同一台盒子共存：

| 应用 | 包名 | 版本 | 播放方式 |
| --- | --- | --- | --- |
| 通用版 B | `org.moontechlab.homemedia.general` | 1.4.13 | 电视直连源站 CDN |
| 专用版 C | `org.moontechlab.homemedia` | 1.1.6 | 默认走服务端代理（去广告、解析分享页），三档模式：仅代理 / 代理优先 / 仅直连 |
| 关机 | `org.moontechlab.shutdown` | 1.3 | 点开即关机（需 root） |

- **日常求稳用 B，播分享页的源、要去广告用 C。** C 的“代理优先”：代理行就走代理，代理失败自动切直连一次，位置不断。
- C 的代理就是本仓库的 `/api/proxy/m3u8`：分享页解析、`#EXT-X-KEY` / `#EXT-X-MAP` 地址改写、广告分片过滤都在服务端做；电视端只负责播。

---

## 快速运行

需要 Go 1.23+（CGO 启用，用于 `go-sqlite3`）。

```bash
go build -o home-media .
HOME_MEDIA_PORT=8090 HOME_MEDIA_DATA=./data ./home-media
```

打开 `http://localhost:8090`，默认管理员 `admin / admin123`，默认受限内容 4 位 PIN 码 `1234`。

---

## Docker 部署

```bash
git clone https://github.com/doge2mars/HomeMedia.git
cd HomeMedia
cp .env.example .env   # 编辑：HOME_MEDIA_SECRET、管理员密码、PUID/PGID、端口
mkdir -p data          # 自己先建好数据目录（见下面“非 root 运行”）
docker compose up -d --build
```

打开 `http://<主机IP>:8090`（宿主机端口由 `.env` 里的 `HOME_MEDIA_HOST_PORT` 决定，默认 8090，容器内部固定 8080）。数据（SQLite 库、海报缓存、分片缓存）全部 bind mount 在 `./data/`（或 `.env` 里的 `HOME_MEDIA_DATA_DIR`），备份整个项目目录即备份全部数据；部署到 NAS 时把 `HOME_MEDIA_DATA_DIR` 指到大盘路径即可。

**非 root 运行（飞牛 fnOS / 群晖 / 普通 Linux 账号）**：容器以 `PUID:PGID`（默认 1000:1000）运行，这样 `data/` 里的文件属于你自己的账号，在文件管理器 / SMB 里可以直接复制、备份、删除，不会被 root 锁死。用 `id -u`、`id -g` 查你的 ID 填进 `.env`。`data/` 要由你**先建好**——不存在时 Docker 会以 root 创建，非 root 的容器就写不进去。若已经被 root 创建过：`sudo chown -R $(id -u):$(id -g) data`。

---

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HOME_MEDIA_PORT` | `8080` | 监听端口 |
| `HOME_MEDIA_DATA` | `/opt/home-media/data` | 数据目录（SQLite 库、图片缓存、分片缓存） |
| `HOME_MEDIA_SECRET` | 自动生成并持久化于 `meta` 表 | Cookie 签名密钥；未设置环境变量时首次启动自动生成 32 字节随机密钥 |
| `HOME_MEDIA_ADMIN_PASS` | `admin123` | 初始管理员密码，**首次运行后请及时修改** |
| `HOME_MEDIA_IMAGE_PROXIES` | 空 | 图片代理（逗号分隔），直连失败时按顺序重试；也可在设置页里改 |

---

## 目录结构

```text
main.go          入口、鉴权（HttpOnly Cookie + 随机密钥）、货架元数据与同步调度
cms.go           采集源搜索、目录同步（单事务批量写库+熔断+剧影分类校验）、m3u8去广告与分片代理
discovery.go     货架查询、家庭在看趋势、同类推荐、搜索毫秒级下拉联想
restricted.go    受限分类（其他/敏感内容）识别与 4 位数字 PIN 锁接口
health.go        视频源健康度记分、10 分钟负缓存、6 小时自动复测恢复
probe.go         播放前真实首分片测速探针与分片缓存复用
douban.go        豆瓣榜单接口与五档代理（direct/tencent/ali/zwei/custom）自动降级
bangumi.go       Bangumi 番组计划每日放送日历
tmdb.go          TMDB 趋势接入
image_proxy.go   海报代理（豆瓣防 418 拦截、SSRF 防护、5 分钟负缓存、1GB 磁盘缓存淘汰）
v2.go ~ v5.go    源管理、配置订阅、直播源解析、SSE 详情流式聚合、即将上映与想看
web/index.html   网页端单页应用
*_test.go        全链路回归单测（`go test ./...`）
```
