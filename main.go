package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif" // 注册解码器
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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

func main() {
	// 1. 定义 Flags
	var titleFlag string
	var testFlag string

	// 2. 兼容任意位置的位置参数与 Flags：手动从 os.Args 中分离出路径
	var targetPath string
	var remainArgs []string
	remainArgs = append(remainArgs, os.Args[0]) // 保留程序名

	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if !strings.HasPrefix(arg, "-") && targetPath == "" {
			if i > 1 && (os.Args[i-1] == "--title" || os.Args[i-1] == "-title" || os.Args[i-1] == "-t" || os.Args[i-1] == "--test") {
				remainArgs = append(remainArgs, arg)
			} else {
				targetPath = arg
			}
		} else {
			remainArgs = append(remainArgs, arg)
		}
	}

	// 将提取路径后的参数重新交给 flag 库解析
	os.Args = remainArgs
	flag.StringVar(&titleFlag, "title", "", "Specify a global caption title for the media group")
	flag.StringVar(&testFlag, "t", "", "Test mode: use '-t=curl' to print curl command without uploading")
	flag.StringVar(&testFlag, "test", "", "Test mode: use '--test=curl' to print curl command without uploading")
	flag.Parse()

	// 3. 检查路径
	if targetPath == "" {
		fmt.Println("Error: Please specify a file or directory path.")
		fmt.Println("Usage: ./tgup [-t=curl] [--title=\"your title\"] <file_or_directory_path>")
		os.Exit(1)
	}

	// 4. 初始化环境与加载配置
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

	// 5. 解析目标路径获取文件列表
	files, err := collectFiles(targetPath)
	if err != nil {
		fmt.Printf("Error accessing path: %v\n", err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Println("No valid files found to upload.")
		return
	}

	if testFlag != "curl" {
		fmt.Printf("Found %d file(s) to upload.\n", len(files))
	}

	// 6. 决定全局标题
	globalCaption := strings.TrimSpace(titleFlag)
	if globalCaption == "" && testFlag != "curl" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Do you want to add a caption for this upload? (Press Enter to skip): ")
		input, _ := reader.ReadString('\n')
		globalCaption = strings.TrimSpace(input)
	}

	// 7. 初始化 Telegram Bot (非测试模式下严格验证)
	var bot *tgbotapi.BotAPI
	if testFlag != "curl" {
		bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(config.BotAPI, config.TgAPIURL+"/bot%s/%s")
		if err != nil {
			fmt.Printf("Error initializing bot: %v\n", err)
			os.Exit(1)
		}
		bot.Debug = false
	}

	// 8. 分批处理（每批最多10个文件）
	const batchSize = 10
	for i := 0; i < len(files); i += batchSize {
		end := i + batchSize
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]

		if testFlag == "curl" {
			// 测试模式：生成并打印带元数据与动态大图检测的 curl
			generateCurlCommand(config, batch, globalCaption, cacheDir)
		} else {
			// 正常模式：实际上传
			fmt.Printf("\n--- Preparing batch: %d to %d (Total: %d) ---\n", i+1, end, len(files))
			uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir)
		}
	}

	if testFlag != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
}

// checkAndResizePhoto 拦截检测图片像素：仅在超过2000万像素时进行高保真等比缩放到2000w以下
func checkAndResizePhoto(photoPath string, cacheDir string) (string, error) {
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

	if _, err := os.Stat(tmpPath); err == nil {
		return tmpPath, nil
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

// generateCurlCommand 支持视频元数据、流支持、超限原图检测与1s SHA1缩略图的 curl 组装
func generateCurlCommand(cfg *Config, files []string, globalCaption string, cacheDir string) {
	hasMedia := false
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if contains(cfg.PhotoExts, ext) || contains(cfg.VideoExts, ext) {
			hasMedia = true
			break
		}
	}

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

	var mediaJSONArray []TgMediaItem
	var fileFormFields []string
	isFirstItem := true

	photoIdx := 1
	videoIdx := 1
	docIdx := 1

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)

		if hasMedia && !isVideo && !isPhoto {
			continue
		}

		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		var itemType string
		var attachKey string

		if isVideo {
			itemType = "video"
			attachKey = fmt.Sprintf("video%d", videoIdx)

			width, height, duration, _ := getVideoMeta(file)
			thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
			var thumbValue string

			thumbPath, thumbErr := generateThumbnail(file, cacheDir)
			if thumbErr == nil && thumbPath != "" {
				absThumbPath, _ := filepath.Abs(thumbPath)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", thumbFormKey, absThumbPath))
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

			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir)
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

	fmt.Println("\n--- Generated Test CURL Command (With 20M Image Optimizer) ---")
	fmt.Printf("curl -X POST \"%s\" \\\n", apiURL)
	fmt.Printf("     -F \"chat_id=%d\" \\\n", cfg.ChatID)
	escapedMediaJSON := strings.ReplaceAll(mediaJSONStr, `"`, `\"`)
	fmt.Printf("     -F \"media=%s\" \\\n", escapedMediaJSON)
	fmt.Println(strings.Join(fileFormFields, " \\\n"))
	fmt.Println("----------------------------------------------------------------------")
}

// writeLog 封装通用日志追加写入方法
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

// loadConfig 读取并解析配置文件
func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &Config{
		PhotoExts: []string{".jpg", ".jpeg", ".png"},
		VideoExts: []string{".mp4", ".mkv", ".avi", ".mov"},
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
		key := strings.TrimSpace(parts[0])
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
			for i, e := range exts {
				exts[i] = strings.TrimSpace(e)
			}
			if len(exts) > 0 && exts[0] != "" {
				cfg.PhotoExts = exts
			}
		case "VIDEO_EXTS":
			exts := strings.Split(strings.ToLower(val), ",")
			for i, e := range exts {
				exts[i] = strings.TrimSpace(e)
			}
			if len(exts) > 0 && exts[0] != "" {
				cfg.VideoExts = exts
			}
		}
	}
	return cfg, scanner.Err()
}

// collectFiles 递归获取目录下的文件（自动过滤隐藏文件）
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
	return files, err
}

// contains 判断切片中是否包含某个字符串
func contains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

// uploadMediaGroup 处理单批次文件的类型判断、批量发送与日志存储
func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string) {
	hasMedia := false
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if contains(cfg.PhotoExts, ext) || contains(cfg.VideoExts, ext) {
			hasMedia = true
			break
		}
	}

	var mediaFiles []interface{}
	var processedFiles []string
	isFirstItem := true

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)

		if hasMedia && !isVideo && !isPhoto {
			fmt.Printf("[Skipped] %s (Cannot mix general Document with Photo/Video in an album)\n", filepath.Base(file))
			continue
		}

		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		if isVideo {
			reqFile := getProgressRequestFile(file)
			processedFiles = append(processedFiles, file)

			videoMedia := tgbotapi.NewInputMediaVideo(reqFile)
			videoMedia.Caption = currentCaption
			videoMedia.SupportsStreaming = true

			width, height, duration, err := getVideoMeta(file)
			if err == nil {
				videoMedia.Width = width
				videoMedia.Height = height
				videoMedia.Duration = duration

				thumbPath, thumbErr := generateThumbnail(file, cacheDir)
				if thumbErr == nil {
					videoMedia.Thumb = tgbotapi.FilePath(thumbPath)
				}
			}
			mediaFiles = append(mediaFiles, videoMedia)

		} else if isPhoto {
			optimizedPath, _ := checkAndResizePhoto(file, cacheDir)

			reqFile := getProgressRequestFile(optimizedPath)
			processedFiles = append(processedFiles, file)

			photoMedia := tgbotapi.NewInputMediaPhoto(reqFile)
			photoMedia.Caption = currentCaption

			mediaFiles = append(mediaFiles, photoMedia)

		} else {
			reqFile := getProgressRequestFile(file)
			processedFiles = append(processedFiles, file)

			docMedia := tgbotapi.NewInputMediaDocument(reqFile)
			docMedia.Caption = currentCaption

			mediaFiles = append(mediaFiles, docMedia)
		}
	}

	if len(mediaFiles) == 0 {
		return
	}

	respMessages, err := bot.SendMediaGroup(tgbotapi.NewMediaGroup(chatID, mediaFiles))

	currentTimeStr := time.Now().Format("2006-01-02 15:04:05")
	fileListStr := strings.Join(processedFiles, ", ")
	respJSON, _ := json.Marshal(respMessages)

	if err != nil {
		fmt.Printf("\nFailed to send media group: %v\n", err)

		if strings.Contains(err.Error(), "PHOTO_INVALID_DIMENSIONS") {
			fmt.Println("⚠️ Detected PHOTO_INVALID_DIMENSIONS. Retrying upload by degrading all items to Safe Document Mode...")
			retryMediaFiles := make([]interface{}, len(processedFiles))
			isFirstRetryItem := true
			for idx, file := range processedFiles {
				reqFile := getProgressRequestFile(file)
				docMedia := tgbotapi.NewInputMediaDocument(reqFile)
				if isFirstRetryItem && globalCaption != "" {
					docMedia.Caption = globalCaption
					isFirstRetryItem = false
				}
				retryMediaFiles[idx] = docMedia
			}

			retryResp, retryErr := bot.SendMediaGroup(tgbotapi.NewMediaGroup(chatID, retryMediaFiles))
			retryJSON, _ := json.Marshal(retryResp)

			if retryErr != nil {
				fmt.Printf("Fallback failed: %v\n", retryErr)
				errLogContent := fmt.Sprintf("[%s] ERROR: %v | Fallback JSON: %s | Files: [%s]\n", currentTimeStr, retryErr, string(retryJSON), fileListStr)
				writeLog(logDir, "error.log", errLogContent)
				return
			} else {
				fmt.Println("Fallback upload success via Document Mode.")
				okLogContent := fmt.Sprintf("[%s] SUCCESS (FALLBACK): MsgIDs: %s | Files: [%s]\n", currentTimeStr, string(retryJSON), fileListStr)
				writeLog(logDir, "ok.log", okLogContent)
				return
			}
		}

		errLogContent := fmt.Sprintf("[%s] ERROR: %v | Response JSON: %s | Files attempted: [%s]\n", currentTimeStr, err, string(respJSON), fileListStr)
		writeLog(logDir, "error.log", errLogContent)
	} else {
		fmt.Println("\nBatch upload success.")
		okLogContent := fmt.Sprintf("[%s] SUCCESS: Response JSON: %s | Files uploaded: [%s]\n", currentTimeStr, string(respJSON), fileListStr)
		writeLog(logDir, "ok.log", okLogContent)
	}
}

// getProgressRequestFile 拦截文件读取并渲染终端进度条
func getProgressRequestFile(path string) tgbotapi.RequestFileData {
	file, err := os.Open(path)
	if err != nil {
		return tgbotapi.FilePath(path)
	}

	fi, err := file.Stat()
	if err != nil {
		return tgbotapi.FilePath(path)
	}

	bar := progressbar.NewOptions64(
		fi.Size(),
		progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", filepath.Base(path))),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(15),
		progressbar.OptionThrottle(65),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
	)

	proxyReader := progressbar.NewReader(file, bar)

	return tgbotapi.FileReader{
		Name:   filepath.Base(path),
		Reader: &proxyReader,
	}
}

// getVideoMeta 使用 ffprobe 获取视频宽高与时长
func getVideoMeta(videoPath string) (width int, height int, duration int, err error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,duration", "-of", "default=noprint_wrappers=1", videoPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0, err
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
			width, _ = strconv.Atoi(val)
		case "height":
			height, _ = strconv.Atoi(val)
		case "duration":
			if idx := strings.Index(val, "."); idx != -1 {
				val = val[:idx]
			}
			duration, _ = strconv.Atoi(val)
		}
	}
	return width, height, duration, nil
}

// generateThumbnail 使用 ffmpeg 提取视频缩略图
func generateThumbnail(videoPath string, cacheDir string) (string, error) {
	tempThumb := filepath.Join(cacheDir, "temp_proto_thumb.jpg")
	_ = os.Remove(tempThumb)

	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", tempThumb)
	if err := cmd.Run(); err != nil {
		return "", err
	}

	data, err := os.ReadFile(tempThumb)
	if err != nil {
		return "", err
	}

	hasher := sha1.New()
	hasher.Write(data)
	sha1Name := hex.EncodeToString(hasher.Sum(nil)) + ".jpg"
	finalThumbPath := filepath.Join(cacheDir, sha1Name)

	if _, err := os.Stat(finalThumbPath); err == nil {
		_ = os.Remove(tempThumb)
		return finalThumbPath, nil
	}

	err = os.Rename(tempThumb, finalThumbPath)
	if err != nil {
		return tempThumb, nil
	}

	return finalThumbPath, nil
}
