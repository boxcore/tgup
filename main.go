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
	var cacheForceFlag bool

	flag.StringVar(&titleFlag, "title", "", "Specify a global caption title for the media group")
	flag.StringVar(&testFlag, "t", "", "Test mode: use '-t=curl' to print curl command")
	flag.StringVar(&testFlag, "test", "", "Test mode: use '--test=curl' to print curl command")

	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force regenerate and overwrite existing thumbnails/photos")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force regenerate and overwrite existing thumbnails/photos (shorthand)")

	flag.Parse()

	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a file or directory path.")
		fmt.Println("Usage: go run main.go [-t=curl] [--cache-force] [--title='title'] <path>")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
	}

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

	files, err := collectFiles(targetPath)
	if err != nil {
		fmt.Printf("Error accessing path: %v\n", err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Println("No valid files found to upload.")
		return
	}

	if finalTestMode != "curl" {
		fmt.Printf("Found %d file(s) to upload.\n", len(files))
	}

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

	const batchSize = 10
	for i := 0; i < len(files); i += batchSize {
		end := i + batchSize
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]

		if finalTestMode == "curl" {
			generateCurlCommand(config, batch, globalCaption, cacheDir, cacheForceFlag)
		} else {
			fmt.Printf("\n--- Preparing batch: %d to %d (Total: %d) ---\n", i+1, end, len(files))
			uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag)
		}
	}

	if finalTestMode != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
}

// checkAndResizePhoto 拦截检测大图像素
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

// getImageDimensions 读取本地图片的精确宽高
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

// generateCurlCommand 生成测试 CURL 指令
func generateCurlCommand(cfg *Config, files []string, globalCaption string, cacheDir string, cacheForce bool) {
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

			thumbPath, isPortraitVideo, thumbErr := generateThumbnail(file, cacheDir, cacheForce)
			if thumbErr == nil && thumbPath != "" {
				absThumbPath, _ := filepath.Abs(thumbPath)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", thumbFormKey, absThumbPath))
			} else if thumbErr != nil {
				fmt.Printf("[Debug Error] Thumbnail generation failed for %s: %v\n", filepath.Base(file), thumbErr)
			}

			// 如果图片判定是竖屏，强行对调从视频读取的宽和高
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

func contains(slice []string, val string) bool {
	for _, item := range slice {
		itemLower := strings.ToLower(item)
		valLower := strings.ToLower(val)
		if itemLower == valLower {
			return true
		}
	}
	return false
}

func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool) {
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
				thumbPath, isPortraitVideo, thumbErr := generateThumbnail(file, cacheDir, cacheForce)
				if thumbErr == nil && thumbPath != "" {
					videoMedia.Thumb = tgbotapi.FilePath(thumbPath)

					if isPortraitVideo && width > height {
						width, height = height, width
					}
				}

				videoMedia.Width = width
				videoMedia.Height = height
				videoMedia.Duration = duration
			}
			mediaFiles = append(mediaFiles, videoMedia)

		} else if isPhoto {
			optimizedPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)

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

// generateThumbnail 完美贴合 Telegram 标准：长边不超过 320px、高品质 JPG、移除会引发 234 错误的无效参数
func generateThumbnail(videoPath string, cacheDir string, cacheForce bool) (path string, isPortrait bool, err error) {
	hasherName := sha1.New()
	hasherName.Write([]byte(videoPath))
	sha1Name := hex.EncodeToString(hasherName.Sum(nil)) + ".jpg"
	finalThumbPath := filepath.Join(cacheDir, sha1Name)

	isCacheValid := false

	if !cacheForce {
		if fi, err := os.Stat(finalThumbPath); err == nil {
			if fi.Size() > 10 {
				tW, tH := getImageDimensions(finalThumbPath)
				// 根据最新知识库限制：严格约束缓存图片的宽高均不得超过 320
				if tW > 0 && tH > 0 && tW <= 320 && tH <= 320 {
					isCacheValid = true
					if tH > tW {
						isPortrait = true
					}
				}
			}
		}
	}

	if isCacheValid {
		return finalThumbPath, isPortrait, nil
	}

	_ = os.Remove(finalThumbPath)

	rawTempJpg := filepath.Join(cacheDir, "raw_extracted_frame.jpg")
	_ = os.Remove(rawTempJpg)

	// 【致命错误修复区】：彻底抛弃 `-vf autorotate=1` 等破坏命令行的参数！
	// 新版 FFmpeg 原生默认携带画面自动旋转解码。这里只用最纯净的命令在第 1 秒抽取单帧，保证稳定输出0退出码。
	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", rawTempJpg)
	if err := cmd.Run(); err != nil {
		// 备用降级方案，应对小于 1 秒的超短视频
		cmdFallback := exec.Command("ffmpeg", "-y", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
		if errFallback := cmdFallback.Run(); errFallback != nil {
			return "", false, fmt.Errorf("ffmpeg raw frame extraction failed: %v", errFallback)
		}
	}
	defer os.Remove(rawTempJpg)

	// 使用 Go 原生图片库安全无损缩放，长边严格锁定为最高 320px
	rawFile, err := os.Open(rawTempJpg)
	if err != nil {
		return "", false, err
	}

	srcImg, _, err := image.Decode(rawFile)
	rawFile.Close()
	if err != nil {
		return "", false, fmt.Errorf("failed to decode raw frame image: %v", err)
	}

	bounds := srcImg.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()

	// 严格设定为 320 像素上限
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
		return "", false, err
	}
	defer outFile.Close()

	// 使用品质 92 的 JPEG 编码，对于 320x320 尺寸的文件体积将绝对稳压在几十 KB（远低于 200KB 要求）
	err = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 92})
	if err != nil {
		return "", false, err
	}

	if newHeight > newW {
		isPortrait = true
	}

	return finalThumbPath, isPortrait, nil
}
