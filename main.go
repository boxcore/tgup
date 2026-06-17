package main

import (
	"bufio"
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
	ChatID    int64
	BotAPI    string
	TgAPIURL  string
	PhotoExts []string
	VideoExts []string
}

// VideoMetaCache 用于本地强缓存 WebDAV 视频的元数据，避免重复 ffprobe
type VideoMetaCache struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	Duration   int  `json:"duration"`
	IsPortrait bool `json:"is_portrait"`
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

func main() {
	// 【📢 混放参数自动识别器】：由于 Go 原生 flag 库在遇到第一个位置参数（路径）后会停止解析后面的参数，
	// 我们在这里手动将位置参数（路径）剥离并一律追加到最后面，完美达成 GNU getopt 选项随意混放的效果。
	var flagArgs []string
	var positionalArgs []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if !strings.Contains(arg, "=") {
				flagName := strings.TrimLeft(arg, "-")
				if flagName == "title" || flagName == "t" || flagName == "test" || flagName == "type" || flagName == "n" || flagName == "s" {
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

	flag.StringVar(&titleFlag, "title", "", "Specify a global caption title for the media group")
	flag.StringVar(&testFlag, "t", "", "Test mode: use '-t=curl' to print curl command")
	flag.StringVar(&testFlag, "test", "", "Test mode: use '--test=curl' to print curl command")
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force regenerate and overwrite existing thumbnails/photos")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force regenerate and overwrite existing thumbnails/photos (shorthand)")

	flag.IntVar(&batchSizeFlag, "n", 10, "Batch size per media group (max 10)")
	flag.StringVar(&typeFilterFlag, "type", "all", "Filter media type: pic, video, all, or specific ext like m4v")
	flag.IntVar(&sleepDurationFlag, "s", 4, "Sleep duration in seconds between batch uploads")
	flag.BoolVar(&transcodeFlag, "transcode", false, "Enable on-the-fly FFmpeg transcoding for non-standard videos")

	// 2. 官方标准解析
	flag.Parse()

	// 3. 获取剩余的位置参数（路径）
	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a file or directory path.")
		fmt.Println("Usage: go run main.go [-n=10] [-type=video] [--transcode] [-s=5] <path>")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	// 4. 统一别名状态与规范防呆
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

	// 6. 解析目标路径获取文件列表（内部已包含人类直觉自然数字排序逻辑）
	rawFiles, err := collectFiles(targetPath)
	if err != nil {
		fmt.Printf("Error accessing path: %v\n", err)
		os.Exit(1)
	}

	// 7. 高级动态过滤：根据 -type 参数（大类或具体后缀）进行细分筛选
	var files []string
	typeFilterFlag = strings.ToLower(strings.TrimSpace(typeFilterFlag))
	for _, file := range rawFiles {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(config.PhotoExts, ext)
		isVideo := contains(config.VideoExts, ext)

		// 优先处理特定文件后缀过滤（如 -type=m4v 或 -type=.m4v）
		if typeFilterFlag != "all" && typeFilterFlag != "pic" && typeFilterFlag != "video" && typeFilterFlag != "vedio" {
			targetExt := typeFilterFlag
			if !strings.HasPrefix(targetExt, ".") {
				targetExt = "." + targetExt
			}
			if ext != targetExt {
				continue
			}
		} else {
			// 处理常规大类过滤
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
		bot.Debug = false
	}

	// 9. 动态分批处理
	for i := 0; i < len(files); i += batchSizeFlag {
		end := i + batchSizeFlag
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]

		// 【WebDAV 专属流控】：只在轮到当前批次时，才对本批次进行单线程顺序探测，且处理完歇一歇
		if finalTestMode != "curl" {
			fmt.Printf("⚡ 正在有序预处理当前批次 (%d/%d) 的 WebDAV 缓存...\n", i+1, end)
			for _, file := range batch {
				ext := strings.ToLower(filepath.Ext(file))
				if contains(config.VideoExts, ext) {
					_, _, _, _, _, _ = getVideoDataUnified(file, cacheDir, cacheForceFlag)
					time.Sleep(600 * time.Millisecond)
				}
			}
		}

		if finalTestMode == "curl" {
			generateCurlCommand(config, batch, globalCaption, cacheDir, cacheForceFlag)
		} else {
			fmt.Printf("\n--- Preparing batch: %d to %d (Total: %d) ---\n", i+1, end, len(files))
			uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag, transcodeFlag)

			// 批次间物理隔离冷却
			if end < len(files) && sleepDurationFlag > 0 {
				fmt.Printf("🛋️ 批次发送完毕，主动进入 %d 秒战略冷却...\n", sleepDurationFlag)
				time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
			}
		}
	}

	if finalTestMode != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
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

	fmt.Println("\n--- Generated Test CURL Command (With Image Optimizer) ---")
	fmt.Printf("curl -X POST \"%s\" \\\n", apiURL)
	fmt.Printf("     -F \"chat_id=%d\" \\\n", cfg.ChatID)
	escapedMediaJSON := strings.ReplaceAll(mediaJSONStr, `"`, `\"`)
	fmt.Printf("     -F \"media=%s\" \\\n", escapedMediaJSON)
	fmt.Println(strings.Join(fileFormFields, " \\\n"))
	fmt.Println("----------------------------------------------------------------------")
}

func writeLog(logDir string, filename string, content string) {
	logPath := filepath.Join(logDir, filename)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to write log to %s: %v\n", filename, err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

// loadConfig 智能防呆版：全自动修复前导点号大小写异常
func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &Config{
		PhotoExts: []string{".jpg", ".jpeg", ".png"},
		VideoExts: []string{".mp4", ".mkv", ".avi", ".mov", ".m4v"},
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

// collectFiles 内置人类直觉自然数字排序内核
func collectFiles(targetPath string) ([]string, error) {
	fi, err := os.Stat(targetPath)
	if err != nil {
		return nil, err
	}

	var files []string
	if !fi.IsDir() {
		return []string{targetPath}, nil
	}

	err = filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 🚀 【关键修复】：应用自定义的高精自然数字排序算法，纠正 1, 100, 11 倒序问题
	sort.Slice(files, func(i, j int) bool {
		return isLessNatural(files[i], files[j])
	})

	return files, nil
}

// uploadMediaGroup 自适应流式控流版：结合 io.Pipe 和 2核VPS极速降维转码管道
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
				Type:              "video",
				Media:             fmt.Sprintf("attach://%s", attachKey),
				Caption:           currentCaption,
				Width:             width,
				Height:            height,
				Duration:          duration,
				SupportsStreaming: true,
				Thumb:             thumbValue,
			})

			vExt := strings.ToLower(filepath.Ext(file))
			needTrans := transcodeFlag && (vExt == ".avi" || vExt == ".mpg" || vExt == ".mpeg" || vExt == ".wmv" || vExt == ".m4v" || vExt == ".flv")

			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true, NeedTranscode: needTrans})
			videoIdx++

		} else if isPhoto {
			attachKey := fmt.Sprintf("photo%d", photoIdx)
			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    "photo",
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: finalPhotoPath, ShowProgress: true, NeedTranscode: false})
			photoIdx++
		} else {
			attachKey := fmt.Sprintf("doc%d", docIdx)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    "document",
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
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
					// 🚀 2核CPU性能压榨转码引擎：指定 veryfast 预设、threads双核硬限、等比降维 720p
					cmd := exec.Command("ffmpeg", "-y", "-threads", "2", "-i", p.FilePath,
						"-c:v", "libx264", "-preset", "veryfast",
						"-vf", "scale='if(gt(iw,ih),-2,720)':'if(gt(iw,ih),720,-2)'",
						"-pix_fmt", "yuv420p", "-profile:v", "main", "-level:v", "4.0",
						"-c:a", "aac", "-b:a", "128k", "-f", "mp4",
						"-movflags", "frag_keyframe+empty_moov+default_base_moof", "pipe:1",
					)
					cmd.Stdout = part

					var bar *progressbar.ProgressBar
					if p.ShowProgress {
						bar = progressbar.NewOptions(-1,
							progressbar.OptionSetDescription(fmt.Sprintf("[⚙️转码上传] %s", filepath.Base(p.FilePath))),
							progressbar.OptionSetWriter(os.Stderr),
							progressbar.OptionShowBytes(true),
							progressbar.OptionSetWidth(15),
							progressbar.OptionThrottle(65),
							progressbar.OptionSpinnerType(14),
							progressbar.OptionFullWidth(),
						)
						cmd.Stdout = io.MultiWriter(part, bar)
					}

					errRun := cmd.Run()
					if bar != nil {
						fmt.Fprint(os.Stderr, "\n")
					}
					if errRun != nil {
						return
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
						bar := progressbar.NewOptions64(
							fi.Size(),
							progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", descStr)),
							progressbar.OptionSetWriter(os.Stderr),
							progressbar.OptionShowBytes(true),
							progressbar.OptionSetWidth(15),
							progressbar.OptionThrottle(65),
							progressbar.OptionShowCount(),
							progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }),
							progressbar.OptionSpinnerType(14),
							progressbar.OptionFullWidth(),
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
		if err != nil {
			fmt.Printf("Upload request failed: %v\n", err)
			return
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Println("Batch upload success.")
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

			// 🚀 【精准优化点】：引入渐进式退避算法
			// 机制：官方要求秒数 + 5秒基础缓冲 + (当前重试轮次 * 15秒延迟叠加)
			// 第一轮重试：等 8 + 5 + 0 = 13秒
			// 第二轮重试：等 8 + 5 + 15 = 28秒（让云端滑动窗口彻底冷却）
			actualWait := waitSeconds + 5 + (retry * 15)

			fmt.Printf("\n⚠️ 触发 Telegram 官方频控。服务器返回: %s\n", bodyStr)
			fmt.Printf("💤 为彻底清空云端权重，当前第 %d 次重试将主动高精度休眠 %d 秒...\n\n", retry+1, actualWait)

			time.Sleep(time.Duration(actualWait) * time.Second)
			continue
		}

		fmt.Printf("\nTelegram API Error: Status %d, Body: %s\n", resp.StatusCode, bodyStr)
		return
	}
	fmt.Println("\n❌ 当前批次已连续重试失败达到上限，放弃该批次。")
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
		return 0, 0, 0, "", false, fmt.Errorf("ffprobe cloud query failed: %v", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(outProbe)))
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

	time.Sleep(400 * time.Millisecond)

	rawTempJpg := filepath.Join(cacheDir, "raw_extracted_frame.jpg")
	_ = os.Remove(rawTempJpg)
	cmdImg := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", rawTempJpg)
	if err := cmdImg.Run(); err != nil {
		cmdFallback := exec.Command("ffmpeg", "-y", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
		if errFallback := cmdFallback.Run(); errFallback != nil {
			return 0, 0, 0, "", false, fmt.Errorf("ffmpeg frame extraction failed: %v", errFallback)
		}
	}
	defer os.Remove(rawTempJpg)

	rawFile, err := os.Open(rawTempJpg)
	if err != nil {
		return 0, 0, 0, "", false, err
	}
	srcImg, _, err := image.Decode(rawFile)
	rawFile.Close()
	if err != nil {
		return 0, 0, 0, "", false, err
	}

	bounds := srcImg.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()

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
	if newW == 0 {
		newW = 2
	}
	if newHeight == 0 {
		newHeight = 2
	}

	dstImg := image.NewRGBA(image.Rect(0, 0, newW, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)

	outFile, err := os.Create(finalThumbPath)
	if err != nil {
		return 0, 0, 0, "", false, err
	}
	err = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 92})
	outFile.Close()
	if err != nil {
		return 0, 0, 0, "", false, err
	}

	if newHeight > newW {
		isPortrait = true
	}

	newCacheData := VideoMetaCache{Width: w, Height: h, Duration: duration, IsPortrait: isPortrait}
	metaBytes, _ := json.Marshal(newCacheData)
	_ = os.WriteFile(finalMetaPath, metaBytes, 0644)

	return w, h, duration, finalThumbPath, isPortrait, nil
}

func contains(slice []string, val string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, val) {
			return true
		}
	}
	return false
}

// isLessNatural 核心算法：完美支持 1, 2, 17, 105 升序人类直觉重排
func isLessNatural(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			startA := i
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			numA, _ := strconv.ParseUint(a[startA:i], 10, 64)

			startB := j
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			numB, _ := strconv.ParseUint(b[startB:j], 10, 64)

			if numA != numB {
				return numA < numB
			}
		} else {
			if a[i] != b[j] {
				return a[i] < b[j]
			}
			i++
			j++
		}
	}
	return len(a) < len(b)
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}
