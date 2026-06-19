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
go build -o tgup main.go

# 赋予执行权限
chmod +x tgup

# 移入系统 bin 目录，方便在任意地方直接调用（可选）
sudo mv tgup /usr/local/bin/

```


这里是为您整理好的 `tgup` 工具完整参数说明文档。您可以直接复制以下 Markdown 内容，无缝粘贴到您的项目 `README.md` 中：

---

## 🛠️ 参数选项说明

`tgup` 提供了丰富的命令行参数，用于精细化控制网盘媒体文件的筛选、并发流控以及转码行为。

| 长参数 | 短参数别名 | 默认值 | 允许值 / 示例 | 功能描述 |
| --- | --- | --- | --- | --- |
| `--title` | 无 | 空 | `"标题内容 #标签"` | 为发送的媒体相册（Media Group）设置全局统一的文本标题/文字介绍。 |
| `--type` | 无 | `all` | `pic` / `video` / `m4v` / `avi` | **媒体流筛选器**。<br>

<br>• `pic`: 只传图片<br>

<br>• `video` (或 `vedio`): 只传视频<br>

<br>• 填入具体后缀（如 `m4v`, `mp4`, `png`）: **精准只上传该后缀的文件**。 |
| `--transcode` | 无 | `false` | `true` / `false` | **不落盘实时转码开关**。开启后，对于 `avi/mpg/m4v` 等无法在 Telegram 直接在线播放的格式，通过 FFmpeg 边转码边上传，且不占用磁盘空间。 |
| `-n` | 无 | `10` | `1 ~ 10` | 单次打包发送的相册文件数上限。Telegram 官方限制相册最大为 10，超过 10 自动防呆重设为 10。 |
| `-s` | 无 | `4` | 整数（秒） | 批次（相册）上传成功后的**战略冷却休眠时间**，用以安全渡过 Telegram 云端的风控权重计算，防频控。 |
| `--cache-force` | `--cf` | `false` | `true` / `false` | 强行粉碎并刷新本地现有的缩略图（`.jpg`）与音视频元数据（`.json`）强缓存。 |
| `--test` | `-t` | 空 | `curl` | **测试/调试模式**。使用 `-t=curl` 不会真实上传，而是直接打印 1:1 的标准 `curl` 封包报文以供审查。 |

---

## 💡 高级特性特性特性说明

### 1. 智能参数乱序混放 (GNU getopt 风格)

由于内部集成了参数自动重排器，你不需要死板地遵守“参数必须在路径前面”的规则。以下两种写法**完全等价**，工具会自动把路径剥离并置后：

```bash
# 标准写法
tgup --title="测试" -type=m4v --transcode /path/to/dir

# 乱序混放（依然能完美识别 --transcode）
tgup --title="测试" /path/to/dir -type=m4v --transcode

```

### 2. WebDAV 专属级全闭环流控

* **按需延迟探测**：不再一启动就全盘扫描。仅在轮到当前批次时，才对该批次的视频进行单线程顺序 `ffprobe` 元数据提取，且文件间强制微歇 600ms，完美隐藏 WebDAV 挂载层的 HTTP Range 密集请求，防止网盘封号。
* **双重本地强缓存**：首次探测成功后，本地会同时生成 `.jpg` 缩略图（严格限边 320px 匹配 TG 规范）和 `.json` 像素元数据。二次运行直接毫秒级读取本地缓存，对网盘达到 **0 字节**网络开销。

### 3. 常数级 $O(1)$ 内存流与 H.264 实时管道

* 抛弃了传统的内存缓冲区，采用 `io.Pipe` 管道网络直冲。
* 配合 `--transcode` 时，FFmpeg 转换后的分片数据直接作为 HTTP 流写入网络，达成“网盘读 1MB -> FFmpeg 转 1MB -> 脚本发 1MB” 的零磁盘落盘占用、零内存堆积。

### 4. Flood Wait 风控自我愈合机制

当一次性投递数十个 G 的大媒体导致 Local Bot API 触发云端限频，返回 `Too Many Requests: retry after X` 错误时，程序会自动高精度截取 X 秒并进入睡眠，醒来后**全自动原地复活重试**，最多重试 5 次，无需人工干预。

---

## 📖 常用场景示例命令

* **场景一：正常混合打包上传，并加入文字介绍**
```bash
tgup --title="今日苹果原相机街拍 #国产" /www/nas/CloudDrive/115open/up/目录

```


* **场景二：指定每次只发 5 个，且发完一组强行歇 10 秒（极稳网盘控流流方案）**
```bash
tgup -n=5 -s=10 /www/nas/CloudDrive/115open/up/目录

```


* **场景三：只要目录下的 m4v 文件，并强制开启 FFmpeg 实时转码为标准 MP4（保证 TG 内点播秒开不黑屏）**
```bash
tgup -type=m4v --transcode /www/nas/CloudDrive/115open/up/目录

```


* **场景四：调试审查，看看带有苹果原相机 90 度旋转的视频，在打包成 `curl` 表单时高宽是否被自动摆正**
```bash
tgup -t=curl --cache-force /www/nas/CloudDrive/115open/up/苹果原相机93/www.98T.la@IMG_6485.mov

```