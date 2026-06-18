package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/image/draw"
)

// Config 存储配置文件信息
type Config struct {
	ChatID        int64
	BotAPI        string
	TgAPIURL      string
	PhotoExts     []string
	VideoExts     []string
	AllowSpilFile bool    // ALLOW_SPIL_FILE：是否允许大视频切片
	AllowMaxSize  int64   // ALLOW_MAX_SIZE：允许读取/切片的最大视频尺寸
	SpilMaxSize   float64 // SPIL_MAX_SIZE：单片切片容量上限（单位GB，如1.5）
}

// VideoMetaCache 用于本地强缓存 WebDAV 视频的元数据，避免重复 ffprobe
type VideoMetaCache struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	Duration   int  `json:"duration"`
	IsPortrait bool `json:"is_portrait"`
}

// OrgVideoCache 超大视频本地下载与持久化元数据说明档结构体
type OrgVideoCache struct {
	OrigFilename   string `json:"orig_filename"`    // 视频原始文件名
	Size           int64  `json:"size"`             // 视频文件大小
	Width          int    `json:"width"`            // 视频宽度
	Height         int    `json:"height"`           // 视频高度
	Duration       int    `json:"duration"`         // 视频时间长度（秒）
	LocalCachePath string `json:"local_cache_path"` // 下载后的本地 cache 文件绝对路径
	Uploaded       bool   `json:"uploaded"`         // 是否已经成功上传完毕
}

// FilePayload 存储流式传输管道中每个文件的元数据
type FilePayload struct {
	FieldKey      string
	FilePath      string
	ShowProgress  bool
	NeedTranscode bool
}

// TgMediaItem 统一的 Telegram 媒体节点 JSON 映射结构体
type TgMediaItem struct {
	Type              string `json:"type"`
	Media             string `json:"media"`
	Caption           string `json:"caption,omitempty"`
	Width             int    `json:"width,omitempty"`
	Height            int    `json:"height,omitempty"`
	Duration          int    `json:"duration,omitempty"`
	SupportsStreaming bool   `json:"supports_streaming,omitempty"`
	Thumb             string `json:"thumb,omitempty"`
}

// fileSortItem 用来做 WebDAV 单次时间与大小快照缓存
type fileSortItem struct {
	path    string
	modTime time.Time
	creTime time.Time
	size    int64 // 文件大小快照（字节）
}

func main() {
	// 【📢 混放参数自动识别器】：完美达成 GNU getopt 选项随意混放的效果。
	var flagArgs []string
	var positionalArgs []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if !strings.Contains(arg, "=") {
				flagName := strings.TrimLeft(arg, "-")
				if flagName == "title" || flagName == "t" || flagName == "test" || flagName == "type" || flagName == "n" || flagName == "s" || flagName == "sort" || flagName == "spil" {
					if i+1 < len(os.Args) {
						flagArgs = append(flagArgs, os.Args[i+1])
						i++
					}
				}
			}
		} else {
			positionalArgs = append(positionalArgs, arg)
		}
	}
	os.Args = append(append([]string{os.Args[0]}, flagArgs...), positionalArgs...)

	// 1. 定义 Flags
	var titleFlag string
	var testFlag string
	var cacheForceFlag bool
	var batchSizeFlag int
	var typeFilterFlag string
	var sleepDurationFlag int
	var transcodeFlag bool
	var sortFlag string
	var spilFlag bool

	flag.StringVar(&titleFlag, "title", "", "Specify a global caption title for the media group")
	flag.StringVar(&testFlag, "t", "", "Test mode: use '-t=curl' to print curl command, '-t=list' to show files")
	flag.StringVar(&testFlag, "test", "", "Test mode: use '--test=curl' to print curl command, '--test=list' to show files")
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force regenerate and overwrite existing thumbnails/photos")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force regenerate and overwrite existing thumbnails/photos (shorthand)")

	flag.IntVar(&batchSizeFlag, "n", 10, "Batch size per media group (max 10)")
	flag.StringVar(&typeFilterFlag, "type", "all", "Filter media type: pic, video, all, or specific ext like m4v")
	flag.IntVar(&sleepDurationFlag, "s", 4, "Sleep duration in seconds between batch uploads")
	flag.BoolVar(&transcodeFlag, "transcode", false, "Enable on-the-fly FFmpeg transcoding for non-standard videos")
	flag.StringVar(&sortFlag, "sort", "name", "Sort files by: name, mod, create, size, size_desc")
	flag.BoolVar(&spilFlag, "spil", false, "Force enable on-the-fly video splitting for files > 2GB")

	// 2. 官方标准解析
	flag.Parse()

	// 3. 获取路径
	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a file or directory path.")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
	}
	if batchSizeFlag > 10 || batchSizeFlag <= 0 {
		batchSizeFlag = 10
	}
	if sleepDurationFlag < 0 {
		sleepDurationFlag = 0
	}

	// 5. 初始化环境与加载配置
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Error getting home directory: %v\n", err)
		os.Exit(1)
	}
	baseDir := filepath.Join(homeDir, ".tgup")
	cacheDir := filepath.Join(baseDir, "cache")
	logDir := filepath.Join(baseDir, "logs")

	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(logDir, 0755)

	configPath := filepath.Join(baseDir, "config.conf")
	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Printf("Error loading config from %s: %v\n", configPath, err)
		os.Exit(1)
	}

	if spilFlag {
		config.AllowSpilFile = true
	}

	// 6. 解析目标路径获取文件列表
	rawFiles, err := collectFiles(targetPath, sortFlag)
	if err != nil {
		fmt.Printf("Error accessing path: %v\n", err)
		os.Exit(1)
	}

	// 7. 高级动态过滤
	var files []string
	typeFilterFlag = strings.ToLower(strings.TrimSpace(typeFilterFlag))
	for _, file := range rawFiles {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(config.PhotoExts, ext)
		isVideo := contains(config.VideoExts, ext)

		if typeFilterFlag != "all" && typeFilterFlag != "pic" && typeFilterFlag != "video" && typeFilterFlag != "vedio" {
			targetExt := typeFilterFlag
			if !strings.HasPrefix(targetExt, ".") {
				targetExt = "." + targetExt
			}
			if ext != targetExt {
				continue
			}
		} else {
			if typeFilterFlag == "pic" && !isPhoto {
				continue
			}
			if (typeFilterFlag == "video" || typeFilterFlag == "vedio") && !isVideo {
				continue
			}
		}

		if !isPhoto && !isVideo {
			continue
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		fmt.Println("No valid files found matching the specific type criteria.")
		return
	}

	// 🚀 【高级审计拦截器】：处理 -t=list 预检模式
	if finalTestMode == "list" {
		totalBatches := (len(files) + batchSizeFlag - 1) / batchSizeFlag
		fmt.Printf("\n📋 预检就绪：过滤与排序完成的文件列表 (共 %d 个，每批 %d 个，将分 %d 批循环执行):\n", len(files), batchSizeFlag, totalBatches)
		fmt.Println(strings.Repeat("-", 60))

		for idx, file := range files {
			fi, err := os.Stat(file)
			sizeStr := "Unknown"
			cTimeStr := "--"
			mTimeStr := "--"

			if err == nil {
				sizeStr = formatSize(fi.Size())
				mTime := fi.ModTime()
				cTime := mTime

				if sys := fi.Sys(); sys != nil {
					v := reflect.ValueOf(sys).Elem()
					statField := v.FieldByName("Ctim")
					if !statField.IsValid() {
						statField = v.FieldByName("Ctimespec")
					}
					if statField.IsValid() {
						secField := statField.FieldByName("Sec")
						nsecField := statField.FieldByName("Nsec")
						if secField.IsValid() && nsecField.IsValid() {
							cTime = time.Unix(secField.Int(), nsecField.Int())
						}
					}
				}
				cTimeStr = cTime.Format("2006-01-02 15:04")
				mTimeStr = mTime.Format("2006-01-02 15:04")
			}
			fmt.Printf("[%03d] %s (%s | C:%s M:%s)\n", idx+1, filepath.Base(file), sizeStr, cTimeStr, mTimeStr)

			if (idx+1)%batchSizeFlag == 0 || (idx+1) == len(files) {
				currentBatch := (idx / batchSizeFlag) + 1
				itemsInBatch := batchSizeFlag
				if (idx+1) == len(files) && len(files)%batchSizeFlag != 0 {
					itemsInBatch = len(files) % batchSizeFlag
				}
				fmt.Printf("📦 👆 以上为 [第 %d / %d 批次] 预检包 (包含 %d 个文件) 👆\n", currentBatch, totalBatches, itemsInBatch)
				fmt.Println(strings.Repeat("-", 60))
			}
		}
		return
	}

	if finalTestMode != "curl" {
		fmt.Printf("Found %d file(s) to process after filtering.\n", len(files))
	}

	// 8. 决定全局标题
	globalCaption := strings.TrimSpace(titleFlag)
	if globalCaption == "" && finalTestMode != "curl" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Do you want to add a caption for this upload? (Press Enter to skip): ")
		input, _ := reader.ReadString('\n')
		globalCaption = strings.TrimSpace(input)
	}

	var bot *tgbotapi.BotAPI
	if finalTestMode != "curl" {
		bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(config.BotAPI, config.TgAPIURL+"/bot%s/%s")
		if err != nil {
			fmt.Printf("Error initializing bot: %v\n", err)
			os.Exit(1)
		}
	}

	// 9. 🚀 【核心动态分批调度内核】
	var currentBatch []string
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		fi, err := os.Stat(file)

		isLargeVideo := false
		if err == nil && contains(config.VideoExts, ext) && fi.Size() > 2000*1024*1024 {
			isLargeVideo = true
		}

		if isLargeVideo {
			if !config.AllowSpilFile {
				fmt.Printf("⚠️ 跳过大视频文件 (未激活切片开关 ALLOW_SPIL_FILE): %s (%s)\n", filepath.Base(file), formatSize(fi.Size()))
				continue
			}
			if fi.Size() > config.AllowMaxSize {
				fmt.Printf("❌ 超过最大文件尺寸限制 (ALLOW_MAX_SIZE: %s)，强制跳过该大文件: %s (%s)\n", formatSize(config.AllowMaxSize), filepath.Base(file), formatSize(fi.Size()))
				continue
			}

			// 触发强制 Flush：先把前面组装好的队列统统送出
			if len(currentBatch) > 0 {
				fmt.Printf("\n⚡ 发现超大视频，正在前置强制性上传当前已积攒的普通批次群组 (包含 %d 个文件)...\n", len(currentBatch))
				preProcessAndUpload(bot, config, currentBatch, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
				currentBatch = nil
				if sleepDurationFlag > 0 {
					time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
				}
			}

			// 单独开启超级大视频的智能断点切片总控流程
			handleSplitVideoUpload(bot, config, file, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
			if sleepDurationFlag > 0 {
				time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
			}
		} else {
			currentBatch = append(currentBatch, file)
			if len(currentBatch) == batchSizeFlag {
				fmt.Printf("\n--- Preparing batch: %d 个普通多媒体包，发起正常投递 ---\n", batchSizeFlag)
				preProcessAndUpload(bot, config, currentBatch, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
				currentBatch = nil
				if sleepDurationFlag > 0 {
					time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
				}
			}
		}
	}

	if len(currentBatch) > 0 {
		fmt.Printf("\n--- Preparing batch: 上传最后一批剩余功能文件 (包含 %d 个文件) ---\n", len(currentBatch))
		preProcessAndUpload(bot, config, currentBatch, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
	}

	if finalTestMode != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
}

func preProcessAndUpload(bot *tgbotapi.BotAPI, config *Config, batch []string, cacheDir string, cacheForceFlag bool, globalCaption string, logDir string, transcodeFlag bool, finalTestMode string) {
	if finalTestMode != "curl" {
		for _, f := range batch {
			ext := strings.ToLower(filepath.Ext(f))
			if contains(config.VideoExts, ext) {
				_, _, _, _, _, _ = getVideoDataUnified(f, cacheDir, cacheForceFlag)
				time.Sleep(600 * time.Millisecond)
			}
		}
	}
	if finalTestMode == "curl" {
		generateCurlCommand(config, batch, globalCaption, cacheDir, cacheForceFlag)
	} else {
		uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag, transcodeFlag)
	}
}

// handleSplitVideoUpload 【优化版】：前置秒级提取元数据特征 Token，彻底消灭流量悖论
func handleSplitVideoUpload(bot *tgbotapi.BotAPI, cfg *Config, origPath string, cacheDir string, cacheForce bool, globalCaption string, logDir string, transcodeFlag bool, finalTestMode string) {
	ext := strings.ToLower(filepath.Ext(origPath))
	baseName := filepath.Base(origPath)

	fi, err := os.Stat(origPath)
	if err != nil {
		fmt.Printf("❌ 无法读取网盘源文件元数据: %v\n", err)
		return
	}

	// 🚀 【灵魂优化】：利用 0 流量开销的 [路径+大小+修改时间时间戳] 混合拼装算出唯一元数据 Token
	hasherToken := sha1.New()
	hasherToken.Write([]byte(origPath + strconv.FormatInt(fi.Size(), 10) + strconv.FormatInt(fi.ModTime().Unix(), 10)))
	token := hex.EncodeToString(hasherToken.Sum(nil))

	jsonPath := filepath.Join(cacheDir, fmt.Sprintf("org_%s.json", token))
	localOrgPath := filepath.Join(cacheDir, fmt.Sprintf("org_%s%s", token, ext))

	var cacheData OrgVideoCache
	hitCacheFile := false

	if !cacheForce {
		if jsonBytes, err := os.ReadFile(jsonPath); err == nil {
			if json.Unmarshal(jsonBytes, &cacheData) == nil {
				// 状态一：如果该特征码在历史记录中已经彻底上传成功过，直接 0 流量秒级过！
				if cacheData.Uploaded {
					fmt.Printf("🎉 [元数据强缓存命中] 该超大视频历史任务已成功切片投递，直接跳过！\n文件：%s\n", baseName)
					return
				}
				// 状态二：如果上次断电/中断，但检查本地 cache 目录下还躺着完整的 org_ 原始大视频
				if fiLocal, errFile := os.Stat(cacheData.LocalCachePath); errFile == nil && fiLocal.Size() == fi.Size() {
					fmt.Printf("📦 [原始文件缓存命中] 本地发现先前下载完整的大视频缓冲，跳过重复拉取，直接切片！\n路径：%s\n", cacheData.LocalCachePath)
					localOrgPath = cacheData.LocalCachePath
					hitCacheFile = true
				}
			}
		}
	}

	// 状态三：本地没有找到完整缓存，启动高速流式落盘拉取
	if !hitCacheFile {
		fmt.Println("⏳ 本地未见缓存，正在从网盘高速流式拉取数据到本地 Cache...")
		src, err := os.Open(origPath)
		if err != nil {
			fmt.Printf("❌ 无法读取网盘源文件路径: %v\n", err)
			return
		}
		defer src.Close()

		tmpFile, err := os.CreateTemp(cacheDir, "tgup_org_download_")
		if err != nil {
			fmt.Printf("❌ 无法创建本地高速缓冲临时文件: %v\n", err)
			return
		}
		defer os.Remove(tmpFile.Name())

		bar := progressbar.NewOptions64(
			fi.Size(),
			progressbar.OptionSetDescription("[本地快速缓存中]"),
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionShowBytes(true),
			progressbar.OptionSetWidth(15),
			progressbar.OptionThrottle(65),
			progressbar.OptionShowCount(),
			progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }),
			progressbar.OptionSpinnerType(14),
			progressbar.OptionFullWidth(),
		)

		// 纯流式对拷，不需要额外在 MultiWriter 里去边算 Hash 边写，省下大量 CPU 算力开销
		_, err = io.Copy(io.MultiWriter(tmpFile, bar), src)
		tmpFile.Close()
		if err != nil {
			fmt.Printf("❌ 视频大文件拉取落盘本地缓存失败: %v\n", err)
			return
		}

		_ = os.Remove(localOrgPath)
		if err := os.Rename(tmpFile.Name(), localOrgPath); err != nil {
			fmt.Printf("❌ 修正规范原始文件名失败: %v\n", err)
			return
		}

		// 🎬 下载落盘后，本地极速高精探测音视频物理属性（0秒网络损耗）
		w, h, duration := probeLocalVideo(localOrgPath)

		// 组装并持久化元数据 JSON 档
		cacheData = OrgVideoCache{
			OrigFilename:   baseName,
			Size:           fi.Size(),
			Width:          w,
			Height:         h,
			Duration:       duration,
			LocalCachePath: localOrgPath,
			Uploaded:       false, // 此时切片还没完结，先置为 false
		}

		metaBytes, _ := json.MarshalIndent(cacheData, "", "  ")
		_ = os.WriteFile(jsonPath, metaBytes, 0644)
	}

	// 🚀 体积向时间的智能映射算法内核
	totalSizeGB := float64(cacheData.Size) / (1024.0 * 1024.0 * 1024.0)
	segmentTimeSec := int(math.Floor((cfg.SpilMaxSize / totalSizeGB) * float64(cacheData.Duration)))
	if segmentTimeSec <= 0 {
		segmentTimeSec = 300
	}

	outputPattern := filepath.Join(cacheDir, fmt.Sprintf("split_%s_%%03d%s", token, ext))
	fmt.Printf("⚙️ 正在调用 FFmpeg 启动秒级无损流拷贝切割 (根据体积智能映射单片时长: %d 秒)...\n", segmentTimeSec)

	cmd := exec.Command("ffmpeg", "-y", "-i", localOrgPath,
		"-c", "copy", "-map", "0",
		"-f", "segment", "-segment_time", strconv.Itoa(segmentTimeSec),
		"-reset_timestamps", "1",
		outputPattern,
	)

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		fmt.Printf("❌ FFmpeg 无损分段切片失败: %v, 详情: %s\n", err, errBuf.String())
		return
	}

	globPattern := filepath.Join(cacheDir, fmt.Sprintf("split_%s_*%s", token, ext))
	splitPieces, err := filepath.Glob(globPattern)
	if err != nil || len(splitPieces) == 0 {
		fmt.Println("❌ 未检测到任何切片产生的视频碎片文件。")
		return
	}

	sort.Slice(splitPieces, func(i, j int) bool { return isLessNatural(splitPieces[i], splitPieces[j]) })

	fmt.Printf("📦 切片处理大功告成！共裂变生成 %d 个合格的分段短视频，开始作为独立相册投递...\n", len(splitPieces))
	preProcessAndUpload(bot, cfg, splitPieces, cacheDir, cacheForce, globalCaption, logDir, transcodeFlag, finalTestMode)

	// 🧹 【生产安全级善后打扫】：清空视频大体积缓存，固留最原始的说明 json 本地强缓存记录
	fmt.Println("🧹 上传动作结束，开始全自动深度洗刷本地大体积视频缓存...")
	_ = os.Remove(localOrgPath) // 粉碎本地原视频缓存
	for _, piece := range splitPieces {
		_ = os.Remove(piece) // 粉碎生成的 split_ 短分片视频

		hasherName := sha1.New()
		hasherName.Write([]byte(piece))
		pieceToken := hex.EncodeToString(hasherName.Sum(nil))
		_ = os.Remove(filepath.Join(cacheDir, pieceToken+".jpg"))
	}

	// 🔒 投递圆满完结，将持久化 JSON 标记Uploaded覆印为已完结状态
	cacheData.Uploaded = true
	metaBytes, _ := json.MarshalIndent(cacheData, "", "  ")
	_ = os.WriteFile(jsonPath, metaBytes, 0644)
	fmt.Println("✨ 本地大视频及分片缓存彻底洗刷完毕，仅保留 org_*.json 元数据记录，空间已完美释放！")
}

// probeLocalVideo 辅助函数：极速本地探测机制
func probeLocalVideo(path string) (w, h, duration int) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,duration", "-of", "default=noprint_wrappers=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "=")
		if len(parts) != 2 {
			continue
		}
		val := parts[1]
		switch parts[0] {
		case "width":
			w, _ = strconv.Atoi(val)
		case "height":
			h, _ = strconv.Atoi(val)
		case "duration":
			if idx := strings.Index(val, "."); idx != -1 {
				val = val[:idx]
			}
			duration, _ = strconv.Atoi(val)
		}
	}
	return
}

func checkAndResizePhoto(photoPath string, cacheDir string, cacheForce bool) (string, error) {
	file, err := os.Open(photoPath)
	if err != nil {
		return photoPath, err
	}
	defer file.Close()

	imgConfig, _, err := image.DecodeConfig(file)
	if err != nil {
		return photoPath, err
	}

	totalPixels := int64(imgConfig.Width) * int64(imgConfig.Height)
	const maxPixels = 19500000

	if totalPixels <= maxPixels {
		return photoPath, nil
	}

	ratio := math.Sqrt(float64(maxPixels) / float64(totalPixels))
	newWidth := int(float64(imgConfig.Width) * ratio)
	newHeight := int(float64(imgConfig.Height) * ratio)

	_, _ = file.Seek(0, 0)
	srcImg, _, err := image.Decode(file)
	if err != nil {
		return photoPath, err
	}

	dstImg := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)

	hasher := sha1.New()
	hasher.Write([]byte(fmt.Sprintf("%s_%d_%d", photoPath, newWidth, newHeight)))
	tmpName := fmt.Sprintf("resized_20m_%s.jpg", hex.EncodeToString(hasher.Sum(nil)))
	tmpPath := filepath.Join(cacheDir, tmpName)

	if !cacheForce {
		if _, err := os.Stat(tmpPath); err == nil {
			return tmpPath, nil
		}
	} else {
		_ = os.Remove(tmpPath)
	}

	outFile, err := os.Create(tmpPath)
	if err != nil {
		return photoPath, err
	}
	defer outFile.Close()

	err = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 95})
	if err != nil {
		return photoPath, err
	}

	return tmpPath, nil
}

func getImageDimensions(path string) (int, int) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func generateCurlCommand(cfg *Config, files []string, globalCaption string, cacheDir string, cacheForce bool) {
	var mediaJSONArray []TgMediaItem
	var fileFormFields []string
	isFirstItem := true

	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)

		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		var itemType, attachKey string

		if isVideo {
			itemType = "video"
			attachKey = fmt.Sprintf("video%d", videoIdx)

			width, height, duration, thumbPath, isPortraitVideo, thumbErr := getVideoDataUnified(file, cacheDir, cacheForce)
			thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
			var thumbValue string

			if thumbErr == nil && thumbPath != "" {
				absThumbPath, _ := filepath.Abs(thumbPath)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", thumbFormKey, absThumbPath))
			}

			if isPortraitVideo && width > height {
				width, height = height, width
			}

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:              itemType,
				Media:             fmt.Sprintf("attach://%s", attachKey),
				Caption:           currentCaption,
				Width:             width,
				Height:            height,
				Duration:          duration,
				SupportsStreaming: true,
				Thumb:             thumbValue,
			})
			videoIdx++

			absPath, _ := filepath.Abs(file)
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, absPath))

		} else if isPhoto {
			itemType = "photo"
			attachKey = fmt.Sprintf("photo%d", photoIdx)
			photoIdx++

			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			absPath, _ := filepath.Abs(finalPhotoPath)

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    itemType,
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, absPath))
		} else {
			itemType = "document"
			attachKey = fmt.Sprintf("doc%d", docIdx)
			docIdx++

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    itemType,
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			absPath, _ := filepath.Abs(file)
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, absPath))
		}
	}

	if len(mediaJSONArray) == 0 {
		return
	}

	mediaJSONBytes, _ := json.MarshalIndent(mediaJSONArray, "     ", "  ")
	mediaJSONStr := string(mediaJSONBytes)

	apiURL := fmt.Sprintf("%s/bot%s/sendMediaGroup", cfg.TgAPIURL, cfg.BotAPI)
	fmt.Printf("curl -X POST \"%s\" \\\n", apiURL)
	fmt.Printf("     -F \"chat_id=%d\" \\\n", cfg.ChatID)
	escapedMediaJSON := strings.ReplaceAll(mediaJSONStr, `"`, `\"`)
	fmt.Printf("     -F \"media=%s\" \\\n", escapedMediaJSON)
	fmt.Println(strings.Join(fileFormFields, " \\\n"))
}

func writeLog(logDir string, filename string, content string) {
	logPath := filepath.Join(logDir, filename)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &Config{
		PhotoExts:     []string{".jpg", ".jpeg", ".png"},
		VideoExts:     []string{".mp4", ".mkv", ".avi", ".mov", ".m4v"},
		AllowSpilFile: false,
		AllowMaxSize:  10 * 1024 * 1024 * 1024,
		SpilMaxSize:   1.5,
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		switch key {
		case "CHAT_ID":
			id, _ := strconv.ParseInt(val, 10, 64)
			cfg.ChatID = id
		case "BOT_API":
			cfg.BotAPI = val
		case "TG_API_URL":
			cfg.TgAPIURL = val
		case "ALLOW_SPIL_FILE":
			cfg.AllowSpilFile = strings.ToLower(val) == "true"
		case "ALLOW_MAX_SIZE":
			valLower := strings.ToLower(val)
			if strings.HasSuffix(valLower, "g") {
				numStr := strings.TrimSuffix(valLower, "g")
				if g, err := strconv.ParseInt(numStr, 10, 64); err == nil {
					cfg.AllowMaxSize = g * 1024 * 1024 * 1024
				}
			} else if strings.HasSuffix(valLower, "m") {
				numStr := strings.TrimSuffix(valLower, "m")
				if m, err := strconv.ParseInt(numStr, 10, 64); err == nil {
					cfg.AllowMaxSize = m * 1024 * 1024
				}
			} else {
				if bytesVal, err := strconv.ParseInt(val, 10, 64); err == nil {
					cfg.AllowMaxSize = bytesVal
				}
			}
		case "SPIL_MAX_SIZE":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				cfg.SpilMaxSize = f
			}
		case "PHOTO_EXTS":
			exts := strings.Split(strings.ToLower(val), ",")
			var cleanExts []string
			for _, e := range exts {
				e = strings.TrimSpace(e)
				if e != "" {
					if !strings.HasPrefix(e, ".") {
						e = "." + e
					}
					cleanExts = append(cleanExts, e)
				}
			}
			if len(cleanExts) > 0 {
				cfg.PhotoExts = cleanExts
			}
		case "VIDEO_EXTS":
			exts := strings.Split(strings.ToLower(val), ",")
			var cleanExts []string
			for _, e := range exts {
				e = strings.TrimSpace(e)
				if e != "" {
					if !strings.HasPrefix(e, ".") {
						e = "." + e
					}
					cleanExts = append(cleanExts, e)
				}
			}
			if len(cleanExts) > 0 {
				cfg.VideoExts = cleanExts
			}
		}
	}
	return cfg, scanner.Err()
}

func collectFiles(targetPath string, sortType string) ([]string, error) {
	fi, err := os.Stat(targetPath)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []string{targetPath}, nil
	}

	var items []fileSortItem
	err = filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			mTime := info.ModTime()
			cTime := mTime
			fSize := info.Size()

			if sys := info.Sys(); sys != nil {
				v := reflect.ValueOf(sys).Elem()
				statField := v.FieldByName("Ctim")
				if !statField.IsValid() {
					statField = v.FieldByName("Ctimespec")
				}
				if statField.IsValid() {
					secField := statField.FieldByName("Sec")
					nsecField := statField.FieldByName("Nsec")
					if secField.IsValid() && nsecField.IsValid() {
						cTime = time.Unix(secField.Int(), nsecField.Int())
					}
				}
			}
			items = append(items, fileSortItem{path: path, modTime: mTime, creTime: cTime, size: fSize})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortType = strings.ToLower(strings.TrimSpace(sortType))
	switch sortType {
	case "mod", "mtime":
		sort.Slice(items, func(i, j int) bool { return items[i].modTime.Before(items[j].modTime) })
	case "create", "ctime":
		sort.Slice(items, func(i, j int) bool { return items[i].creTime.Before(items[j].creTime) })
	case "size", "size_asc", "sizeasc":
		sort.Slice(items, func(i, j int) bool { return items[i].size < items[j].size })
	case "size_desc", "sizedesc":
		sort.Slice(items, func(i, j int) bool { return items[i].size > items[j].size })
	default:
		sort.Slice(items, func(i, j int) bool { return isLessNatural(items[i].path, items[j].path) })
	}

	var sortedFiles []string
	for _, item := range items {
		sortedFiles = append(sortedFiles, item.path)
	}
	return sortedFiles, nil
}

func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool, transcodeFlag bool) {
	var mediaJSONArray []TgMediaItem
	var payloads []FilePayload
	isFirstItem := true
	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)
		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		if isVideo {
			attachKey := fmt.Sprintf("video%d", videoIdx)
			width, height, duration, thumbPath, isPortraitVideo, _ := getVideoDataUnified(file, cacheDir, cacheForce)
			var thumbValue string
			if thumbPath != "" {
				thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				payloads = append(payloads, FilePayload{FieldKey: thumbFormKey, FilePath: thumbPath, ShowProgress: false, NeedTranscode: false})
			}
			if isPortraitVideo && width > height {
				width, height = height, width
			}

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type: "video", Media: fmt.Sprintf("attach://%s", attachKey), Caption: currentCaption,
				Width: width, Height: height, Duration: duration, SupportsStreaming: true, Thumb: thumbValue,
			})
			vExt := strings.ToLower(filepath.Ext(file))
			needTrans := transcodeFlag && (vExt == ".avi" || vExt == ".mpg" || vExt == ".mpeg" || vExt == ".wmv" || vExt == ".m4v" || vExt == ".flv")
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true, NeedTranscode: needTrans})
			videoIdx++
		} else if isPhoto {
			attachKey := fmt.Sprintf("photo%d", photoIdx)
			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "photo", Media: fmt.Sprintf("attach://%s", attachKey), Caption: currentCaption})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: finalPhotoPath, ShowProgress: true, NeedTranscode: false})
			photoIdx++
		} else {
			attachKey := fmt.Sprintf("doc%d", docIdx)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "document", Media: fmt.Sprintf("attach://%s", attachKey), Caption: currentCaption})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true, NeedTranscode: false})
			docIdx++
		}
	}

	maxRetries := 5
	for retry := 0; retry < maxRetries; retry++ {
		pipeReader, pipeWriter := io.Pipe()
		multipartWriter := multipart.NewWriter(pipeWriter)
		contentType := multipartWriter.FormDataContentType()

		go func() {
			defer pipeWriter.Close()
			defer multipartWriter.Close()
			_ = multipartWriter.WriteField("chat_id", strconv.FormatInt(chatID, 10))
			mediaJSONBytes, _ := json.Marshal(mediaJSONArray)
			_ = multipartWriter.WriteField("media", string(mediaJSONBytes))

			for _, p := range payloads {
				uploadFileName := filepath.Base(p.FilePath)
				if p.NeedTranscode {
					uploadFileName = strings.TrimSuffix(uploadFileName, filepath.Ext(uploadFileName)) + ".mp4"
				}
				part, err := multipartWriter.CreateFormFile(p.FieldKey, uploadFileName)
				if err != nil {
					return
				}

				if p.NeedTranscode {
					cmd := exec.Command("ffmpeg", "-y", "-threads", "2", "-i", p.FilePath,
						"-c:v", "libx264", "-preset", "veryfast", "-vf", "scale='if(gt(iw,ih),-2,720)':'if(gt(iw,ih),720,-2)'",
						"-pix_fmt", "yuv420p", "-profile:v", "main", "-level:v", "4.0", "-c:a", "aac", "-b:a", "128k", "-f", "mp4",
						"-movflags", "frag_keyframe+empty_moov+default_base_moof", "pipe:1",
					)
					cmd.Stdout = part
					var bar *progressbar.ProgressBar
					if p.ShowProgress {
						bar = progressbar.NewOptions(-1, progressbar.OptionSetDescription(fmt.Sprintf("[⚙️转码上传] %s", filepath.Base(p.FilePath))),
							progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowBytes(true), progressbar.OptionSetWidth(15), progressbar.OptionThrottle(65), progressbar.OptionSpinnerType(14), progressbar.OptionFullWidth(),
						)
						cmd.Stdout = io.MultiWriter(part, bar)
					}
					_ = cmd.Run()
					if bar != nil {
						fmt.Fprint(os.Stderr, "\n")
					}
				} else {
					file, err := os.Open(p.FilePath)
					if err != nil {
						return
					}
					if p.ShowProgress {
						fi, _ := file.Stat()
						descStr := filepath.Base(p.FilePath)
						if retry > 0 {
							descStr = fmt.Sprintf("[重试-%d] %s", retry, descStr)
						}
						bar := progressbar.NewOptions64(fi.Size(), progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", descStr)),
							progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowBytes(true), progressbar.OptionSetWidth(15), progressbar.OptionThrottle(65), progressbar.OptionShowCount(),
							progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }), progressbar.OptionSpinnerType(14), progressbar.OptionFullWidth(),
						)
						proxyReader := progressbar.NewReader(file, bar)
						_, _ = io.Copy(part, &proxyReader)
					} else {
						_, _ = io.Copy(part, file)
					}
					file.Close()
				}
			}
		}()

		apiURL := fmt.Sprintf("%s/bot%s/sendMediaGroup", cfg.TgAPIURL, cfg.BotAPI)
		req, err := http.NewRequest("POST", apiURL, pipeReader)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", contentType)

		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		currentTimeStr := time.Now().Format("2006-01-02 15:04:05")
		fileListStr := strings.Join(files, ", ")

		if err != nil {
			writeLog(logDir, "error.log", fmt.Sprintf("[%s] ERROR: %v | Files: [%s]\n", currentTimeStr, err, fileListStr))
			return
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Println("Batch upload success.")
			writeLog(logDir, "ok.log", fmt.Sprintf("[%s] SUCCESS: Status: %d | Files: [%s]\n", currentTimeStr, resp.StatusCode, fileListStr))
			return
		}

		bodyStr := string(respBody)
		if strings.Contains(bodyStr, "Too Many Requests") || strings.Contains(bodyStr, "retry after") {
			waitSeconds := 10
			if idx := strings.Index(bodyStr, "retry after "); idx != -1 {
				sub := bodyStr[idx+len("retry after "):]
				endIdx := 0
				for endIdx < len(sub) && sub[endIdx] >= '0' && sub[endIdx] <= '9' {
					endIdx++
				}
				if endIdx > 0 {
					if s, err := strconv.Atoi(sub[:endIdx]); err == nil {
						waitSeconds = s
					}
				}
			}
			actualWait := waitSeconds + 5 + (retry * 15)
			fmt.Printf("\n⚠️ 触发 Telegram 官方频控。服务器返回: %s\n💤 为彻底清空云端权重，当前第 %d 次重试将主动高精度休眠 %d 秒...\n\n", bodyStr, retry+1, actualWait)
			writeLog(logDir, "error.log", fmt.Sprintf("[%s] RETRY-%d: FloodWait %d sec | Files: [%s]\n", currentTimeStr, retry+1, actualWait, fileListStr))
			time.Sleep(time.Duration(actualWait) * time.Second)
			continue
		}
		fmt.Printf("\nTelegram API Error: Status %d, Body: %s\n", resp.StatusCode, bodyStr)
		writeLog(logDir, "error.log", fmt.Sprintf("[%s] ERROR: Status %d | Body: %s | Files: [%s]\n", currentTimeStr, resp.StatusCode, bodyStr, fileListStr))
		return
	}
}

func getVideoDataUnified(videoPath string, cacheDir string, cacheForce bool) (w, h, duration int, thumbPath string, isPortrait bool, err error) {
	hasherName := sha1.New()
	hasherName.Write([]byte(videoPath))
	token := hex.EncodeToString(hasherName.Sum(nil))
	finalThumbPath := filepath.Join(cacheDir, token+".jpg")
	finalMetaPath := filepath.Join(cacheDir, token+".json")

	isCacheValid := false
	var cachedMeta VideoMetaCache
	if !cacheForce {
		_, errThumb := os.Stat(finalThumbPath)
		metaBytes, errMeta := os.ReadFile(finalMetaPath)
		if errThumb == nil && errMeta == nil {
			if json.Unmarshal(metaBytes, &cachedMeta) == nil {
				tW, tH := getImageDimensions(finalThumbPath)
				if tW > 0 && tH > 0 && tW <= 320 && tH <= 320 {
					isCacheValid = true
				}
			}
		}
	}
	if isCacheValid {
		return cachedMeta.Width, cachedMeta.Height, cachedMeta.Duration, finalThumbPath, cachedMeta.IsPortrait, nil
	}

	_ = os.Remove(finalThumbPath)
	_ = os.Remove(finalMetaPath)
	time.Sleep(200 * time.Millisecond)

	cmdProbe := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,duration", "-of", "default=noprint_wrappers=1", videoPath)
	outProbe, err := cmdProbe.Output()
	if err != nil {
		return 0, 0, 0, "", false, fmt.Errorf("ffprobe cloud failed: %v", err)
	}
	parentScanner := bufio.NewScanner(strings.NewReader(string(outProbe)))
	for parentScanner.Scan() {
		line := parentScanner.Text()
		parts := strings.Split(line, "=")
		if len(parts) != 2 {
			continue
		}
		val := parts[1]
		switch parts[0] {
		case "width":
			w, _ = strconv.Atoi(val)
		case "height":
			h, _ = strconv.Atoi(val)
		case "duration":
			if idx := strings.Index(val, "."); idx != -1 {
				val = val[:idx]
			}
			duration, _ = strconv.Atoi(val)
		}
	}

	time.Sleep(400 * time.Millisecond)
	rawTempJpg := filepath.Join(cacheDir, "raw_extracted_frame.jpg")
	_ = os.Remove(rawTempJpg)
	cmdImg := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", rawTempJpg)
	if err := cmdImg.Run(); err != nil {
		cmdFallback := exec.Command("ffmpeg", "-y", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
		_ = cmdFallback.Run()
	}
	defer os.Remove(rawTempJpg)

	rawFile, err := os.Open(rawTempJpg)
	if err != nil {
		return w, h, duration, "", false, nil
	}
	srcImg, _, err := image.Decode(rawFile)
	rawFile.Close()
	if err != nil {
		return w, h, duration, "", false, nil
	}

	bounds := srcImg.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	maxSize := 320
	var newW, newHeight int
	if srcW > srcH {
		newW = maxSize
		newHeight = int(float64(srcH) * (float64(maxSize) / float64(srcW)))
	} else {
		newHeight = maxSize
		newW = int(float64(srcW) * (float64(maxSize) / float64(srcH)))
	}
	newW = (newW / 2) * 2
	newHeight = (newHeight / 2) * 2

	dstImg := image.NewRGBA(image.Rect(0, 0, newW, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)
	outFile, err := os.Create(finalThumbPath)
	if err == nil {
		_ = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 92})
		outFile.Close()
	}

	if newHeight > newW {
		isPortrait = true
	}
	newCacheData := VideoMetaCache{Width: w, Height: h, Duration: duration, IsPortrait: isPortrait}
	metaBytes, _ := json.Marshal(newCacheData)
	_ = os.WriteFile(finalMetaPath, metaBytes, 0644)

	return w, h, duration, finalThumbPath, isPortrait, nil
}
