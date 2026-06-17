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

	// 规范化布尔开关：--cache-force 或快捷缩写 -cf
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force regenerate and overwrite existing thumbnails/photos")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force regenerate and overwrite existing thumbnails/photos (shorthand)")

	// 2. 官方标准解析
	flag.Parse()

	// 3. 获取剩余的位置参数（路径）
	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a file or directory path.")
		fmt.Println("Usage: go run main.go [-t=curl] [--cache-force] [--title='title'] <path>")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	// 4. 统一别名状态
	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
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

	// 6. 解析目标路径获取文件列表
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

	// 7. 决定全局标题
	globalCaption := strings.TrimSpace(titleFlag)
	if globalCaption == "" && finalTestMode != "curl" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Do you want to add a caption for this upload? (Press Enter to skip): ")
		input, _ := reader.ReadString('\n')
		globalCaption = strings.TrimSpace(input)
	}

	// 8. 初始化 Telegram Bot (保留声明以向下兼容日志模块，实际发包使用原生 HTTP)
	var bot *tgbotapi.BotAPI
	if finalTestMode != "curl" {
		bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(config.BotAPI, config.TgAPIURL+"/bot%s/%s")
		if err != nil {
			fmt.Printf("Error initializing bot: %v\n", err)
			os.Exit(1)
		}
		bot.Debug = false
	}

	// 9. 分批处理
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

	photoIdx, videoIdx, docIdx := 1, 1, 1

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

		var itemType, attachKey string

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

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// 兜底默认值
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

// uploadMediaGroup 实际上传功能：改用原生发包，解决旧版 Go 库丢失 Thumb 的缺陷
func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool) {
	hasMedia := false
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if contains(cfg.PhotoExts, ext) || contains(cfg.VideoExts, ext) {
			hasMedia = true
			break
		}
	}

	var validFiles []string
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)
		if hasMedia && !isVideo && !isPhoto {
			fmt.Printf("[Skipped] %s (Cannot mix general Document with Photo/Video in an album)\n", filepath.Base(file))
			continue
		}
		validFiles = append(validFiles, file)
	}

	if len(validFiles) == 0 {
		return
	}

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

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

	_ = writer.WriteField("chat_id", strconv.FormatInt(chatID, 10))

	isFirstItem := true
	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range validFiles {
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
			width, height, duration, _ := getVideoMeta(file)
			var thumbValue string

			thumbPath, isPortraitVideo, thumbErr := generateThumbnail(file, cacheDir, cacheForce)
			if thumbErr == nil && thumbPath != "" {
				thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				addFileToMultipart(writer, thumbFormKey, thumbPath, false)
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
			addFileToMultipart(writer, attachKey, file, true)

		} else if isPhoto {
			itemType = "photo"
			attachKey = fmt.Sprintf("photo%d", photoIdx)
			photoIdx++

			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    itemType,
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			addFileToMultipart(writer, attachKey, finalPhotoPath, true)
		} else {
			itemType = "document"
			attachKey = fmt.Sprintf("doc%d", docIdx)
			docIdx++

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    itemType,
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			addFileToMultipart(writer, attachKey, file, true)
		}
	}

	mediaJSONBytes, _ := json.Marshal(mediaJSONArray)
	_ = writer.WriteField("media", string(mediaJSONBytes))
	_ = writer.Close()

	apiURL := fmt.Sprintf("%s/bot%s/sendMediaGroup", cfg.TgAPIURL, cfg.BotAPI)
	req, err := http.NewRequest("POST", apiURL, &requestBody)
	if err != nil {
		fmt.Printf("Failed to create request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)

	currentTimeStr := time.Now().Format("2006-01-02 15:04:05")
	fileListStr := strings.Join(validFiles, ", ")

	if err != nil {
		errLogContent := fmt.Sprintf("[%s] ERROR: %v | Files attempted: [%s]\n", currentTimeStr, err, fileListStr)
		writeLog(logDir, "error.log", errLogContent)
		fmt.Printf("Upload request failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		fmt.Println("Batch upload success.")
		okLogContent := fmt.Sprintf("[%s] SUCCESS: Status: %d | Files uploaded: [%s]\n", currentTimeStr, resp.StatusCode, fileListStr)
		writeLog(logDir, "ok.log", okLogContent)
	} else {
		fmt.Printf("\nTelegram API Error: Status %d, Body: %s\n", resp.StatusCode, string(respBody))
		errLogContent := fmt.Sprintf("[%s] ERROR: Status %d | Body: %s | Files attempted: [%s]\n", currentTimeStr, resp.StatusCode, string(respBody), fileListStr)
		writeLog(logDir, "error.log", errLogContent)
	}
}

func addFileToMultipart(writer *multipart.Writer, fieldName string, filePath string, showProgress bool) {
	part, err := writer.CreateFormFile(fieldName, filepath.Base(filePath))
	if err != nil {
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer file.Close()

	if showProgress {
		fi, _ := file.Stat()
		bar := progressbar.NewOptions64(
			fi.Size(),
			progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", filepath.Base(filePath))),
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
		_, _ = io.Copy(part, &proxyReader)
	} else {
		_, _ = io.Copy(part, file)
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

	// 只截取第 1 秒的原帧大图，没有任何冗余滤镜干扰
	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", rawTempJpg)
	if err := cmd.Run(); err != nil {
		cmdFallback := exec.Command("ffmpeg", "-y", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
		if errFallback := cmdFallback.Run(); errFallback != nil {
			return "", false, errFallback
		}
	}
	defer os.Remove(rawTempJpg)

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

	// 严格执行 320px 知识库限制
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

	err = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 92})
	if err != nil {
		return "", false, err
	}

	if newHeight > newW {
		isPortrait = true
	}

	return finalThumbPath, isPortrait, nil
}
