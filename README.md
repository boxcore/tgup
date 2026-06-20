# tgup

## 部署
```

cd ~/tgup

# 如果还没有 go.mod 文件，先初始化（模块名可以自定义）
go mod init tgup

# 自动下载并整理代码中 import 的依赖包
go mod tidy

## gen config
mkdir -pv ~/.tgup/{cache,logs}

cat > ~/.tgup/config.conf <"EOF"
CHAT_ID=-100xxxxxxxxx
BOT_API=your_bot_token_here
TG_API_URL=https://api.telegram.org
PHOTO_EXTS=.jpg,.jpeg,.png
VIDEO_EXTS=.mp4,.mkv,.avi,.mov
EOF

cd DIR

# 编译出名为 tgup 的可执行文件
go build -o tgup ./cmd/tgup

# 赋予执行权限
chmod +x tgup

# 移入系统 bin 目录，方便在任意地方直接调用（可选）
sudo mv tgup /usr/local/bin/

```


这里是为您整理好的 `tgup` 工具完整参数说明文档。您可以直接复制以下 Markdown 内容，无缝粘贴到您的项目 `README.md` 中：

---

## 🛠️ 参数选项说明

`tgup` 提供了丰富的命令行参数，用于精细化控制网盘媒体文件的筛选、并发流控以及切片行为。

| 长参数 | 短参数别名 | 默认值 | 允许值 / 示例 | 功能描述 |
| --- | --- | --- | --- | --- |
| `--title` | 无 | 空 | `"标题内容 #标签"` | 为发送的媒体相册（Media Group）设置全局统一的文本标题/文字介绍。 |
| `--sort` | 无 | `name` | `name` / `mod` / `create` / `size` / `size_desc` | **文件上传排序控制器**：<br>• `name`: 智能自然数字顺序（人类直觉习惯）。<br>• `mod` / `create`: 文件的修改时间 / 创建时间。<br>• `size` / `size_desc`: 文件大小正序 / 倒序。 |
| `--type` | 无 | `all` | `all` / `pic` / `video` / `m4v` | **媒体流筛选器**。<br>• `pic`: 只传图片<br>• `video` (或 `vedio`): 只传视频<br>• 填入具体后缀（如 `m4v`, `mp4`, `png`）: 精准只上传该后缀的文件。 |
| `-n` | 无 | `10` | `1 ~ 10` | 单次打包发送的相册文件数上限。Telegram 官方限制相册最大为 10，超过 10 自动重设为 10。 |
| `-s` | 无 | `4` | 整数（秒） | 批次（相册）上传成功后的**战略冷却休眠时间**，用以安全渡过 Telegram 云端的风控权重计算，防频控。 |
| `--rate-limit` | `-r` | `20` | 整数（个） | **群组发包限频阈值**。防止 Bot 在群组/频道内消息发送过快（每分钟最多 20 条），当在 60 秒内发送次数达到阈值时会自动进行休眠等待。设为 `0` 或 `-1` 禁用该功能。 |
| `--spil` | 无 | `false` | `true` / `false` | **强制激活大视频切片核心**。无需更改配置文件，直接在命令行开启 >2GB 视频流拷贝秒切投递功能。 |
| `--cache-force` | `--cf` | `false` | `true` / `false` | 强行粉碎并刷新本地现有的缩略图（`.jpg`）与音视频元数据（`.json`）强缓存。 |
| `--test` | `-t` | 空 | `list` / `curl` | **测试/调试模式**。<br>• `list`: 在终端打印预检包线列表，不产生实质网络开销。<br>• `curl`: 打印 1:1 的标准 `curl` 封包报文以供审查。 |
| `--version` | `-v` | `false` | `true` / `false` | 输出当前工具的版本号并退出。 |

---

## 💡 高级特性说明

### 1. 智能参数乱序混放 (GNU getopt 风格)
由于内部集成了参数自动重排器，你不需要死板地遵守“参数必须在路径前面”的规则。工具会自动把路径剥离并置后。

### 2. 双重本地强缓存与 WebDAV 专属级流控
* **按需延迟探测**：仅在轮到当前批次时，才对该批次的视频进行单线程顺序 `ffprobe` 元数据提取，且文件间强制微歇 600ms，防止网盘封号。
* **双重本地强缓存**：首次探测成功后，本地会同时生成 `.jpg` 缩略图（严格限边 320px 匹配 TG 规范）和 `.json` 像素元数据。二次运行直接毫秒级读取本地缓存，对网盘达到 **0 字节**网络开销。
* **已完结秒跳过与自愈**：扫描时若发现本地历史缓存记录且已成功上传，直接跳过；若中途崩溃，再次运行时直接利用本地缓存，原地恢复。

### 3. 常数级 $O(1)$ 内存流与分段数据流
* 采用 `io.Pipe` 管道网络直冲，分片数据直接作为 HTTP 流写入网络，避免在系统内存中缓存大文件。

### 4. Flood Wait 风控自我愈合机制
* 当一次性投递导致 Local Bot API 触发云端限频时，程序会自动高精度截取 `retry after X` 中的秒数并进入安全休眠，醒来后全自动原地复活重试。

---

## 📖 常用场景示例命令

* **场景一：正常混合打包上传，并加入文字介绍**
```bash
tgup --title="今日苹果原相机街拍 #国产" /www/nas/CloudDrive/115open/up/目录
```

* **场景二：指定每次只发 5 个，且发完一组强行歇 10 秒（极稳网盘控流方案）**
```bash
tgup -n=5 -s=10 /www/nas/CloudDrive/115open/up/目录
```

* **场景三：检测目录下的所有视频文件元数据（如大小、分辨率、编码格式等）**
```bash
tgup --check-video /www/nas/CloudDrive/115open/up/目录
```

* **场景四：调试审查，看看带有苹果原相机 90 度旋转的视频，在打包成 `curl` 表单时高宽是否被自动摆正**
```bash
tgup -t=curl --cache-force /www/nas/CloudDrive/115open/up/苹果原相机93/www.98T.la@IMG_6485.mov
```